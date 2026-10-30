//go:build windows

package ui

import (
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// The page, embedded whole. web/dev/ (the page's mock backend) is in it too but only served with Options.Dev.
//
//go:embed web
var embedded embed.FS

const csp = "default-src 'self'; img-src 'self' blob: data:; style-src 'self'; script-src 'self'; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// contentTypes are set explicitly: mime.TypeByExtension reads the Windows registry, where .js is sometimes
// text/plain, and a module script with that type does not run.
var contentTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".json":        "application/json; charset=utf-8",
	".map":         "application/json; charset=utf-8",
	".webmanifest": "application/manifest+json",
	".txt":         "text/plain; charset=utf-8",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".ico":         "image/x-icon",
	".woff2":       "font/woff2",
	".woff":        "font/woff",
	".wasm":        "application/wasm",
}

type staticFile struct {
	data  []byte
	ctype string
	etag  string
}

// loadStatic reads every file of fsys once (the page is small) with its type and ETag.
func loadStatic(fsys fs.FS) (map[string]*staticFile, error) {
	files := map[string]*staticFile{}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		ctype, ok := contentTypes[strings.ToLower(path.Ext(p))]
		if !ok {
			ctype = "application/octet-stream"
		}
		sum := sha256.Sum256(data)
		files[p] = &staticFile{data: data, ctype: ctype, etag: `"` + hex.EncodeToString(sum[:12]) + `"`}
		return nil
	})
	return files, err
}

// handleStatic serves the page. No cookie is needed (the page has to load before it can have one), but the Host
// check of ServeHTTP applies. "/?t=<token>" is the login: cookie + redirect to "/".
func (s *server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "405 只支持 GET", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if p == "/" && r.URL.Query().Has("t") {
		s.login(w, r)
		return
	}
	if strings.HasSuffix(p, "/") {
		p += "index.html"
	}
	name := strings.TrimPrefix(p, "/")
	f := s.files[name]
	if f == nil || !fs.ValidPath(name) || (!s.dev && (name == "dev" || strings.HasPrefix(name, "dev/"))) {
		http.Error(w, "404 找不到 "+r.URL.Path, http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	// no-cache = revalidate every time: the exe and with it the page change with every update
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(f.data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(f.data)
	}
}

// login sets the cookie when the token is right and redirects to "/" either way: a stale token (an old bookmark,
// a window of an earlier run) gets the page, whose API calls then report "not logged in".
func (s *server) login(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(s.token)) == 1 {
		http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: s.token, Path: "/", HttpOnly: true,
			SameSite: http.SameSiteStrictMode})
	} else {
		s.log.Warnf("有人用错误的令牌打开管理器页面")
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusFound)
}
