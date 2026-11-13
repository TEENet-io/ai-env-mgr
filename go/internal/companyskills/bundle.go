// Package companyskills validates and versions company-owned Codex skills.
// Content is delivered through the existing authenticated device channel.
package companyskills

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
	"gopkg.in/yaml.v3"
)

const (
	MaxFileBytes   = 1 << 20
	MaxBundleBytes = 4 << 20
	MaxFiles       = 64
	MaxSkills      = 64
)

var namePattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)+$`)

func ValidName(name string) bool { return len(name) <= 64 && namePattern.MatchString(name) }

// Digest is independent of map ordering; names and bytes are length delimited.
func Digest(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		fmt.Fprintf(h, "%d:%s%d:", len(name), name, len(files[name]))
		h.Write(files[name])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validPath(name string) bool {
	if name == "" || !utf8.ValidString(name) || len(name) > 180 || strings.HasPrefix(name, "/") || path.Clean(name) != name || strings.ContainsAny(name, "\\:<>\"|?*\x00\r\n\t") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return false
		}
		stem := strings.ToUpper(strings.SplitN(part, ".", 2)[0])
		if stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || regexp.MustCompile(`^(COM|LPT)[0-9¹²³]$`).MatchString(stem) {
			return false
		}
		for _, c := range part {
			if c < 32 || c == 127 {
				return false
			}
		}
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".txt", ".csv", ".json", ".yaml", ".yml", ".pdf", ".docx", ".xlsx", ".pptx", ".png", ".jpg", ".jpeg":
		return true
	}
	return false
}

func Validate(s model.ManagedSkill) error {
	if !ValidName(s.Name) || s.Version < 1 {
		return errors.New("skill 名称须使用带前缀的英文标识，例如 teenet-renewal，只含小写字母、数字和短横线；版本须大于零")
	}
	if len(s.Files) == 0 || len(s.Files) > MaxFiles {
		return errors.New("每个 skill 须包含 1–64 个文件")
	}
	total, seen := 0, map[string]bool{}
	for name, data := range s.Files {
		lower := strings.ToLower(name)
		if !validPath(name) || seen[lower] {
			return fmt.Errorf("不支持的文件路径或同名文件：%s", name)
		}
		seen[lower] = true
		total += len(data)
		if len(data) > MaxFileBytes || total > MaxBundleBytes {
			return errors.New("文件最大 1 MiB，每个 skill 最大 4 MiB")
		}
	}
	for name := range seen {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if seen[parent] {
				return errors.New("文件与目录名称冲突")
			}
		}
	}
	content := bytes.ReplaceAll(s.Files["SKILL.md"], []byte("\r\n"), []byte("\n"))
	if !utf8.Valid(content) || !bytes.HasPrefix(content, []byte("---\n")) {
		return errors.New("须包含 UTF-8 SKILL.md 和 YAML 说明头")
	}
	end := bytes.Index(content[4:], []byte("\n---\n"))
	if end < 0 {
		return errors.New("SKILL.md 说明头未结束")
	}
	var header struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(content[4:4+end], &header); err != nil {
		return fmt.Errorf("SKILL.md 说明头：%w", err)
	}
	if header.Name != s.Name || strings.TrimSpace(header.Description) == "" || len(header.Description) > 1024 {
		return errors.New("SKILL.md 名称须与下发名称相同，用途须为 1–1024 字节")
	}
	if len(bytes.TrimSpace(content[4+end+5:])) == 0 {
		return errors.New("SKILL.md 缺少操作步骤")
	}
	if s.SHA256 != Digest(s.Files) {
		return errors.New("skill SHA-256 校验不符")
	}
	return nil
}

// FromForm generates frontmatter without accepting YAML or shell fragments.
func FromForm(name, description, when, steps string, attachments map[string][]byte) (model.ManagedSkill, error) {
	if strings.TrimSpace(steps) == "" {
		return model.ManagedSkill{}, errors.New("请填写操作步骤")
	}
	n, _ := json.Marshal(name)
	d, _ := json.Marshal(strings.TrimSpace(description))
	content := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n## 何时使用\n\n%s\n\n## 操作步骤\n\n%s\n", n, d, strings.TrimSpace(when), strings.TrimSpace(steps))
	s := model.ManagedSkill{Name: name, Version: 1, Files: map[string][]byte{"SKILL.md": []byte(content)}}
	for file, data := range attachments {
		if file == "SKILL.md" {
			return s, errors.New("附件不能替换 SKILL.md")
		}
		s.Files[file] = data
	}
	s.SHA256 = Digest(s.Files)
	return s, Validate(s)
}

// FromZIP accepts a flat skill bundle or one enclosing skill-name directory.
// No extraction happens until every path, size and link has been checked.
func FromZIP(name string, data []byte) (model.ManagedSkill, error) {
	s := model.ManagedSkill{Name: name, Version: 1, Files: map[string][]byte{}}
	r, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return s, fmt.Errorf("读取 skill ZIP：%w", err)
	}
	if len(r.File) > MaxFiles*2 {
		return s, errors.New("ZIP 文件数过多")
	}
	total := 0
	for _, f := range r.File {
		if f.Mode().IsDir() {
			continue
		}
		if !f.Mode().IsRegular() {
			return s, errors.New("ZIP 不允许链接或特殊文件")
		}
		file := strings.TrimPrefix(f.Name, name+"/")
		if !validPath(file) || f.UncompressedSize64 > MaxFileBytes {
			return s, fmt.Errorf("ZIP 文件无效：%s", f.Name)
		}
		if _, ok := s.Files[file]; ok {
			return s, errors.New("ZIP 包含重复文件")
		}
		reader, err := f.Open()
		if err != nil {
			return s, err
		}
		body, readErr := io.ReadAll(io.LimitReader(reader, MaxFileBytes+1))
		closeErr := reader.Close()
		if readErr != nil {
			return s, readErr
		}
		if closeErr != nil {
			return s, closeErr
		}
		total += len(body)
		if len(body) > MaxFileBytes || total > MaxBundleBytes {
			return s, errors.New("ZIP 解压内容过大")
		}
		s.Files[file] = body
	}
	s.SHA256 = Digest(s.Files)
	return s, Validate(s)
}
