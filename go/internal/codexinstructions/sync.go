// Package codexinstructions keeps one agent-managed block in the employee's
// global Codex instructions (%USERPROFILE%\.codex\AGENTS.md). Everything
// outside the block is the employee's and is never touched.
package codexinstructions

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	Begin = "<!-- BEGIN ai-env-mgr managed: windows-utf8. Edits inside this block are replaced. -->"
	End   = "<!-- END ai-env-mgr managed: windows-utf8 -->"
)

// Body is what every model should know on a Chinese Windows desktop: the
// console encoding is GBK (code page 936), so PowerShell prints UTF-8 files
// as mojibake to Codex even when it reads them with -Encoding UTF8.
const Body = "## Windows 中文环境下读写文件\n\n" +
	"在 Windows 上用 PowerShell 读取或输出含中文的文本（SKILL.md、Skill 附件、模板、员工文档）时，先在同一条命令开头执行：\n\n" +
	"```powershell\n[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; $OutputEncoding = [System.Text.Encoding]::UTF8\n```\n\n" +
	"再用 `Get-Content -Raw -Encoding UTF8` 读取文件。输出仍是乱码时，不要根据乱码猜测内容：换一种方式重新读取，或如实说明读取失败。\n"

func block() []byte { return []byte(Begin + "\n" + Body + End + "\n") }

// Sync writes the block into profileDir\.codex\AGENTS.md when enabled, or
// removes it when not. It reports whether the file changed. A profile without
// a .codex directory is left alone: credential delivery creates that
// directory with the employee's access, and the block follows it.
func Sync(profileDir string, enabled bool) (bool, error) {
	dir := filepath.Join(profileDir, ".codex")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, fmt.Errorf("%s is not a plain directory", dir)
	}
	if err := rejectReparse(dir); err != nil {
		return false, err
	}
	path := filepath.Join(dir, "AGENTS.md")
	current, err := readRegular(path)
	if err != nil {
		return false, err
	}
	next, err := apply(current, enabled)
	if err != nil {
		return false, err
	}
	if bytes.Equal(next, current) {
		return false, nil
	}
	if len(bytes.TrimSpace(next)) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return true, nil
	}
	return true, writeAtomic(path, next)
}

// apply returns content with the managed block present (enabled) or absent.
func apply(content []byte, enabled bool) ([]byte, error) {
	start := bytes.Index(content, []byte(Begin))
	stop := bytes.Index(content, []byte(End))
	if bytes.Count(content, []byte(Begin)) > 1 || bytes.Count(content, []byte(End)) > 1 {
		return nil, errors.New("AGENTS.md holds more than one managed block")
	}
	if (start < 0) != (stop < 0) || (start >= 0 && stop < start) {
		return nil, errors.New("AGENTS.md holds an incomplete managed block")
	}
	var before, after []byte
	if start < 0 {
		before = content
	} else {
		before = content[:start]
		after = content[stop+len(End):]
		after = bytes.TrimPrefix(bytes.TrimPrefix(after, []byte("\r")), []byte("\n"))
	}
	out := append([]byte{}, before...)
	if enabled {
		if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
			out = append(out, '\n')
		}
		if len(bytes.TrimSpace(out)) > 0 && !bytes.HasSuffix(out, []byte("\n\n")) && start < 0 {
			out = append(out, '\n')
		}
		out = append(out, block()...)
	}
	return append(out, after...), nil
}

func readRegular(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if err := rejectReparse(path); err != nil {
		return nil, err
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("%s is larger than 1 MiB", path)
	}
	return os.ReadFile(path)
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".AGENTS.md.*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
