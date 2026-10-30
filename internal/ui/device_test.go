//go:build windows

package ui

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"modavd/internal/adb"
	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestScreenshot(t *testing.T) {
	h := newHarness(t, nil)
	h.f.put(3, emu.StatusOnline)
	png := append(append([]byte{}, pngSignature...), []byte("IHDR rest of the image")...)
	h.f.mu.Lock()
	h.f.shot = adb.Result{Code: 0, Stdout: png}
	h.f.shotDelay = 150 * time.Millisecond
	h.f.mu.Unlock()

	// five viewers at once share one capture
	var wg sync.WaitGroup
	bodies := make([][]byte, 5)
	for k := range bodies {
		wg.Go(func() {
			resp := h.do("GET", "/api/instances/3/screenshot", nil)
			defer resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" ||
				resp.Header.Get("Cache-Control") != "no-store" {
				t.Errorf("screenshot: %d %v", resp.StatusCode, resp.Header)
			}
			bodies[k], _ = io.ReadAll(resp.Body)
		})
	}
	wg.Wait()
	for _, b := range bodies {
		if !bytes.Equal(b, png) {
			t.Fatalf("screenshot body %q", b)
		}
	}
	if n := h.f.adbCalls.Load(); n != 1 {
		t.Fatalf("%d captures for five concurrent requests", n)
	}
	// a capture younger than 300 ms is served again
	expect(t, h.do("GET", "/api/instances/3/screenshot", nil), 200, nil)
	if n := h.f.adbCalls.Load(); n != 1 {
		t.Fatalf("fresh capture not reused (%d captures)", n)
	}
	time.Sleep(shotReuse + 50*time.Millisecond)
	expect(t, h.do("GET", "/api/instances/3/screenshot", nil), 200, nil)
	if n := h.f.adbCalls.Load(); n != 2 {
		t.Fatalf("stale capture reused (%d captures)", n)
	}

	// adb output that is no PNG
	h.f.mu.Lock()
	h.f.shot = adb.Result{Code: 0, Stdout: []byte("/system/bin/sh: screencap: not found\r\n")}
	h.f.shotDelay = 0
	h.f.mu.Unlock()
	time.Sleep(shotReuse + 50*time.Millisecond)
	expectError(t, h.do("GET", "/api/instances/3/screenshot", nil), 500, errs.Internal)

	// not booted: 409; unknown instance: 404
	h.f.mu.Lock()
	h.f.shot = adb.Result{Code: 1, Out: "error: device 'emulator-20040' not found"}
	h.f.mu.Unlock()
	expectError(t, h.do("GET", "/api/instances/4/screenshot", nil), http.StatusConflict, errs.Busy)
	expectError(t, h.do("GET", "/api/instances/8/screenshot", nil), http.StatusNotFound, errs.NotFound)
}

func TestInputValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.f.put(3, emu.StatusOnline)

	ok := map[string]string{
		`{"type":"tap","x":100,"y":200}`:      "input tap 100 200",
		`{"type":"tap","x":1279.6,"y":719.2}`: "input tap 1279 719",
		`{"type":"tap","x":0,"y":0}`:          "input tap 0 0",
		`{"type":"key","key":"back"}`:         "input keyevent KEYCODE_BACK",
		`{"type":"key","key":"home"}`:         "input keyevent KEYCODE_HOME",
		`{"type":"key","key":"recents"}`:      "input keyevent KEYCODE_APP_SWITCH",
	}
	for body, want := range ok {
		var out map[string]any
		expect(t, h.do("POST", "/api/instances/3/input", body), 200, &out)
		if len(out) != 0 {
			t.Errorf("%s answered %v", body, out)
		}
		h.f.mu.Lock()
		got := h.f.shells[len(h.f.shells)-1]
		h.f.mu.Unlock()
		if got != want {
			t.Errorf("%s ran %q, want %q", body, got, want)
		}
	}
	bad := map[string]string{
		`{"type":"tap","x":1280,"y":10}`:    errs.InvalidArgument,
		`{"type":"tap","x":10,"y":720}`:     errs.InvalidArgument,
		`{"type":"tap","x":-1,"y":10}`:      errs.InvalidArgument,
		`{"type":"tap","x":10}`:             errs.Usage,
		`{"type":"tap","x":"1","y":2}`:      errs.InvalidArgument,
		`{"type":"swipe","x":1,"y":2}`:      errs.InvalidArgument,
		`{"type":"key","key":"menu"}`:       errs.InvalidArgument,
		`{"type":"key"}`:                    errs.InvalidArgument,
		`{"type":"key","key":"back","z":1}`: errs.Usage,
		``:                                  errs.InvalidArgument,
	}
	h.f.mu.Lock()
	before := len(h.f.shells)
	h.f.mu.Unlock()
	for body, code := range bad {
		expectError(t, h.do("POST", "/api/instances/3/input", body), 400, code)
	}
	h.f.mu.Lock()
	after := len(h.f.shells)
	h.f.mu.Unlock()
	if after != before {
		t.Fatalf("invalid input reached the guest (%d commands)", after-before)
	}

	expectError(t, h.do("POST", "/api/instances/4/input", `{"type":"key","key":"back"}`), http.StatusConflict, errs.Busy)
	expectError(t, h.do("POST", "/api/instances/8/input", `{"type":"key","key":"back"}`), http.StatusNotFound, errs.NotFound)
	h.f.mu.Lock()
	h.f.shellCode = 1
	h.f.mu.Unlock()
	expectError(t, h.do("POST", "/api/instances/3/input", `{"type":"key","key":"back"}`), 500, errs.Internal)
}

