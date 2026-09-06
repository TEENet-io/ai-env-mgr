package creds

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

// profileTargets maps zip-internal paths (model.PathXxx) to their location
// relative to the user's profile directory.
//
// This map is a whitelist, not just a convenience lookup: it is the only
// place that decides where bytes from an untrusted archive may land on disk.
// Any entry not listed here is rejected outright, so a crafted zip path such
// as "../../../etc/passwd" or "evil/../../whatever" never reaches
// filepath.Join with attacker-controlled segments — it is turned away before
// any path arithmetic happens.
var profileTargets = map[string][]string{
	model.PathCodexAuth:    {".codex", "auth.json"},
	model.PathCodexConfig:  {".codex", "config.toml"},
	model.PathCodexModels:  {".codex", "models.json"},
	model.PathClaudeCreds:  {".claude", ".credentials.json"},
	model.PathClaudeConfig: {".claude.json"},
}

// TargetPath resolves a zip-internal credential path to its absolute
// destination under profileDir. Only the entries in profileTargets are
// recognized; anything else is an error.
func TargetPath(profileDir, entry string) (string, error) {
	rel, ok := profileTargets[entry]
	if !ok {
		return "", fmt.Errorf("unknown credential entry %q", entry)
	}
	parts := append([]string{profileDir}, rel...)
	return filepath.Join(parts...), nil
}

// WriteToProfile writes each recognized credential in set to its location
// under profileDir, creating parent directories as needed. It returns how
// many files were written.
//
// Entries not present in profileTargets are skipped silently rather than
// erroring: the archive is produced by our own Pack function from a trusted
// admin roster, so an unrecognized entry is far more likely to be a stray or
// future-format file than an attack, and failing the whole deploy over one
// unknown entry would leave a user's known-good credentials undelivered.
func WriteToProfile(profileDir string, set model.CredentialSet) (int, error) {
	r, err := WriteToProfileReport(profileDir, set)
	return r.Written, err
}

// Report describes what one delivery actually did.
type Report struct {
	// Written counts the files placed on disk.
	Written int

	// Skipped names archive entries this build has no target for. A silent
	// skip is indistinguishable from success: an agent older than the console
	// delivering to it writes what it knows, reports a clean deploy, and
	// leaves the employee without the file the delivery existed for.
	Skipped []string

	// Placed maps each written file's path to the SHA-256 of the bytes that
	// actually reached disk.
	//
	// It hashes what was WRITTEN rather than what was delivered, which is the
	// only version that can be checked later: config.toml is merged into the
	// employee's own file, so its contents never equal the delivered bytes.
	Placed map[string]string
}

// WriteToProfileReport is WriteToProfile plus the entries it did not
// recognize.
//
// The skipped list exists because a silent skip is indistinguishable from
// success. An agent older than the console delivering to it has no entry for
// a newly introduced file, writes the ones it does know, and reports a clean
// deploy -- while the employee is missing the file that made the delivery
// worth doing. That failure is invisible from every angle unless the skips
// are carried back out.
func WriteToProfileReport(profileDir string, set model.CredentialSet) (Report, error) {
	rep := Report{Placed: map[string]string{}}
	written := 0
	var skipped []string
	for entry, data := range set {
		target, err := TargetPath(profileDir, entry)
		if err != nil {
			skipped = append(skipped, entry)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			rep.Written, rep.Skipped = written, skipped
			return rep, fmt.Errorf("create directory for %q: %w", entry, err)
		}

		payload := data
		switch entry {
		case model.PathClaudeConfig:
			merged, err := mergeClaudeConfig(target, data)
			if err != nil {
				rep.Written, rep.Skipped = written, skipped
				return rep, err
			}
			payload = merged
		case model.PathCodexConfig:
			merged, err := mergeCodexConfig(target, data)
			if err != nil {
				rep.Written, rep.Skipped = written, skipped
				return rep, err
			}
			payload = merged
		}

		if err := os.WriteFile(target, payload, 0o600); err != nil {
			rep.Written, rep.Skipped = written, skipped
			return rep, fmt.Errorf("write %q: %w", entry, err)
		}
		rep.Placed[target] = fmt.Sprintf("%x", sha256.Sum256(payload))
		written++
	}
	sort.Strings(skipped)
	rep.Written, rep.Skipped = written, skipped
	return rep, nil
}

