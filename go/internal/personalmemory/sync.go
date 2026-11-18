// Package personalmemory distributes the Codex personal-memory instructions
// and skills to a bound employee. Card contents stay in the employee profile
// and are never read or sent to Admin by this package.
package personalmemory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	startMarker = "<!-- teenet-personal-memory:start -->"
	endMarker   = "<!-- teenet-personal-memory:end -->"
	stateName   = "personal-memory-agent.json"
)

var skillNames = []string{"memory-save-success", "memory-save-problem", "memory-manage"}

func managedRelativePaths() map[string]bool {
	paths := map[string]bool{filepath.Join(".offline-package", "memory-store-path.txt"): true}
	for _, name := range skillNames {
		paths[filepath.Join("skills", name, "SKILL.md")] = true
	}
	return paths
}

type state struct {
	Version    string            `json:"version"`
	BlockHash  string            `json:"block_hash"`
	HelperHash string            `json:"helper_hash"`
	Files      map[string]string `json:"files"`
}

func hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func safePath(path string) error {
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symbolic link in managed memory path: %s", current)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
	}
}

func regularFile(path string) ([]byte, error) {
	if err := safePath(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", path)
	}
	return os.ReadFile(path)
}

func existingFile(path string) ([]byte, bool, error) {
	data, err := regularFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func writeFile(path string, data []byte) error {
	if err := safePath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := safePath(path); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func blockIn(content string) (int, int, string, error) {
	start := strings.Index(content, startMarker)
	end := strings.Index(content, endMarker)
	if (start < 0) != (end < 0) || (start >= 0 && end < start) {
		return 0, 0, "", errors.New("incomplete personal memory AGENTS.md block")
	}
	if start < 0 {
		return -1, -1, "", nil
	}
	end += len(endMarker)
	if strings.Contains(content[end:], startMarker) || strings.Contains(content[end:], endMarker) {
		return 0, 0, "", errors.New("duplicate personal memory AGENTS.md block")
	}
	return start, end, content[start:end], nil
}

func loadState(path string) (state, bool, error) {
	data, exists, err := existingFile(path)
	if err != nil || !exists {
		return state{}, exists, err
	}
	var current state
	if err := json.Unmarshal(data, &current); err != nil {
		return state{}, false, fmt.Errorf("read personal memory state: %w", err)
	}
	if current.Files == nil {
		return state{}, false, errors.New("personal memory state has no file list")
	}
	allowed := managedRelativePaths()
	if len(current.Files) != len(allowed) {
		return state{}, false, errors.New("personal memory state has an incomplete file list")
	}
	for relative := range current.Files {
		if !allowed[relative] {
			return state{}, false, fmt.Errorf("personal memory state contains an unmanaged path: %s", relative)
		}
	}
	for relative := range allowed {
		if _, ok := current.Files[relative]; !ok {
			return state{}, false, fmt.Errorf("personal memory state is missing a managed path: %s", relative)
		}
	}
	return current, true, nil
}

func sourceFiles(internalRoot string) (map[string][]byte, []byte, []byte, error) {
	block, err := regularFile(filepath.Join(internalRoot, "seed", "personal-memory", "AGENTS-block.md"))
	if err != nil {
		return nil, nil, nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(block)), startMarker) ||
		!strings.HasSuffix(strings.TrimSpace(string(block)), endMarker) {
		return nil, nil, nil, errors.New("invalid bundled personal memory AGENTS.md block")
	}
	helper, err := regularFile(filepath.Join(internalRoot, "memory_store.exe"))
	if err != nil {
		return nil, nil, nil, err
	}
	if len(helper) < 2 || helper[0] != 'M' || helper[1] != 'Z' {
		return nil, nil, nil, errors.New("bundled memory helper is not a Windows executable")
	}
	files := make(map[string][]byte, len(skillNames))
	for _, name := range skillNames {
		relative := filepath.Join("skills", name, "SKILL.md")
		data, err := regularFile(filepath.Join(internalRoot, "seed", "codex-home", relative))
		if err != nil {
			return nil, nil, nil, err
		}
		files[relative] = data
	}
	return files, []byte(strings.TrimSpace(string(block))), helper, nil
}

// Sync updates only files owned by this feature. Passing enabled=false removes
// the managed instructions and skill files while leaving .ai-memory untouched.
// It is safe to call on every Agent sync; unchanged files are not rewritten.
func Sync(internalRoot, profileDir, version string, enabled bool) (bool, error) {
	codexHome := filepath.Join(profileDir, ".codex")
	statePath := filepath.Join(codexHome, ".offline-package", stateName)
	current, hasState, err := loadState(statePath)
	if err != nil {
		return false, err
	}
	if !enabled && !hasState {
		return false, nil
	}

	var files map[string][]byte
	var block, helper []byte
	if enabled {
		files, block, helper, err = sourceFiles(internalRoot)
		if err != nil {
			return false, fmt.Errorf("personal memory bundle: %w", err)
		}
	}

	agentsPath := filepath.Join(codexHome, "AGENTS.md")
	agentsData, _, err := existingFile(agentsPath)
	if err != nil {
		return false, err
	}
	start, end, oldBlock, err := blockIn(string(agentsData))
	if err != nil {
		return false, err
	}
	if start >= 0 && oldBlock != string(block) && (!hasState || hash([]byte(oldBlock)) != current.BlockHash) {
		return false, errors.New("managed AGENTS.md block was edited; refusing to overwrite it")
	}

	pathRel := filepath.Join(".offline-package", "memory-store-path.txt")
	if enabled {
		files[pathRel] = []byte(filepath.Join(internalRoot, "memory_store.exe"))
	}
	// Check every target before making any change. An unmanaged same-name skill
	// is a conflict, while a file matching this bundle can be adopted from a
	// prior per-user bootstrap installation.
	for relative, desired := range files {
		path := filepath.Join(codexHome, relative)
		old, exists, err := existingFile(path)
		if err != nil {
			return false, err
		}
		if exists && !hasState && string(old) != string(desired) {
			return false, fmt.Errorf("unmanaged personal memory file exists: %s", path)
		}
		if exists && hasState && string(old) != string(desired) && hash(old) != current.Files[relative] {
			return false, fmt.Errorf("managed personal memory file was edited: %s", path)
		}
	}
	if !enabled {
		for relative, oldHash := range current.Files {
			path := filepath.Join(codexHome, relative)
			old, exists, err := existingFile(path)
			if err != nil {
				return false, err
			}
			if exists && hash(old) != oldHash {
				return false, fmt.Errorf("managed personal memory file was edited: %s", path)
			}
		}
	}

	changed := false
	updatedAgents := string(agentsData)
	if start >= 0 {
		updatedAgents = updatedAgents[:start] + string(block) + updatedAgents[end:]
	} else if enabled {
		separator := "\n\n"
		if len(agentsData) == 0 {
			separator = ""
		} else if strings.HasSuffix(updatedAgents, "\n") {
			separator = "\n"
		}
		updatedAgents += separator + string(block) + "\n"
	}
	if updatedAgents != string(agentsData) {
		if err := writeFile(agentsPath, []byte(updatedAgents)); err != nil {
			return false, err
		}
		changed = true
	}
	if !enabled {
		for relative := range current.Files {
			path := filepath.Join(codexHome, relative)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return changed, err
			}
			changed = true
		}
		if err := os.Remove(statePath); err != nil {
			return changed, err
		}
		return true, nil
	}

	next := state{Version: version, BlockHash: hash(block), HelperHash: hash(helper), Files: make(map[string]string, len(files))}
	for relative, desired := range files {
		path := filepath.Join(codexHome, relative)
		old, exists, err := existingFile(path)
		if err != nil {
			return changed, err
		}
		if !exists || string(old) != string(desired) {
			if err := writeFile(path, desired); err != nil {
				return changed, err
			}
			changed = true
		}
		next.Files[relative] = hash(desired)
	}
	if !hasState || current.Version != version || current.BlockHash != next.BlockHash ||
		current.HelperHash != next.HelperHash || !sameFiles(current.Files, next.Files) {
		data, err := json.MarshalIndent(next, "", "  ")
		if err != nil {
			return changed, err
		}
		if err := writeFile(statePath, append(data, '\n')); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func sameFiles(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range right {
		if left[key] != value {
			return false
		}
	}
	return true
}
