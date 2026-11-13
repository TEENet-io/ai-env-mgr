// Package managedskills reconciles the company's machine-level Codex skills.
// It never opens an employee's personal skills, configuration or conversations.
package managedskills

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/TEENet-io/ai-env-mgr/internal/companyskills"
	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

type Manager struct {
	Root, StateDir string
	Protect        func(string) error
	ProtectState   func(string) error
	Stop           func(string) error
}

type journal struct {
	User    string                      `json:"user"`
	Owned   map[string]bool             `json:"owned"`
	Applied []model.ManagedSkillVersion `json:"applied"`
	Pending map[string]bool             `json:"pending"`
}

func safePath(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("managed skill path contains a link: %s", current)
			}
			if err := rejectReparse(current); err != nil {
				return err
			}
		}
		if parent := filepath.Dir(current); parent == current {
			return nil
		}
	}
}

func atomicJSON(path string, value any) error {
	if err := safePath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".company-skills-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func (m *Manager) load() (journal, error) {
	j := journal{Owned: map[string]bool{}, Pending: map[string]bool{}}
	path := filepath.Join(m.StateDir, "company-skills", "state.json")
	if err := safePath(path); err != nil {
		return j, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if err := json.Unmarshal(data, &j); err != nil {
		return j, fmt.Errorf("read company skill journal: %w", err)
	}
	if j.Owned == nil || j.Pending == nil {
		return j, errors.New("incomplete company skill journal")
	}
	for name := range j.Owned {
		if !companyskills.ValidName(name) {
			return j, errors.New("unsafe owned skill name in journal")
		}
	}
	return j, nil
}

func (m *Manager) save(j journal) error {
	return atomicJSON(filepath.Join(m.StateDir, "company-skills", "state.json"), j)
}

func directoryDigest(root string) (string, error) {
	if err := safePath(root); err != nil {
		return "", err
	}
	if _, err := os.Stat(root); err != nil {
		return "", err
	}
	files := map[string][]byte{}
	total := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := safePath(path); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > companyskills.MaxFileBytes {
			return errors.New("unexpected managed skill file")
		}
		if len(files) >= companyskills.MaxFiles {
			return errors.New("too many managed skill files")
		}
		total += int(info.Size())
		if total > companyskills.MaxBundleBytes {
			return errors.New("managed skill content too large")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = data
		return nil
	})
	return companyskills.Digest(files), err
}

