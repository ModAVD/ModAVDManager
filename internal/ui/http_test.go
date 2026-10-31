//go:build windows

package ui

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestLoginCookieFlow(t *testing.T) {
	h := newHarness(t, nil)

	resp := h.do("GET", "/?t="+testToken, nil, noCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: status %d, Location %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	set := resp.Header.Get("Set-Cookie")
	for _, want := range []string{h.s.cookieName() + "=" + testToken, "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(set, want) {
			t.Errorf("Set-Cookie %q lacks %q", set, want)
		}
	}
	cookies := resp.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies %v", cookies)
	}

	// the cookie from the login opens the API
	var info struct {
		Version  string       `json:"version"`
		Protocol int          `json:"protocol"`
		Root     emu.RootInfo `json:"root"`
	}
	expect(t, h.do("GET", "/api/info", nil, noCookie, withHeader("Cookie", cookies[0].Name+"="+cookies[0].Value)),
		http.StatusOK, &info)
	if info.Version == "" || info.Protocol == 0 || info.Root.Root != fakeRoot.Root || info.Root.Build != 35 {
		t.Fatalf("info %+v", info)
	}

	// a wrong token still lands on the page, without a cookie
	resp = h.do("GET", "/?t=wrong", nil, noCookie)
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("wrong token: status %d, Set-Cookie %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
}

func TestRejections(t *testing.T) {
	h := newHarness(t, nil)
	port := strconv.Itoa(h.s.port)

	expectError(t, h.do("GET", "/api/info", nil, noCookie), http.StatusUnauthorized, "unauthorized")
	expectError(t, h.do("GET", "/api/events", nil, noCookie), http.StatusUnauthorized, "unauthorized")
	expectError(t, h.do("GET", "/api/info", nil, withHeader("Cookie", h.s.cookieName()+"=nope")), http.StatusUnauthorized,
		"unauthorized")

	// DNS rebinding: a foreign name pointing at 127.0.0.1, or our name with another port
	expectError(t, h.do("GET", "/api/info", nil, withHost("evil.example:"+port)), http.StatusForbidden, "forbidden")
	expectError(t, h.do("GET", "/api/info", nil, withHost("127.0.0.1:1")), http.StatusForbidden, "forbidden")
	expectError(t, h.do("GET", "/", nil, withHost("evil.example:"+port)), http.StatusForbidden, "forbidden")
	expect(t, h.do("GET", "/api/info", nil, withHost("localhost:"+port)), http.StatusOK, nil)

	// non-GET needs our own Origin
	expectError(t, h.do("POST", "/api/instances", "{}", withHeader("Origin", "")), http.StatusForbidden, "forbidden")
	expectError(t, h.do("POST", "/api/instances", "{}", withHeader("Origin", "http://evil.example")),
		http.StatusForbidden, "forbidden")
	expectError(t, h.do("POST", "/api/instances", "{}", withHeader("Origin", "http://127.0.0.1:1")),
		http.StatusForbidden, "forbidden")
	expect(t, h.do("POST", "/api/instances", "{}", withHeader("Origin", "http://localhost:"+port)),
		http.StatusCreated, nil)

	// unknown API paths answer in JSON too
	expectError(t, h.do("GET", "/api/nothing", nil), http.StatusNotFound, errs.NotFound)
}

func TestErrorStatusMapping(t *testing.T) {
	h := newHarness(t, nil)
	cases := map[string]int{
		errs.Usage: 400, errs.InvalidArgument: 400, errs.NotFound: 404, errs.Busy: 409, errs.AlreadyExists: 409,
		errs.PortConflict: 409, errs.NotReady: 422, errs.PrecheckFailed: 422, errs.Unsupported: 422,
		errs.Canceled: 499, errs.LaunchFailed: 502, errs.Timeout: 504, errs.Internal: 500,
		errs.MetadataUnreadable: 500, errs.NotImplemented: 500,
	}
	for code, status := range cases {
		h.f.mu.Lock()
		h.f.infoErr[3] = errs.New(code, "测试错误 "+code).WithAdvice("建议 " + code)
		h.f.mu.Unlock()
		e := expectError(t, h.do("GET", "/api/instances/3", nil), status, code)
		if e.Message != "测试错误 "+code || e.Advice != "建议 "+code {
			t.Errorf("%s: body %+v", code, e)
		}
	}
	// a plain Go error is internal
	h.f.mu.Lock()
	h.f.infoErr[3] = io.ErrUnexpectedEOF
	h.f.mu.Unlock()
	expectError(t, h.do("GET", "/api/instances/3", nil), 500, errs.Internal)

	// bad indexes never reach emu
	for _, bad := range []string{"5", "10", "x", "mas_3", "-1"} {
		expectError(t, h.do("GET", "/api/instances/"+bad, nil), 400, errs.InvalidArgument)
	}
}