func TestLogTail(t *testing.T) {
	h := newHarness(t, nil)
	logs := h.s.root.LogsDir()

	expectError(t, h.do("GET", "/api/instances/3/log", nil), 404, errs.NotFound) // no logs folder yet
	if err := os.MkdirAll(logs, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, text string, age time.Duration) {
		t.Helper()
		p := filepath.Join(logs, name)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-age)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	var b strings.Builder
	for k := 1; k <= 500; k++ {
		b.WriteString("line " + strconv.Itoa(k) + "\r\n")
	}
	write("mas_3-20261028-100000.log", "old log\n", time.Hour)
	write("mas_3-20261028-110000.log", b.String(), time.Minute)
	write("mas_30-20261028-120000.log", "not instance 3\n", 0) // no such instance, but a prefix trap
	write("mas_4-20261028-120000.log", "instance 4\n", 0)
	write("logcat-mas_3-20261028-110000.txt", "10-28 11:00:00 I tag: hello\n10-28 11:00:01 I tag: 世界\n", 0)

	get := func(query string) (int, string, http.Header) {
		resp := h.do("GET", "/api/instances/3/log"+query, nil)
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(data), resp.Header
	}
	status, text, hd := get("?kind=emulator&lines=3")
	if status != 200 || text != "line 498\r\nline 499\r\nline 500\r\n" {
		t.Fatalf("tail: %d %q", status, text)
	}
	if hd.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type %q", hd.Get("Content-Type"))
	}
	if _, text, _ = get(""); strings.Count(text, "\n") != defaultLogLines || !strings.HasPrefix(text, "line 201\r\n") {
		t.Fatalf("default tail starts %q (%d lines)", text[:20], strings.Count(text, "\n"))
	}
	if _, text, _ = get("?lines=10000"); !strings.HasPrefix(text, "line 1\r\n") {
		t.Fatalf("whole file starts %q", text[:20])
	}
	if status, text, _ = get("?kind=logcat&lines=1"); status != 200 || text != "10-28 11:00:01 I tag: 世界\n" {
		t.Fatalf("logcat: %d %q", status, text)
	}
	for _, q := range []string{"?kind=kernel", "?lines=0", "?lines=x", "?lines=10001"} {
		expectError(t, h.do("GET", "/api/instances/3/log"+q, nil), 400, errs.InvalidArgument)
	}
	expectError(t, h.do("GET", "/api/instances/6/log", nil), 404, errs.NotFound)
	expectError(t, h.do("GET", "/api/instances/4/log?kind=logcat", nil), 404, errs.NotFound)
}

func TestTailLinesLimits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.log")
	var b bytes.Buffer
	// ~180-byte lines: 1 MB holds fewer than maxLogLines of them, so the byte cap is what cuts
	line := "0123456789 一行日志 " + strings.Repeat("abcdefghij", 15) + "\n"
	for b.Len() < 3<<20 {
		b.WriteString(line)
	}
	b.Write([]byte{0xff, 0xfe, 'x', '\n'}) // not UTF-8 (an ANSI code page line)
	if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	text, err := tailLines(p, maxLogLines, maxLogTail)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > maxLogTail+16 || len(text) < maxLogTail-len(line)-16 || !utf8.ValidString(text) ||
		!strings.HasPrefix(text, "0123456789 ") || !strings.HasSuffix(text, "\ufffd\ufffdx\n") {
		t.Fatalf("tail of a big file: %d bytes, starts %q, ends %q", len(text), text[:16], text[len(text)-8:])
	}
	// a file without a line end, and an empty one
	os.WriteFile(p, []byte("no newline"), 0o644)
	if text, _ := tailLines(p, 5, maxLogTail); text != "no newline" {
		t.Fatalf("%q", text)
	}
	os.WriteFile(p, nil, 0o644)
	if text, _ := tailLines(p, 5, maxLogTail); text != "" {
		t.Fatalf("%q", text)
	}
}