func summaries(skills []model.ManagedSkill) []model.ManagedSkillVersion {
	out := make([]model.ManagedSkillVersion, 0, len(skills))
	for _, s := range skills {
		out = append(out, model.ManagedSkillVersion{Name: s.Name, Version: s.Version, SHA256: s.SHA256})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Sync validates the whole snapshot before touching installed skills. Ownership
// and pending restarts are journalled before mutation, so a service crash can
// neither turn a partial installation into an unmanaged conflict nor lose a
// required Codex cache refresh. Failed process stops are retried next cycle.
func (m *Manager) Sync(plan model.ManagedSkills) (result model.ManagedSkillsStatus, err error) {
	result.State, result.User = "failed", plan.User
	if m.Root == "" || m.StateDir == "" || m.Protect == nil || m.ProtectState == nil || m.Stop == nil {
		return result, errors.New("company skill manager is not configured")
	}
	if len(plan.Skills) > 0 && (strings.TrimSpace(plan.User) == "" || strings.EqualFold(plan.User, "administrator")) {
		return result, errors.New("company skills require a bound employee")
	}
	if len(plan.Skills) > companyskills.MaxSkills {
		return result, errors.New("too many company skills")
	}
	wanted := map[string]model.ManagedSkill{}
	total := 0
	for _, s := range plan.Skills {
		if err := companyskills.Validate(s); err != nil {
			return result, err
		}
		if _, ok := wanted[s.Name]; ok {
			return result, errors.New("duplicate company skill")
		}
		for _, data := range s.Files {
			total += len(data)
		}
		if total > companyskills.MaxPlanBytes {
			return result, errors.New("company skill snapshot too large")
		}
		wanted[s.Name] = s
	}
	private := filepath.Join(m.StateDir, "company-skills")
	if err := safePath(private); err != nil {
		return result, err
	}
	if err := os.MkdirAll(private, 0700); err != nil {
		return result, err
	}
	if err := m.ProtectState(private); err != nil {
		return result, fmt.Errorf("protect skill ownership state: %w", err)
	}
	j, err := m.load()
	if err != nil {
		return result, err
	}
	if err := safePath(m.Root); err != nil {
		return result, err
	}
	if err := os.MkdirAll(m.Root, 0755); err != nil {
		return result, err
	}
	// Protect the top level only. Child directories and files inherit these
	// rules; recursively granting directory rights breaks file readability.
	if err := m.Protect(m.Root); err != nil {
		return result, fmt.Errorf("protect company skill directory: %w", err)
	}
	root := filepath.Join(m.Root, "skills")
	staging := filepath.Join(m.Root, ".company-skills-staging")
	if err := safePath(root); err != nil {
		return result, err
	}
	if err := safePath(staging); err != nil {
		return result, err
	}
	changes := map[string]bool{}
	refresh := j.User != plan.User && len(j.Owned) > 0
	for name, skill := range wanted {
		path := filepath.Join(root, name)
		if err := safePath(path); err != nil {
			return result, err
		}
		_, statErr := os.Lstat(path)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return result, statErr
		}
		if statErr == nil && !j.Owned[name] {
			return result, fmt.Errorf("unmanaged company skill already exists: %s", name)
		}
		digest, readErr := directoryDigest(path)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return result, readErr
		}
		if digest != skill.SHA256 || readErr != nil {
			changes[name] = true
			refresh = refresh || j.Owned[name]
		}
	}
	for name := range j.Owned {
		if err := safePath(filepath.Join(root, name)); err != nil {
			return result, err
		}
		if _, exists := wanted[name]; !exists {
			refresh = true
		}
	}
	if refresh {
		if j.User != "" {
			j.Pending[j.User] = true
		}
		if plan.User != "" {
			j.Pending[plan.User] = true
		}
	}
	// Claim before creating files, after all conflicts and paths were checked.
	oldOwned := map[string]bool{}
	for name := range j.Owned {
		oldOwned[name] = true
	}
	for name := range wanted {
		j.Owned[name] = true
	}
	if err := m.save(j); err != nil {
		return result, err
	}
	for name := range changes {
		skill := wanted[name]
		stage, backup := filepath.Join(staging, name+".new"), filepath.Join(staging, name+".old")
		for _, path := range []string{stage, backup} {
			if err := safePath(path); err != nil {
				return result, err
			}
			if err := os.RemoveAll(path); err != nil {
				return result, err
			}
		}
		for relative, data := range skill.Files {
			path := filepath.Join(stage, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
				return result, err
			}
			if err := os.WriteFile(path, data, 0644); err != nil {
				return result, err
			}
		}
		if err := os.MkdirAll(root, 0755); err != nil {
			return result, err
		}
		installed := filepath.Join(root, name)
		if _, err := os.Stat(installed); err == nil {
			if err := os.Rename(installed, backup); err != nil {
				return result, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		if err := os.Rename(stage, installed); err != nil {
			_ = os.Rename(backup, installed)
			return result, err
		}
		if err := os.RemoveAll(backup); err != nil {
			return result, err
		}
	}
	for name := range oldOwned {
		if _, keep := wanted[name]; keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, name)); err != nil {
			return result, err
		}
		for _, suffix := range []string{".old", ".new"} {
			path := filepath.Join(staging, name+suffix)
			if err := safePath(path); err != nil {
				return result, err
			}
			if err := os.RemoveAll(path); err != nil {
				return result, err
			}
		}
	}
	// A crash after the directory swap can leave an old backup even when the
	// installed digest already matches. Clean it before acknowledging success.
	for name := range wanted {
		for _, suffix := range []string{".old", ".new"} {
			path := filepath.Join(staging, name+suffix)
			if err := safePath(path); err != nil {
				return result, err
			}
			if err := os.RemoveAll(path); err != nil {
				return result, err
			}
		}
	}
	j.Owned = map[string]bool{}
	for name := range wanted {
		j.Owned[name] = true
	}
	j.User, j.Applied = plan.User, summaries(plan.Skills)
	if err := m.save(j); err != nil {
		return result, err
	}
	result.User, result.Skills = j.User, j.Applied
	users := make([]string, 0, len(j.Pending))
	for user := range j.Pending {
		users = append(users, user)
	}
	sort.Strings(users)
	var failures []error
	for _, user := range users {
		if err := m.Stop(user); err != nil {
			failures = append(failures, fmt.Errorf("refresh Codex for %s: %w", user, err))
			continue
		}
		delete(j.Pending, user)
		if err := m.save(j); err != nil {
			return result, err
		}
	}
	for user := range j.Pending {
		result.RestartPending = append(result.RestartPending, user)
	}
	sort.Strings(result.RestartPending)
	result.State = "applied"
	if len(failures) > 0 {
		result.State = "restart_pending"
	}
	return result, errors.Join(failures...)
}
