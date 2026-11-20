package adminweb

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// simplifiedChars are characters written differently in Traditional Chinese.
// The console must not use them; 樂山 is the reference for wording.
const simplifiedChars = "与业丢两严个临为么义书于云产仅从仓们价优会传体余侧储关内册写决况净几凭划则刚创删别办务动区单占厂历压参双发变叠号后吗启员响团围国图处备复头宝实审对导将尝尽属币带帮干并库应开异弃张强归当录彻径态总户执扫护报拦择挡换据数断旧时显暂机权条来构标样档检汇沟没测浏滚滤点状独环现电监盖盘着码确离种积称稳筛签简类紧约级纹线组细织终经绑结给络绝统继续维绿缀缓编缩网职联脑脱腾节范荐获营虚补装见规览计订认让讯记许设访证评识诉试话询该详误说请读谁调谱负责败账贴费资转轮软载辑输达迁过运还这进远连迟适选递遗邮采释钉钟钥钮链销锁错镜长门闭问间闻阅阈队阶际随隐静页项须预题额飞验骤"

// TestInterfaceTextIsTraditional reads every page, script and string the console
// shows. The exceptions are the old spelling of "none" still accepted in a form,
// and the ticket cards for our own chat group, which are simplified on purpose.
func TestInterfaceTextIsTraditional(t *testing.T) {
	check := func(where, text string) {
		t.Helper()
		if i := strings.IndexAny(text, simplifiedChars); i >= 0 {
			t.Errorf("%s has simplified text: %q", where, text[i:min(i+30, len(text))])
		}
	}
	for _, pattern := range []string{"assets/*.html", "assets/static/*.js"} {
		names, err := fs.Glob(assetFS, pattern)
		if err != nil || len(names) == 0 {
			t.Fatalf("%s: %v", pattern, err)
		}
		for _, name := range names {
			body, err := fs.ReadFile(assetFS, name)
			if err != nil {
				t.Fatal(err)
			}
			check(name, string(body))
		}
	}
	var paths []string
	for _, pattern := range []string{"*.go", "../dbstore/*.go", "../worker/*.go", "../ops/*.go", "../admincore/*.go", "../litellm/*.go", "../notify/*.go"} {
		found, err := filepath.Glob(filepath.FromSlash(pattern))
		if err != nil || len(found) == 0 {
			t.Fatalf("%s: %v", pattern, err)
		}
		paths = append(paths, found...)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || filepath.ToSlash(path) == "../worker/ticketnotify_group.go" {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				check(fset.Position(lit.Pos()).String(), lit.Value)
			}
			return true
		})
	}
}
