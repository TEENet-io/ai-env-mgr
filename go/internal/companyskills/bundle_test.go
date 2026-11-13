package companyskills

import (
	"archive/zip"
	"bytes"
	"os"
	"testing"

	"github.com/TEENet-io/ai-env-mgr/internal/model"
)

func testBundle(name, text string) model.ManagedSkill {
	s := model.ManagedSkill{Name: name, Version: 1, Files: map[string][]byte{"SKILL.md": []byte("---\nname: " + name + "\ndescription: Test business workflow\n---\n\n" + text)}}
	s.SHA256 = Digest(s.Files)
	return s
}

func TestZIPRejectsLinksTraversalAndDuplicates(t *testing.T) {
	for _, unsafeName := range []string{"../escape.md", "bippo-demo/../../escape.md", "template.ps1", "link.md", "SKILL.md"} {
		t.Run(unsafeName, func(t *testing.T) {
			var data bytes.Buffer
			writer := zip.NewWriter(&data)
			f, err := writer.Create("SKILL.md")
			if err != nil {
				t.Fatal(err)
			}
			f.Write(testBundle("bippo-demo", "steps").Files["SKILL.md"])
			header := &zip.FileHeader{Name: unsafeName}
			header.SetMode(0644)
			if unsafeName == "link.md" {
				header.SetMode(os.ModeSymlink | 0777)
			}
			f, err = writer.CreateHeader(header)
			if err != nil {
				t.Fatal(err)
			}
			f.Write([]byte("bad"))
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := FromZIP("bippo-demo", data.Bytes()); err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
		})
	}
	var data bytes.Buffer
	writer := zip.NewWriter(&data)
	for name, content := range testBundle("bippo-demo", "steps").Files {
		f, _ := writer.Create("bippo-demo/" + name)
		f.Write(content)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := FromZIP("bippo-demo", data.Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestBundleRejectsWindowsPathsAndExecutableContent(t *testing.T) {
	for _, path := range []string{"../escape.md", "a/../../escape.md", "a\\file.md", "C:/file.md", "SKILL.md:stream", "CON.txt", "a/NUL.md", "a./test.md", "a /test.md", "a//test.md", "scripts/run.ps1", "script.js", "template.xlsm"} {
		t.Run(path, func(t *testing.T) {
			s := testBundle("bippo-renewal", "Follow these steps")
			s.Files[path] = []byte("bad")
			s.SHA256 = Digest(s.Files)
			if err := Validate(s); err == nil {
				t.Fatal("unsafe bundle accepted")
			}
		})
	}
	s := testBundle("bippo-renewal", "Follow these steps")
	s.Files["references/template.csv"] = []byte("customer,date\n")
	s.Files["references/客户 清单.csv"] = []byte("客户,日期\n")
	s.SHA256 = Digest(s.Files)
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
	s.Files["references/TEMPLATE.csv"] = []byte("collision")
	s.SHA256 = Digest(s.Files)
	if err := Validate(s); err == nil {
		t.Fatal("case-insensitive Windows collision accepted")
	}
}

func TestBundleRequiresMatchingNameHeaderAndDigest(t *testing.T) {
	s := testBundle("bippo-renewal", "Steps")
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
	s.SHA256 = "bad"
	if err := Validate(s); err == nil {
		t.Fatal("bad digest accepted")
	}
	s = testBundle("bippo-renewal", "Steps")
	s.Name = "../other"
	if err := Validate(s); err == nil {
		t.Fatal("unsafe name accepted")
	}
	s = testBundle("bippo-renewal", "Steps")
	s.Files["SKILL.md"] = []byte("---\nname: other\ndescription: test\n---\nSteps")
	s.SHA256 = Digest(s.Files)
	if err := Validate(s); err == nil {
		t.Fatal("mismatched skill header accepted")
	}
}
