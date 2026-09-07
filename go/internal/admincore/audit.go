package admincore

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
)

// AuditAction names one of the five account operations.
type AuditAction string

const (
	AuditOnboard  AuditAction = "onboard"
	AuditOffboard AuditAction = "offboard"
	AuditQuota    AuditAction = "quota"
	AuditModels   AuditAction = "models"
	AuditReissue  AuditAction = "reissue"
)

// AuditEntry is one line of an employee's history.
//
// There is no operator field: the console signs in with an OSS key, not a
// personal identity, so "who" is not knowable here. Recording a guess would
// be worse than recording nothing.
type AuditEntry struct {
	At     string         `json:"at"`
	Action AuditAction    `json:"action"`
	User   string         `json:"user"`
	Detail map[string]any `json:"detail,omitempty"`
}

// AuditKey is one employee's history: admin/audit/<user>.jsonl.
func AuditKey(windowsUser string) string {
	return ossclient.AdminKey("audit/" + strings.ToLower(windowsUser) + ".jsonl")
}

// auditReadLimit bounds what the detail page shows. Newest first, so the
// cut falls on the oldest entries.
const auditReadLimit = 200

// appendAudit records one action. It never fails the caller: the object
// store has no append, so this is read-modify-write, and losing one audit
// line is a better outcome than rolling back an onboarding that succeeded.
func (m *Manager) appendAudit(windowsUser string, action AuditAction, detail map[string]any) {
	entry := AuditEntry{
		At:     time.Now().UTC().Format(time.RFC3339),
		Action: action,
		User:   windowsUser,
		Detail: detail,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		log.Printf("admincore: audit %s for %q: encode: %v", action, windowsUser, err)
		return
	}
	key := AuditKey(windowsUser)
	existing, _, _ := m.Store.Get(key) // missing is fine: first entry
	var buf bytes.Buffer
	buf.Write(existing)
	if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.Write(line)
	buf.WriteByte('\n')
	if err := m.Store.Put(key, buf.Bytes()); err != nil {
		log.Printf("admincore: audit %s for %q: write: %v", action, windowsUser, err)
	}
}

// ReadAudit returns an employee's history, newest first, at most
// auditReadLimit entries. No file means no history, not an error.
func (m *Manager) ReadAudit(windowsUser string) ([]AuditEntry, error) {
	data, _, err := m.Store.Get(AuditKey(windowsUser))
	if err != nil {
		return nil, nil
	}
	var all []AuditEntry
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			// One damaged line should not hide the rest of the history.
			log.Printf("admincore: audit for %q: skip unreadable line: %v", windowsUser, err)
			continue
		}
		all = append(all, e)
	}
	// Reverse in place, then cut.
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if len(all) > auditReadLimit {
		all = all[:auditReadLimit]
	}
	return all, nil
}