func TestOpenFolders(t *testing.T) {
	h := newHarness(t, nil)
	r := h.s.root
	expect(t, h.do("POST", "/api/open", `{"what":"root"}`), 200, nil)
	expect(t, h.do("POST", "/api/open", `{"what":"logs"}`), 200, nil)
	if st, err := os.Stat(r.LogsDir()); err != nil || !st.IsDir() {
		t.Fatalf("logs folder not created: %v", err)
	}
	expectError(t, h.do("POST", "/api/open", `{"what":"instance","index":3}`), 404, errs.NotFound)
	inst := filepath.Join(r.AvdHome(), "mas_3.avd")
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	expect(t, h.do("POST", "/api/open", `{"what":"instance","index":3}`), 200, nil)
	expectError(t, h.do("POST", "/api/open", `{"what":"instance"}`), 400, errs.Usage)
	expectError(t, h.do("POST", "/api/open", `{"what":"instance","index":5}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/open", `{"what":"desktop"}`), 400, errs.InvalidArgument)
	h.mu.Lock()
	defer h.mu.Unlock()
	want := []string{r.Dir, r.LogsDir(), inst}
	if strings.Join(h.opened, "|") != strings.Join(want, "|") {
		t.Fatalf("opened %v, want %v", h.opened, want)
	}
}

func multipartBody(t *testing.T, field, name, content string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("note", "ignored")
	fw, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(fw, content)
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func TestInstallUpload(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	h := newHarness(t, nil)
	h.f.put(3, emu.StatusOnline)

	body, ct := multipartBody(t, "apk", "游戏 1.2.apk", "PK\x03\x04 fake apk")
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/install", body, withHeader("Content-Type", ct)), http.StatusAccepted, &j)
	if j.Kind != kindInstall || j.Index != 3 {
		t.Fatalf("install job %+v", j)
	}
	var done struct {
		jobView
		Result *emu.InstallResult `json:"result"`
	}
	h.waitJob(j.ID, stateIs(stateDone))
	expect(t, h.do("GET", "/api/jobs/"+j.ID, nil), 200, &done)
	if done.Result == nil || done.Result.APK != "游戏 1.2.apk" || done.Result.Package != "com.example" {
		t.Fatalf("install result %+v", done.Result)
	}
	h.f.mu.Lock()
	inst := h.f.installs[0]
	h.f.mu.Unlock()
	if inst.content != "PK\x03\x04 fake apk" || filepath.Base(inst.path) != "游戏 1.2.apk" ||
		!strings.HasPrefix(inst.path, filepath.Join(tmp, "modavd-ui")+`\`) {
		t.Fatalf("installed %+v", inst)
	}
	if _, err := os.Stat(filepath.Dir(inst.path)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("upload folder left behind: %v", err)
	}

	body, ct = multipartBody(t, "apk", "game.xapk", "x")
	expectError(t, h.do("POST", "/api/instances/3/install", body, withHeader("Content-Type", ct)), 400, errs.InvalidArgument)
	body, ct = multipartBody(t, "file", "game.apk", "x")
	expectError(t, h.do("POST", "/api/instances/3/install", body, withHeader("Content-Type", ct)), 400, errs.Usage)
	body, ct = multipartBody(t, "apk", "empty.apk", "")
	expectError(t, h.do("POST", "/api/instances/3/install", body, withHeader("Content-Type", ct)), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances/3/install", `{"apk":"C:\\x.apk"}`), 400, errs.Usage)
	entries, _ := os.ReadDir(filepath.Join(tmp, "modavd-ui"))
	if len(entries) != 0 {
		t.Fatalf("refused uploads left %d entries", len(entries))
	}
}

func TestSafeFileName(t *testing.T) {
	cases := map[string]string{
		"game.apk":                 "game.apk",
		`..\..\evil.apk`:           "evil.apk",
		"a:b?.apk":                 "a_b_.apk",
		"":                         "upload.apk",
		"name.apk. ":               "name.apk",
		"游戏/v2/包.apk":              "包.apk",
		"tab\there.apk":            "tab_here.apk",
		`C:\Users\<user>\game.apk`: "game.apk",
	}
	for in, want := range cases {
		if got := safeFileName(in); got != want {
			t.Errorf("safeFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckCached(t *testing.T) {
	h := newHarness(t, nil)
	var res emu.CheckResult
	expect(t, h.do("GET", "/api/check", nil), 200, &res)
	expect(t, h.do("GET", "/api/check", nil), 200, &res)
	if n := h.f.checks.Load(); n != 1 || !res.Ready {
		t.Fatalf("%d checks, result %+v", n, res)
	}
	expect(t, h.do("GET", "/api/check?refresh=1", nil), 200, &res)
	if n := h.f.checks.Load(); n != 2 {
		t.Fatalf("refresh=1 did not check again (%d)", n)
	}
}
