// Package eventlog writes the console's structured events for the unified
// log: one JSON object per line, ops and audit in separate files, rotated by
// renaming so the shipper (Logtail) never loses a line to truncation.
//
// It deliberately does not talk to SLS. The file is the durable queue; the
// shipper owns retries, checkpoints and rotation tracking. When no directory
// is configured events go to stderr and nothing is kept, which is what a
// developer running `admin` locally wants.
package eventlog

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const SchemaVersion = 1

// Writer emits console events. Safe for concurrent use.
type Writer struct {
	source string
	ops    *rotatingFile
	audit  *rotatingFile
	stderr io.Writer
}

// New opens <dir>/admin.jsonl and <dir>/audit.jsonl. An empty dir means
// stderr only.
func New(dir, source string) (*Writer, error) {
	w := &Writer{source: source, stderr: os.Stderr}
	if dir == "" {
		return w, nil
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("eventlog: %w", err)
	}
	var err error
	if w.ops, err = openRotating(filepath.Join(dir, "admin.jsonl")); err != nil {
		return nil, err
	}
	if w.audit, err = openRotating(filepath.Join(dir, "audit.jsonl")); err != nil {
		return nil, err
	}
	return w, nil
}

// Ops records a runtime event. level is debug|info|warn|error.
func (w *Writer) Ops(level, eventType, msg string, fields map[string]any) {
	if w == nil {
		return
	}
	w.emit(w.ops, level, eventType, msg, fields, "", time.Time{})
}

// Audit records an administrative fact. Always level info.
func (w *Writer) Audit(eventType, msg string, fields map[string]any) {
	if w == nil {
		return
	}
	w.emit(w.audit, "info", eventType, msg, fields, "", time.Time{})
}

// AuditRecorded writes an audit event whose identity was fixed elsewhere.
//
// The database row is the event; the copy shipped to the archive has to
// carry the same event_id and occurred_at, or the two can never be
// reconciled line by line -- the archive is asked "is event X there?", and
// a copy under a fresh id answers no forever. The common-field rule still
// applies to every other key.
func (w *Writer) AuditRecorded(eventID string, occurredAt time.Time, eventType, msg string, fields map[string]any) {
	if w == nil {
		return
	}
	w.emit(w.audit, "info", eventType, msg, fields, eventID, occurredAt)
}

func (w *Writer) emit(dst *rotatingFile, level, eventType, msg string, fields map[string]any, eventID string, occurredAt time.Time) {
	if eventID == "" {
		eventID = newID()
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now()
	}
	ev := map[string]any{
		"schema_version": SchemaVersion,
		"event_id":       eventID,
		"event_type":     eventType,
		"occurred_at":    occurredAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		"module":         "console",
		"source_id":      w.source,
		"level":          level,
		"message":        secretValues.ReplaceAllString(msg, "[redacted]"),
	}
	for k, v := range Redact(fields) {
		if _, taken := ev[k]; taken {
			// A caller field named like a common field cannot overwrite it:
			// the shape of every event has to stay the same or the queries
			// and alerts built on it stop meaning one thing. Say which key
			// was dropped, or the field silently goes missing and whoever
			// added it hunts for it in SLS instead of here.
			fmt.Fprintf(w.stderr, "eventlog: %s: field %q collides with a common field and was dropped\n", eventType, k)
			continue
		}
		ev[k] = v
	}
	line, err := json.Marshal(ev)
	if err != nil {
		fmt.Fprintf(w.stderr, "eventlog: encode: %v\n", err)
		return
	}
	// Backstop. Redact works on the values it is given, but a custom
	// MarshalJSON, or a type nobody anticipated, can still put a secret into
	// the encoded line. This is the last place the bytes can be inspected
	// before they become durable, so inspect them.
	line = secretValues.ReplaceAll(line, []byte("[redacted]"))
	line = append(line, '\n')
	if dst == nil {
		w.stderr.Write(line)
		return
	}
	if err := dst.write(line); err != nil {
		fmt.Fprintf(w.stderr, "eventlog: write %s: %v\n", dst.path, err)
	}
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// rotatingFile appends lines and, past maxBytes, renames the file to .1
// (shifting older backups up) and reopens. Whole lines only: a line is
// never split across the rotation.
type rotatingFile struct {
	mu       sync.Mutex
	path     string
	f        *os.File
	size     int64
	maxBytes int64
	backups  int
}

func openRotating(path string) (*rotatingFile, error) {
	r := &rotatingFile{path: path, maxBytes: 50 << 20, backups: 5}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *rotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("eventlog: open %s: %w", r.path, err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotatingFile) write(line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size > 0 && r.size+int64(len(line)) > r.maxBytes {
		if err := r.rotate(); err != nil {
			return err
		}
	}
	n, err := r.f.Write(line)
	r.size += int64(n)
	return err
}

// rotate closes the live file, shifts .1..N-1 up one, renames the live file
// to .1 and opens a fresh one.
//
// It is called with the lock held, from write. If a rename or the reopen
// fails, the file stays closed and the error reaches the caller, which logs
// it to stderr: the lines written in between are visible there but are not
// kept and not shipped. The next write finds the size still over the limit
// and tries the whole rotation again, so a transient failure (a full disk
// that is then cleared, a directory briefly replaced) heals itself without
// anyone restarting the console.
func (r *rotatingFile) rotate() error {
	r.f.Close()
	for i := r.backups - 1; i >= 1; i-- {
		os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
	}
	if err := os.Rename(r.path, r.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return r.open()
}