// VerifyPlaced reports whether every file in placed is still on disk with the
// same contents.
//
// This is what makes a delivery checkable after the fact. The agent otherwise
// records only which archive it fetched, so a file that was never written --
// because an older build had no target for it -- or one the employee later
// deleted or edited looks identical to a clean delivery forever after.
func VerifyPlaced(placed map[string]string) (ok bool, drifted []string) {
	for path, want := range placed {
		data, err := os.ReadFile(path)
		if err != nil {
			drifted = append(drifted, path)
			continue
		}
		if fmt.Sprintf("%x", sha256.Sum256(data)) != want {
			drifted = append(drifted, path)
		}
	}
	sort.Strings(drifted)
	return len(drifted) == 0, drifted
}

// Present reports which credential files are already in place under
// profileDir. It is read-only, and backs `agent.exe status` answering
// "does this employee have their logins?" without redelivering anything.
func Present(profileDir string) []string {
	var found []string
	for entry := range profileTargets {
		path, err := TargetPath(profileDir, entry)
		if err != nil {
			continue
		}
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Size() > 0 {
			found = append(found, entry)
		}
	}
	sort.Strings(found)
	return found
}

// Remove deletes the AI credential files under profileDir and reports how
// many it removed.
//
// This is the local half of offboarding: once an employee is disabled, the
// logins the administrator delivered must not keep working on the machine.
// Only the whitelisted paths are touched -- the same map that governs what
// may be written -- so nothing else in the profile can be removed by this.
//
// A file that is already gone is not an error; the operation is idempotent
// because it runs on every sync once revocation is in effect.
func Remove(profileDir string) (int, error) {
	removed := 0
	var failed []string
	for entry := range profileTargets {
		path, err := TargetPath(profileDir, entry)
		if err != nil {
			continue
		}
		switch err := os.Remove(path); {
		case err == nil:
			removed++
		case os.IsNotExist(err):
			// Already gone.
		default:
			failed = append(failed, fmt.Sprintf("%s: %v", entry, err))
		}
	}
	if len(failed) > 0 {
		sort.Strings(failed)
		return removed, fmt.Errorf("remove credentials: %s", strings.Join(failed, "; "))
	}
	return removed, nil
}

// mergeClaudeConfig folds the delivered keys into any ~/.claude.json already
// on the machine instead of replacing the file.
//
// That file is not purely a credential: alongside the signed-in identity it
// accumulates the employee's own state -- project history, MCP servers, tips
// already seen. Overwriting it wholesale would silently wipe all of that
// every time an administrator re-runs login, which happens on every token
// refresh. Only the keys we deliver are touched.
//
// A file that is absent or unreadable as JSON is replaced outright: there is
// nothing to preserve, and refusing to write would leave the employee unable
// to sign in. A delivered payload that is not JSON is written verbatim.
func mergeClaudeConfig(target string, incoming []byte) ([]byte, error) {
	var delivered map[string]any
	if err := json.Unmarshal(incoming, &delivered); err != nil {
		// Not something we can merge into. Fall back to writing it verbatim,
		// which is how every other entry behaves, rather than failing the
		// whole deploy and leaving the Codex credentials undelivered too.
		return incoming, nil
	}

	existing := map[string]any{}
	if raw, err := os.ReadFile(target); err == nil {
		if err := json.Unmarshal(raw, &existing); err != nil {
			// Keep going with an empty base rather than failing the deploy.
			existing = map[string]any{}
		}
	}

	for k, v := range delivered {
		existing[k] = v
	}

	merged, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode merged claude.json: %w", err)
	}
	return append(merged, '\n'), nil
}
