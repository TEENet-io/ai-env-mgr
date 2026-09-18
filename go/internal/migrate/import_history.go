package migrate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/TEENet-io/ai-env-mgr/internal/ossclient"
	"github.com/TEENet-io/ai-env-mgr/internal/repo"
)

// legacyAuditEntry is one line of admin/audit/<user>.jsonl as the old console
// wrote it. Mirrored here rather than imported from admincore so the import
// does not depend on the code it is replacing.
type legacyAuditEntry struct {
	At     string         `json:"at"`
	Action string         `json:"action"`
	User   string         `json:"user"`
	Detail map[string]any `json:"detail,omitempty"`
}

const legacyActor = "legacy-console"

// importHistory brings each employee's audit file into audit_events.
//
// The files are append-only and the import counts how many lines it already
// holds, so a second run continues where the first stopped instead of
// duplicating the trail. The request id "legacy:<user>:<line>" is what makes
// that count possible, and it is also how an imported line can be traced back
// to the file it came from.
func (im *Importer) importHistory(ctx context.Context, tx repo.Store, byUser map[string]repo.Employee, report *Report) error {
	keys, err := im.Objects.List(ossclient.AdminKey("audit/"))
	if err != nil {
		return fmt.Errorf("list audit history: %w", err)
	}
	sort.Strings(keys)

	for _, key := range keys {
		if !strings.HasSuffix(key, ".jsonl") {
			continue
		}
		windowsUser := strings.TrimSuffix(strings.TrimPrefix(key, ossclient.AdminKey("audit/")), ".jsonl")
		employee, ok := byUser[repo.NormalizeWindowsUser(windowsUser)]
		if !ok {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"audit history exists for %q, who is not on the roster; it was not imported", windowsUser))
			continue
		}
		data, _, err := im.Objects.Get(key)
		if errors.Is(err, ossclient.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}

		prefix := "legacy:" + employee.WindowsUser + ":"
		already, err := tx.Audit().CountByRequestPrefix(ctx, prefix)
		if err != nil {
			return err
		}

		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			raw := bytes.TrimSpace(scanner.Bytes())
			if len(raw) == 0 {
				continue
			}
			line++
			if line <= already {
				continue
			}
			var entry legacyAuditEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf(
					"%s line %d is not readable and was skipped", key, line))
				continue
			}
			occurred, _ := time.Parse(time.RFC3339, entry.At)
			detail, _ := json.Marshal(entry.Detail)
			if _, err := tx.Audit().Append(ctx, repo.AuditEvent{
				OccurredAt: occurred,
				ActorType:  repo.ActorSystem,
				ActorID:    legacyActor,
				Action:     entry.Action,
				TargetType: "employee",
				TargetID:   employee.ID,
				Detail:     detail,
				RequestID:  fmt.Sprintf("%s%d", prefix, line),
			}); err != nil {
				return fmt.Errorf("import %s line %d: %w", key, line, err)
			}
			report.AuditLines++
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read %s: %w", key, err)
		}
	}
	return nil
}

// importSettings brings in the console's global settings: today, only the
// quota new accounts are pre-filled with.
func (im *Importer) importSettings(ctx context.Context, tx repo.Store, report *Report) error {
	data, _, err := im.Objects.Get(ossclient.AdminKey("quota-defaults.json"))
	if errors.Is(err, ossclient.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read quota defaults: %w", err)
	}
	if !json.Valid(data) {
		report.Warnings = append(report.Warnings, "quota-defaults.json is not readable JSON and was not imported")
		return nil
	}
	current, err := tx.Settings().Get(ctx, repo.SettingQuotaDefaults)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		_, err = tx.Settings().Set(ctx, repo.SettingQuotaDefaults, data, 0, im.Actor)
		if err == nil {
			report.Settings++
		}
		return err
	case err != nil:
		return err
	}
	if sameJSON(current.Value, data) {
		return nil
	}
	_, err = tx.Settings().Set(ctx, repo.SettingQuotaDefaults, data, current.Version, im.Actor)
	return err
}
