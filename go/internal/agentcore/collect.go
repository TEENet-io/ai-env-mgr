package agentcore

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/TEENet-io/airlock/internal/ossclient"
)

// Putter is the write-only slice of the store the collector uses. It is
// deliberately narrower than Store: the agent has no read access to
// data_collect, so a leaked machine key cannot pull back anyone's
// conversations. Encoding that as a type keeps the guarantee from eroding.
type Putter interface {
	Put(key string, data []byte) error
}

// SessionFile is one raw AI session file found under an employee profile.
type SessionFile struct {
	Path    string    // absolute source path on this machine
	Rel     string    // path relative to the profile, posix (".claude/projects/...")
	ModTime time.Time // last modification time
	Size    int64     // size in bytes
}

// FileSource enumerates and reads session files. It is an interface so the
// collector can be tested with in-memory files on any platform.
type FileSource interface {
	Sessions(profileDir string) ([]SessionFile, error)
	Open(path string) (io.ReadCloser, error)
}

// CollectResult reports one collection pass.
type CollectResult struct {
	Uploaded int
	Errors   []string
}

// CollectRunner is what Syncer needs from a collector, so the sync test can
// substitute a fake without building a real Collector.
type CollectRunner interface {
	CollectOnce(user string, quietSeconds int, since string) CollectResult
}

const collectStateFile = "collect-state.json"

// defaultQuietSeconds mirrors collect_to_oss.py's --quiet default: only upload
// a file that has been idle this long, so a session still being written is not
// re-uploaded half-formed on every pass.
const defaultQuietSeconds = 60

// Collector uploads changed session files to an employee's data_collect
// directory. Deduplication is local-only (see Putter): it keeps a state file
// of (mtime,size) per source path and uploads a file only when that changes.
type Collector struct {
	Store    Putter
	Source   FileSource
	Machine  Machine
	StateDir string
	Now      func() time.Time
}

// sig is the change signature stored per source file: modification time (unix
// nanoseconds) and size. An append always changes size, so the pair is enough.
type sig [2]int64

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) statePath() string { return filepath.Join(c.StateDir, collectStateFile) }

func (c *Collector) loadState() map[string]sig {
	state := map[string]sig{}
	data, err := os.ReadFile(c.statePath())
	if err != nil {
		return state
	}
	_ = json.Unmarshal(data, &state) // a corrupt state file just means "re-upload"
	return state
}

func (c *Collector) saveState(state map[string]sig) {
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return
	}
	data, err := json.Marshal(state)
	if err != nil {
		return
	}
	tmp := c.statePath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.statePath())
}

// CollectOnce runs one pass for a single employee. Mirrors run_once() in
// collect_to_oss.py: skip unchanged, debounce still-being-written files, skip
// history before `since`, retry failed uploads next pass, and drop deleted
// sources from state while leaving their uploaded copies in OSS.
func (c *Collector) CollectOnce(user string, quietSeconds int, since string) CollectResult {
	if quietSeconds <= 0 {
		quietSeconds = defaultQuietSeconds
	}
	var res CollectResult
	profileDir := c.Machine.ProfileDir(user)
	files, err := c.Source.Sessions(profileDir)
	if err != nil {
		res.Errors = append(res.Errors, fmt.Sprintf("collect: list sessions: %v", err))
		return res
	}

	state := c.loadState()
	now := c.now()
	seen := map[string]bool{}

	for _, f := range files {
		seen[f.Path] = true
		cur := sig{f.ModTime.UnixNano(), f.Size}

		// since: skip files whose UTC date is before the cutoff (as in the script).
		if since != "" && f.ModTime.UTC().Format("2006-01-02") < since {
			continue
		}
		// unchanged since last upload.
		if old, ok := state[f.Path]; ok && old == cur {
			continue
		}
		// debounce: modified within the quiet window -> still being written.
		if now.Sub(f.ModTime) < time.Duration(quietSeconds)*time.Second {
			continue
		}

		rc, err := c.Source.Open(f.Path)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("collect: open %s: %v", f.Rel, err))
			continue // no state write -> retried next pass
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("collect: read %s: %v", f.Rel, err))
			continue
		}
		if err := c.Store.Put(ossclient.DataCollectKey(user, f.Rel), data); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("collect: upload %s: %v", f.Rel, err))
			continue // failure -> not recorded, retried next pass
		}
		state[f.Path] = cur
		res.Uploaded++
	}

	// Source deleted: forget it locally. The uploaded object stays in OSS,
	// subject to the bucket's lifecycle/retention policy.
	for p := range state {
		if !seen[p] {
			delete(state, p)
		}
	}
	c.saveState(state)
	return res
}
