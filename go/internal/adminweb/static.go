package adminweb

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

// staticAssets serves assets/static and names each file by its content, so
// that the browser and Cloudflare can keep it for good.
//
// Pages link "/static/app.css?v=<hash>" (the "static" template function). A
// request carrying the current hash is answered as immutable; a new build
// changes the hash and with it the URL, so nobody is served a stale file.
// Without the hash -- an old page, a bookmark -- the file must be checked
// each time.
type staticAssets struct {
	fsys    fs.FS
	version map[string]string // file name -> short content hash
	files   http.Handler
}

func newStaticAssets(fsys fs.FS) (*staticAssets, error) {
	a := &staticAssets{fsys: fsys, version: map[string]string{}, files: http.FileServer(http.FS(fsys))}
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		a.version[name] = hex.EncodeToString(sum[:6])
		return nil
	})
	return a, err
}

// URL is the link to name: versioned when name is one of the files.
func (a *staticAssets) URL(name string) string {
	if v, ok := a.version[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// ServeHTTP expects the /static/ prefix stripped. It runs inside
// secureHeaders, so it replaces that no-store: these files carry nothing
// private.
func (a *staticAssets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if v, ok := a.version[name]; ok && r.URL.Query().Get("v") == v {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
		if ok {
			w.Header().Set("ETag", `"`+a.version[name]+`"`)
		}
	}
	a.files.ServeHTTP(w, r)
}