func TestCreateUpdateDelete(t *testing.T) {
	h := newHarness(t, nil)

	var info emu.InstanceInfo
	expect(t, h.do("POST", "/api/instances", map[string]any{"title": "测试机", "index": 2, "memory_mb": 4096, "cpu": 4,
		"data_partition_gb": 32, "fps": 30}), http.StatusCreated, &info)
	if info.Index != 2 || info.Title != "测试机" {
		t.Fatalf("created %+v", info)
	}
	expect(t, h.do("POST", "/api/instances", "{}"), http.StatusCreated, nil)
	expect(t, h.do("POST", "/api/instances", nil), http.StatusCreated, nil)
	h.f.mu.Lock()
	c := h.f.created
	h.f.mu.Unlock()
	if len(c) != 3 {
		t.Fatalf("creates %d", len(c))
	}
	r := c[0]
	if r.Title != "测试机" || *r.Index != 2 || *r.MemoryMB != 4096 || *r.CPU != 4 || *r.DataPartitionGB != 32 || *r.FPS != 30 {
		t.Fatalf("create request %+v", r)
	}
	if r := c[1]; r.Title != "" || r.Index != nil || r.MemoryMB != nil || r.CPU != nil || r.DataPartitionGB != nil || r.FPS != nil {
		t.Fatalf("empty create request %+v", r)
	}

	if e := expectError(t, h.do("POST", "/api/instances", `{"memory":4096}`), 400, errs.Usage); !strings.Contains(e.Message, "memory") {
		t.Errorf("unknown field message %q", e.Message)
	}
	if e := expectError(t, h.do("POST", "/api/instances", `{"memory_mb":"6G"}`), 400, errs.InvalidArgument); !strings.Contains(e.Message, "memory_mb") {
		t.Errorf("type error message %q", e.Message)
	}
	expectError(t, h.do("POST", "/api/instances", `{"cpu":4.5}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances", `{"cpu":`), 400, errs.Usage)
	expectError(t, h.do("POST", "/api/instances", `[]`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances", `{} {}`), 400, errs.Usage)
	expectError(t, h.do("POST", "/api/instances", `{}`, withHeader("Content-Type", "text/plain")), 400, errs.Usage)

	// update
	expectError(t, h.do("PATCH", "/api/instances/2", "{}"), 400, errs.Usage)
	var up emu.UpdateResult
	expect(t, h.do("PATCH", "/api/instances/2", `{"fps":30}`), http.StatusOK, &up)
	if up.Applied["fps"] != float64(30) {
		t.Fatalf("update result %+v", up)
	}
	expect(t, h.do("PATCH", "/api/instances/2", `{"title":"新名字","memory_mb":8192,"cpu":8}`), http.StatusOK, nil)
	expect(t, h.do("PATCH", "/api/instances/2", `{"data_partition_gb":96}`), http.StatusOK, nil)
	expectError(t, h.do("PATCH", "/api/instances/7", `{"fps":30}`), 404, errs.NotFound)
	h.f.mu.Lock()
	u := h.f.updated
	h.f.mu.Unlock()
	if len(u) != 4 || *u[0].FPS != 30 || u[0].Title != nil || u[0].MemoryMB != nil || u[0].CPU != nil ||
		u[0].DataPartitionGB != nil {
		t.Fatalf("update requests %+v", u)
	}
	if *u[1].Title != "新名字" || *u[1].MemoryMB != 8192 || *u[1].CPU != 8 || u[1].FPS != nil {
		t.Fatalf("update request %+v", u[1])
	}
	if u[2].DataPartitionGB == nil || *u[2].DataPartitionGB != 96 || u[2].FPS != nil {
		t.Fatalf("update request %+v", u[2])
	}

	// delete
	expectError(t, h.do("DELETE", "/api/instances/2", nil), 400, errs.Usage)
	expectError(t, h.do("DELETE", "/api/instances/2?confirm=yes", nil), 400, errs.Usage)
	var del emu.DeleteResult
	expect(t, h.do("DELETE", "/api/instances/2?confirm=1", nil), http.StatusOK, &del)
	if del.Index != 2 || del.Name != "mas_2" {
		t.Fatalf("delete result %+v", del)
	}
	expectError(t, h.do("DELETE", "/api/instances/2?confirm=1", nil), 404, errs.NotFound)

	// window
	var win emu.WindowResult
	expect(t, h.do("POST", "/api/instances/3/window", `{"visible":true}`), http.StatusOK, &win)
	if !win.Visible || win.Index != 3 {
		t.Fatalf("window %+v", win)
	}
	expectError(t, h.do("POST", "/api/instances/3/window", `{}`), 400, errs.Usage)
}

var testWeb = fstest.MapFS{
	"index.html":      {Data: []byte("<!doctype html><title>t</title>")},
	"app.js":          {Data: []byte("export {}")},
	"app.css":         {Data: []byte("body{}")},
	"js/view.mjs":     {Data: []byte("export {}")},
	"assets/icon.png": {Data: []byte("\x89PNG\r\n\x1a\n")},
	"assets/logo.svg": {Data: []byte("<svg/>")},
	"assets/x.woff2":  {Data: []byte("wOF2")},
	"data.bin":        {Data: []byte{1, 2, 3}},
	"dev/index.html":  {Data: []byte("<!doctype html><title>mock</title>")},
	"dev/mock.js":     {Data: []byte("export {}")},
}

const wantCSP = "default-src 'self'; img-src 'self' blob: data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func TestStaticFiles(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.web = testWeb })

	types := map[string]string{
		"/":                "text/html; charset=utf-8",
		"/index.html":      "text/html; charset=utf-8",
		"/app.js":          "text/javascript; charset=utf-8",
		"/js/view.mjs":     "text/javascript; charset=utf-8",
		"/app.css":         "text/css; charset=utf-8",
		"/assets/icon.png": "image/png",
		"/assets/logo.svg": "image/svg+xml",
		"/assets/x.woff2":  "font/woff2",
		"/data.bin":        "application/octet-stream",
	}
	for path, ct := range types {
		resp := h.do("GET", path, nil, noCookie) // static files need no cookie
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
		hd := resp.Header
		if hd.Get("Content-Type") != ct {
			t.Errorf("%s: Content-Type %q, want %q", path, hd.Get("Content-Type"), ct)
		}
		if hd.Get("Content-Security-Policy") != wantCSP || hd.Get("X-Content-Type-Options") != "nosniff" ||
			hd.Get("Referrer-Policy") != "no-referrer" || hd.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: headers %v", path, hd)
		}
		if len(body) == 0 {
			t.Errorf("%s: empty body", path)
		}
	}

	// revalidation
	resp := h.do("GET", "/app.js", nil)
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	resp = h.do("GET", "/app.js", nil, withHeader("If-None-Match", etag))
	resp.Body.Close()
	if etag == "" || resp.StatusCode != http.StatusNotModified {
		t.Fatalf("ETag %q, conditional status %d", etag, resp.StatusCode)
	}

	for _, path := range []string{"/dev/", "/dev/index.html", "/dev/mock.js", "/missing.js", "/js/", "/API.md"} {
		resp := h.do("GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
	resp = h.do("POST", "/", "{}")
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /: status %d", resp.StatusCode)
	}
	// API responses carry the security headers too, and are never cached
	resp = h.do("GET", "/api/info", nil)
	resp.Body.Close()
	if resp.Header.Get("Content-Security-Policy") != wantCSP || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("API headers %v", resp.Header)
	}

	dev := newHarness(t, func(o *Options) { o.web = testWeb; o.Dev = true })
	for _, path := range []string{"/dev/", "/dev/mock.js"} {
		resp := dev.do("GET", path, nil)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("dev %s: status %d", path, resp.StatusCode)
		}
	}
}

func TestEmbeddedPage(t *testing.T) {
	h := newHarness(t, nil)
	if h.s.files["index.html"] == nil {
		t.Fatal("web/index.html is not embedded")
	}
	resp := h.do("GET", "/", nil, noCookie)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("GET /: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// TestUpdateSettings: the settings' update part — read without going online, source and CDK saved (the CDK never
// comes back), installing left to the command line.
func TestUpdateSettings(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	h := newHarness(t, nil)
	var v map[string]any
	expect(t, h.do("GET", "/api/update", nil), http.StatusOK, &v)
	if v["source"] != "github" || v["cdk_set"] != false || v["install_by_cli"] != true || v["checking"] != false {
		t.Fatalf("GET /api/update: %v", v)
	}
	resp := h.do("POST", "/api/update/settings", `{"source":"mirrorchyan","cdk":"cdk-xyz-1"}`)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "cdk-xyz-1") ||
		!strings.Contains(body, `"source":"mirrorchyan"`) || !strings.Contains(body, `"cdk_set":true`) {
		t.Fatalf("POST settings: %d %s", resp.StatusCode, body)
	}
	expect(t, h.do("POST", "/api/update/settings", `{"cdk":""}`), http.StatusOK, &v)
	if v["cdk_set"] != false || v["source"] != "mirrorchyan" {
		t.Fatalf("clear CDK: %v", v)
	}
	expectError(t, h.do("POST", "/api/update/settings", `{}`), 400, errs.Usage)
	expectError(t, h.do("POST", "/api/update/settings", `{"source":"ftp"}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/update/apply", `{}`), 404, errs.NotFound)
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
