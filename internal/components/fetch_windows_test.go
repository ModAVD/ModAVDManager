//go:build windows

package components

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

// The whole fetch against local httptest mirrors and small synthetic zips; nothing goes online.

const (
	adbBody     = "adb.exe from the test archive"
	dllBody     = "AdbWinApi.dll from the test archive"
	ptProps     = "Pkg.UserSrc=false\nPkg.Revision=1.0\n"
	imgProps    = "Pkg.Desc=Test image\nPkg.Revision=14\n"
	testLicense = "Terms and Conditions\n\nThis is a test license for the fetch tests."
)

// zipTime is the entries' time: even seconds, as MS-DOS times have them.
var zipTime = time.Date(2025, 3, 4, 5, 6, 8, 0, time.Local)

type zentry struct{ name, body string }

func makeZip(t *testing.T, entries ...zentry) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: zipTime}
		w, err := z.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func ptZip(t *testing.T, adb string, extra ...zentry) []byte {
	t.Helper()
	return makeZip(t, append([]zentry{{"platform-tools/", ""}, {"platform-tools/adb.exe", adb},
		{"platform-tools/AdbWinApi.dll", dllBody}, {"platform-tools/source.properties", ptProps}}, extra...)...)
}

// imgZip has no source.properties: fetch writes it from the table.
func imgZip(t *testing.T) []byte {
	t.Helper()
	return makeZip(t, zentry{"x86_64/system.img", "system image"}, zentry{"x86_64/kernel-ranchu", "kernel"},
		zentry{"x86_64/data/build.prop", "ro.test=1"})
}

func sum1(b []byte) string {
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:])
}

func sum256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ptSpec is platform-tools as in the real table: pinned SHA-256s and an exact revision.
func ptSpec(data []byte) root.SDKComponent {
	return root.SDKComponent{
		ID: "platform-tools", Name: root.PlatformToolsName, FetchName: "平台工具（adb）", Version: "1.0",
		Target: "platform-tools", StatusFiles: []string{"adb.exe"},
		URL: "pt.zip", Size: int64(len(data)), SHA1: sum1(data), ArchiveRoot: "platform-tools",
		KeyFiles:       []string{"adb.exe", "AdbWinApi.dll"},
		KeyHashes:      map[string]string{"adb.exe": sum256(adbBody), "AdbWinApi.dll": sum256(dllBody)},
		ExtractedBytes: 1 << 20, Properties: ptProps,
	}
}

// imgSpec is a system image: no pins, so its revision is not required.
func imgSpec(data []byte) root.SDKComponent {
	return root.SDKComponent{
		ID: "system-image", Name: root.SystemImageName, FetchName: "系统镜像（测试）", Version: "14",
		Target: filepath.Join("system-images", "test", "x86_64"), StatusFiles: []string{"system.img"},
		URL: "img/x86_64-test.zip", Size: int64(len(data)), SHA1: sum1(data), ArchiveRoot: "x86_64",
		KeyFiles: []string{"system.img", "kernel-ranchu"}, ExtractedBytes: 1 << 20, Properties: imgProps,
	}
}

// setup swaps in the test catalogue, a short retry pause and plenty of free space.
func setup(t *testing.T, specs ...root.SDKComponent) {
	t.Helper()
	oldOS, oldCat, oldDelay, oldDisk := fetchGOOS, componentCatalogue, retryDelay, diskFree
	t.Cleanup(func() { fetchGOOS, componentCatalogue, retryDelay, diskFree = oldOS, oldCat, oldDelay, oldDisk })
	fetchGOOS = "windows"
	componentCatalogue = func() []root.SDKComponent { return append([]root.SDKComponent(nil), specs...) }
	retryDelay = 10 * time.Millisecond
	diskFree = func(string) (uint64, error) { return 1 << 40, nil }
}

// tempRoot is a ModAVD root (sdk\emulator\emulator.exe) under $MODAVD_TEST_TMP\components when set, else the OS temp directory;
// sub adds directories under it. Skipped when the base path is not ASCII: fetch refuses such a root.
func tempRoot(t *testing.T, sub ...string) *root.Root {
	t.Helper()
	var base string
	if env := os.Getenv("MODAVD_TEST_TMP"); env != "" {
		d := filepath.Join(env, "components")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		base = testutil.MkdirTemp(t, d, "t-")
	} else {
		base = testutil.TempDir(t)
	}
	if !isASCII(base) {
		t.Skip("临时目录的路径里有非 ASCII 字符，补全组件会拒绝；把 MODAVD_TEST_TMP 设成只含英文的目录再测")
	}
	dir := filepath.Join(append([]string{base}, sub...)...)
	putFile(t, filepath.Join(dir, "sdk", "emulator", "emulator.exe"), "MZ")
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func putFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if errs.CodeOf(err) != code {
		t.Fatalf("want %s, got %v (%s)", code, err, errs.CodeOf(err))
	}
}

// ---- mirrors ---------------------------------------------------------------------------------------------------

type mirrorReq struct{ path, rng, encoding, agent string }

// mirror is an httptest mirror: repository2-3.xml and the zips by base name, served with http.ServeContent (which
// honours Range like a CDN). serve, when set, may answer a zip request itself; serveManifest answers the manifest
// request instead of manifest.
type mirror struct {
	srv            *httptest.Server
	manifest       string
	manifestStatus int
	files          map[string][]byte
	serve          func(w http.ResponseWriter, r *http.Request, data []byte) bool
	serveManifest  func(w http.ResponseWriter, r *http.Request)

	mu   sync.Mutex
	reqs []mirrorReq
	sent int64 // zip body bytes sent by ServeContent
}

func manifestDoc(text string) string {
	return `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>` + "\n" +
		`<sdk:sdk-repository xmlns:sdk="http://example.invalid/sdk/repository">` +
		`<license id="android-sdk-preview-license" type="text">preview terms</license>` +
		`<license id="android-sdk-license" type="text">` + text + `</license></sdk:sdk-repository>`
}

func newMirror(t *testing.T, files map[string][]byte) *mirror {
	m := &mirror{manifest: manifestDoc(testLicense), files: files}
	m.srv = httptest.NewServer(m)
	t.Cleanup(m.srv.Close)
	return m
}

type countWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func (m *mirror) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.reqs = append(m.reqs, mirrorReq{r.URL.Path, r.Header.Get("Range"), r.Header.Get("Accept-Encoding"), r.Header.Get("User-Agent")})
	m.mu.Unlock()
	if strings.HasSuffix(r.URL.Path, "/"+licenseManifest) {
		switch {
		case m.manifestStatus != 0:
			http.Error(w, "no manifest here", m.manifestStatus)
		case m.serveManifest != nil:
			m.serveManifest(w, r)
		default:
			io.WriteString(w, m.manifest)
		}
		return
	}
	data, ok := m.files[path.Base(r.URL.Path)]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if m.serve != nil && m.serve(w, r, data) {
		return
	}
	cw := &countWriter{ResponseWriter: w}
	http.ServeContent(cw, r, path.Base(r.URL.Path), time.Time{}, bytes.NewReader(data))
	m.mu.Lock()
	m.sent += cw.n
	m.mu.Unlock()
}

func (m *mirror) url() string { return m.srv.URL + "/repo/" }

func (m *mirror) all() []mirrorReq {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mirrorReq(nil), m.reqs...)
}

func (m *mirror) zipReqs() []mirrorReq {
	var out []mirrorReq
	for _, q := range m.all() {
		if !strings.HasSuffix(q.path, licenseManifest) {
			out = append(out, q)
		}
	}
	return out
}

func (m *mirror) bytesSent() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sent
}

// rawServer answers every connection with handle; one request per connection. The URL is http://127.0.0.1:<port>/.
func rawServer(t *testing.T, handle func(c net.Conn, req *http.Request)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { ln.Close(); wg.Wait() })
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil {
					return
				}
				handle(c, req)
			})
		}
	})
	return "http://" + ln.Addr().String() + "/"
}

// ---- running a fetch -------------------------------------------------------------------------------------------

type outcome struct {
	res *Result
	err error

	mu       sync.Mutex
	infos    []string
	warns    []string
	notices  strings.Builder
	progress []Progress
}

func (o *outcome) said(s string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, l := range append(append([]string(nil), o.infos...), o.warns...) {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func (o *outcome) warned(s string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, l := range o.warns {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func opts(ms ...*mirror) Options {
	o := Options{AcceptLicense: true}
	for _, m := range ms {
		o.MirrorURLs = append(o.MirrorURLs, m.url())
	}
	return o
}

func fetch(t *testing.T, ctx context.Context, r *root.Root, o Options) *outcome {
	t.Helper()
	out := &outcome{}
	o.Log = logx.New(func(level logx.Level, msg string) {
		out.mu.Lock()
		defer out.mu.Unlock()
		if level >= logx.Warn {
			out.warns = append(out.warns, msg)
		} else {
			out.infos = append(out.infos, msg)
		}
	})
	o.Notice = func(s string) {
		out.mu.Lock()
		out.notices.WriteString(s)
		out.mu.Unlock()
	}
	if o.Progress == nil {
		o.Progress = func(p Progress) {
			out.mu.Lock()
			out.progress = append(out.progress, p)
			out.mu.Unlock()
		}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 5 * time.Second
	}
	if o.StallTimeout == 0 {
		o.StallTimeout = 5 * time.Second
	}
	out.res, out.err = Fetch(ctx, r, o)
	return out
}

func logText(t *testing.T, r *root.Root) string {
	t.Helper()
	return readText(t, filepath.Join(r.LogsDir(), "fetch-components.log"))
}

func downloadsDir(r *root.Root) string { return filepath.Join(r.Dir, "downloads") }

// ---- tests -----------------------------------------------------------------------------------------------------

// TestFetchInstallsThenSkips: a fresh root gets both components (raw bytes asked for, source.properties written for the
// image, zip times kept, license copy kept, zips and sdk\.staging removed); the next run makes no request at all.
func TestFetchInstallsThenSkips(t *testing.T) {
	pt, img := ptZip(t, adbBody), imgZip(t)
	setup(t, ptSpec(pt), imgSpec(img))
	m := newMirror(t, map[string][]byte{"pt.zip": pt, "x86_64-test.zip": img})
	r := tempRoot(t)
	out := fetch(t, nil, r, opts(m))
	if out.err != nil {
		t.Fatal(out.err)
	}
	reqs := m.all()
	if len(reqs) != 3 || len(m.zipReqs()) != 2 {
		t.Fatalf("requests %+v", reqs)
	}
	for _, q := range reqs {
		if q.encoding != "identity" || q.agent != "ModAVDManager/"+buildinfo.Version || q.rng != "" {
			t.Errorf("request %+v", q)
		}
	}
	sdk := r.SDK()
	if got := readText(t, filepath.Join(sdk, "platform-tools", "adb.exe")); got != adbBody {
		t.Errorf("adb.exe = %q", got)
	}
	image := filepath.Join(sdk, "system-images", "test", "x86_64")
	if got := readText(t, filepath.Join(image, "data", "build.prop")); got != "ro.test=1" {
		t.Errorf("build.prop = %q", got)
	}
	if got := readText(t, filepath.Join(image, "source.properties")); got != imgProps || !out.said("压缩包里没有 source.properties，已补写") {
		t.Errorf("source.properties = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(sdk, "platform-tools", "adb.exe")); err != nil || !fi.ModTime().Equal(zipTime) {
		t.Errorf("adb.exe time %v, want %v (%v)", fi.ModTime(), zipTime, err)
	}
	want := "\xef\xbb\xbf" + strings.ReplaceAll(testLicense, "\n", "\r\n") + "\r\n"
	if got := readText(t, filepath.Join(downloadsDir(r), "android-sdk-license.txt")); got != want {
		t.Errorf("license copy %q", got)
	}
	if entries, _ := os.ReadDir(downloadsDir(r)); len(entries) != 1 || len(out.res.Downloads) != 0 {
		t.Errorf("downloads %v, result %v", entries, out.res.Downloads)
	}
	if exists(filepath.Join(sdk, ".staging")) {
		t.Error(`sdk\.staging left behind`)
	}
	res := out.res
	if res.License != testLicense || res.LicenseSource != "自定义镜像（"+m.url()+"）" || res.Root != r.Dir {
		t.Errorf("result %+v", res)
	}
	wantComps := []ComponentResult{
		{ID: "platform-tools", Name: "平台工具（adb）", Version: "1.0", Installed: true, Downloaded: true},
		{ID: "system-image", Name: "系统镜像（测试）", Version: "14", Installed: true, Downloaded: true},
	}
	for k := range wantComps {
		if res.Components[k] != wantComps[k] {
			t.Errorf("component %d: %+v", k, res.Components[k])
		}
	}
	notices := out.notices.String()
	if !strings.Contains(notices, testLicense) || !strings.Contains(notices, "取自 自定义镜像（"+m.url()+"）") ||
		!strings.Contains(notices, "接下来要下载的 平台工具（adb）、系统镜像（测试） 由 Google 提供") ||
		!out.said("已通过 --accept-license 同意许可协议。") {
		t.Errorf("notices %q", notices)
	}
	// where it downloads from is said once, before the license is fetched
	if !out.said("下载源：自定义镜像（" + m.url() + "）（--mirror-url），按顺序试") {
		t.Error("the download source was not said")
	}
	logged := logText(t, r)
	if src, lic := strings.Index(logged, "下载源："), strings.Index(logged, "许可协议另存"); src < 0 || lic < 0 || src > lic {
		t.Errorf("the download source line is not before the license:\n%s", logged)
	}
	for _, s := range []string{"参数：AcceptLicense=true", "  URL " + m.url() + "pt.zip", "下载结束：", "退出码 0",
		"完成，可以在 AUTO-MAS 里添加这个文件夹了。"} {
		if !strings.Contains(logged, s) {
			t.Errorf("log lacks %q:\n%s", s, logged)
		}
	}
	if strings.Contains(logged, "剩余") || strings.Contains(logged, "平均") || strings.Contains(logged, "解压 平台") {
		t.Errorf("progress in the log:\n%s", logged)
	}

	// Again: complete, so no request; the version shown is the one on disk.
	n := len(m.all())
	out = fetch(t, nil, r, opts(m))
	if out.err != nil || len(m.all()) != n {
		t.Fatalf("rerun: %v, %d requests", out.err, len(m.all())-n)
	}
	if !out.res.Components[0].Skipped || !out.res.Components[1].Skipped || out.res.License != "" ||
		!out.said("已有：平台工具（adb）（版本 1.0），跳过") || !out.said("组件都已齐全，不需要下载") || out.said("下载源：") {
		t.Fatalf("rerun %+v", out.res)
	}
	// A system image of another revision is complete; the script only notes it.
	putFile(t, filepath.Join(image, "source.properties"), "Pkg.Revision=13\n")
	out = fetch(t, nil, r, opts(m))
	if out.err != nil || len(m.all()) != n || out.res.Components[1].Version != "13" ||
		!out.said("已有：系统镜像（测试）（版本 13），跳过") || !out.warned("注意：这个版本和本包测试过的 14 不同") {
		t.Fatalf("other revision: %v %+v", out.err, out.res)
	}
}

func TestFetchKeepDownloads(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	o := opts(m)
	o.KeepDownloads = true
	out := fetch(t, nil, r, o)
	if out.err != nil {
		t.Fatal(out.err)
	}
	if !exists(filepath.Join(downloadsDir(r), "pt.zip")) || exists(filepath.Join(downloadsDir(r), "pt.zip.part")) ||
		len(out.res.Downloads) != 1 || out.res.Downloads[0] != "pt.zip" {
		t.Fatalf("downloads %v", out.res.Downloads)
	}
}

// TestFetchResumesPart: a .part from an interrupted run is continued with a Range request; only the rest is sent.
func TestFetchResumesPart(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	half := len(pt) / 2
	putFile(t, filepath.Join(downloadsDir(r), "pt.zip.part"), string(pt[:half]))
	out := fetch(t, nil, r, opts(m))
	if out.err != nil {
		t.Fatal(out.err)
	}
	z := m.zipReqs()
	if len(z) != 1 || z[0].rng != fmt.Sprintf("bytes=%d-", half) || m.bytesSent() != int64(len(pt)-half) {
		t.Fatalf("requests %+v, %d bytes sent", z, m.bytesSent())
	}
	if !out.said("处接着下") {
		t.Error("no resume message")
	}
}

// TestFetchCompletePart: a .part that already has every byte (the run was killed during the SHA-1 or before the
// rename) needs no request; a complete one with the wrong SHA-1 is dropped before trying the first mirror.
func TestFetchCompletePart(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	a := newMirror(t, map[string][]byte{"pt.zip": pt})
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	part := filepath.Join(downloadsDir(r), "pt.zip.part")
	putFile(t, part, string(pt))
	out := fetch(t, nil, r, opts(a, b))
	if out.err != nil || len(a.zipReqs()) != 0 || len(b.zipReqs()) != 0 {
		t.Fatalf("%v: requests %+v %+v", out.err, a.zipReqs(), b.zipReqs())
	}

	r = tempRoot(t)
	part = filepath.Join(downloadsDir(r), "pt.zip.part")
	bad := append([]byte(nil), pt...)
	bad[len(bad)/2] ^= 0xff
	putFile(t, part, string(bad))
	out = fetch(t, nil, r, opts(a, b))
	if out.err != nil {
		t.Fatal(out.err)
	}
	if z := a.zipReqs(); len(b.zipReqs()) != 0 || len(z) != 1 || z[0].rng != "" || !out.warned("SHA-1 不对") {
		t.Fatalf("requests %+v %+v", a.zipReqs(), z)
	}
}

// TestFetch416KeepsPart: a mirror that cannot continue from the .part's size (HTTP 416: its file is shorter) is left
// at once, without a request from 0, and the .part stays as it was for the next mirror to resume (the script's
// 'HTTP 4\d\d'). When no mirror can continue it, the fetch fails with the .part still there and the advice to delete
// it. The connection is not kept: the bytes the mirror sends after its answer must not make net/http log "Unsolicited
// response" on stderr (seen with a real mirror, in the middle of the --json lines).
func TestFetch416KeepsPart(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	var mu sync.Mutex
	var ranges []string
	short := rawServer(t, func(c net.Conn, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, licenseManifest) {
			doc := manifestDoc(testLicense)
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(doc), doc)
			return
		}
		mu.Lock()
		ranges = append(ranges, req.Header.Get("Range"))
		mu.Unlock()
		if req.Header.Get("Range") != "" {
			io.WriteString(c, "HTTP/1.1 416 Range Not Satisfiable\r\nContent-Length: 0\r\n\r\nPK\x03\x04 stray bytes after the answer")
			time.Sleep(300 * time.Millisecond)
			return
		}
		// the short file a mirror answering 416 has; never asked for
		fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", 8)
		c.Write(pt[:8])
	})
	var logged lockedBuffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	good := newMirror(t, map[string][]byte{"pt.zip": pt})

	r := tempRoot(t)
	putFile(t, filepath.Join(downloadsDir(r), "pt.zip.part"), string(pt[:10]))
	out := fetch(t, nil, r, Options{AcceptLicense: true, MirrorURLs: []string{short, good.url()}})
	time.Sleep(200 * time.Millisecond) // a pooled connection would have read the stray bytes by now
	if out.err != nil {
		t.Fatal(out.err)
	}
	mu.Lock()
	if len(ranges) != 1 || ranges[0] != "bytes=10-" || !out.warned("HTTP 416") {
		t.Fatalf("ranges %q, warnings %q", ranges, out.warns)
	}
	ranges = nil
	mu.Unlock()
	if z := good.zipReqs(); len(z) != 1 || z[0].rng != "bytes=10-" || good.bytesSent() != int64(len(pt)-10) {
		t.Fatalf("second mirror: requests %+v, %d bytes sent", z, good.bytesSent())
	}
	if got := readText(t, filepath.Join(r.SDK(), "platform-tools", "adb.exe")); got != adbBody {
		t.Fatalf("adb.exe = %q", got)
	}

	// Only the short mirror: no run gets past it, the part is kept untouched and the advice says what to delete.
	r = tempRoot(t)
	part := filepath.Join(downloadsDir(r), "pt.zip.part")
	putFile(t, part, string(pt[:10]))
	out = fetch(t, nil, r, Options{AcceptLicense: true, MirrorURLs: []string{short}})
	time.Sleep(200 * time.Millisecond)
	wantCode(t, out.err, errs.ComponentsUnavailable)
	if e := errs.As(out.err); !strings.Contains(e.Message, "HTTP 416") || !strings.Contains(e.Advice, "删掉 "+part) {
		t.Fatalf("error %q, advice %q", e.Message, e.Advice)
	}
	if got := readText(t, part); got != string(pt[:10]) {
		t.Fatalf(".part = %q, want the 10 bytes it had", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 1 || ranges[0] != "bytes=10-" {
		t.Fatalf("ranges %q: the short mirror was asked again", ranges)
	}
	if s := logged.String(); strings.Contains(s, "Unsolicited") {
		t.Fatalf("net/http logged: %s", s)
	}
}

// TestFetchResumeAnswered200: a mirror that ignores the Range of a resume and sends the whole file with 200 gets it
// written from 0 over the .part (the script's "这个镜像不支持断点续传，从头下载"), counted from 0.
func TestFetchResumeAnswered200(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	m.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return true
	}
	r := tempRoot(t)
	// not the start of pt: appended to, or counted as already there, the result would be wrong
	putFile(t, filepath.Join(downloadsDir(r), "pt.zip.part"), strings.Repeat("x", 10))
	out := fetch(t, nil, r, opts(m))
	if out.err != nil {
		t.Fatal(out.err)
	}
	if z := m.zipReqs(); len(z) != 1 || z[0].rng != "bytes=10-" || !out.warned("这个镜像不支持断点续传，从头下载") {
		t.Fatalf("requests %+v, warnings %q", z, out.warns)
	}
	if got := readText(t, filepath.Join(r.SDK(), "platform-tools", "adb.exe")); got != adbBody {
		t.Fatalf("adb.exe = %q", got)
	}
	if !strings.Contains(logText(t, r), fmt.Sprintf("下载开始：从第 0 字节起，共 %d 字节", len(pt))) {
		t.Fatal("the log does not say the download started from 0")
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestFetchSHA1Fallback: a mirror serving a file of the right size but the wrong SHA-1 is dropped after one try and
// the next mirror used; when every mirror does that, components_rejected.
func TestFetchSHA1Fallback(t *testing.T) {
	pt := ptZip(t, adbBody)
	bad := append([]byte(nil), pt...)
	bad[len(bad)/3] ^= 0x55
	setup(t, ptSpec(pt))
	a := newMirror(t, map[string][]byte{"pt.zip": bad})
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	out := fetch(t, nil, r, opts(a, b))
	if out.err != nil {
		t.Fatal(out.err)
	}
	if len(a.zipReqs()) != 1 || len(b.zipReqs()) != 1 || !out.warned("SHA-1 不对") ||
		readText(t, filepath.Join(r.SDK(), "platform-tools", "adb.exe")) != adbBody ||
		exists(filepath.Join(downloadsDir(r), "pt.zip.part")) {
		t.Fatalf("requests %d %d", len(a.zipReqs()), len(b.zipReqs()))
	}
	out = fetch(t, nil, tempRoot(t), opts(a))
	wantCode(t, out.err, errs.ComponentsRejected)
	if errs.ExitCode(out.err) != 9 || !strings.Contains(out.err.Error(), "校验：SHA-1 为") {
		t.Fatal(out.err)
	}
}

// TestFetchRefusesCompressedContent: a gzip answer is refused at once and the next mirror used.
func TestFetchRefusesCompressedContent(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	a := newMirror(t, map[string][]byte{"pt.zip": pt})
	a.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write(data)
		gz.Close()
		return true
	}
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	out := fetch(t, nil, r, opts(a, b))
	if out.err != nil {
		t.Fatal(out.err)
	}
	if len(a.zipReqs()) != 1 || len(b.zipReqs()) != 1 || !out.warned("压缩过的内容（Content-Encoding: gzip）") {
		t.Fatalf("requests %d %d, warnings %q", len(a.zipReqs()), len(b.zipReqs()), out.warns)
	}
}

// TestFetchLicenseQuestion: without --accept-license the license and the question are shown and y / yes (any case)
// is read; anything else downloads nothing and leaves no downloads\.
func TestFetchLicenseQuestion(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	for _, c := range []struct {
		input string
		ok    bool
	}{{"n\n", false}, {"", false}, {"no\r\n", false}, {"y\r\n", true}, {" YES \n", true}} {
		m := newMirror(t, map[string][]byte{"pt.zip": pt})
		r := tempRoot(t)
		o := opts(m)
		o.AcceptLicense, o.Input = false, strings.NewReader(c.input)
		out := fetch(t, nil, r, o)
		notices := out.notices.String()
		if !strings.Contains(notices, testLicense) || !strings.Contains(notices, "是否同意这份许可协议？输入 y 并回车表示同意") {
			t.Errorf("%q: notices %q", c.input, notices)
		}
		if !strings.Contains(logText(t, r), "用户输入："+strings.TrimRight(c.input, "\r\n")) {
			t.Errorf("%q: answer not logged", c.input)
		}
		if c.ok {
			if out.err != nil || !out.said("已同意许可协议。") {
				t.Errorf("%q: %v", c.input, out.err)
			}
			continue
		}
		if errs.CodeOf(out.err) != errs.LicenseDeclined || errs.ExitCode(out.err) != 2 || len(m.zipReqs()) != 0 ||
			exists(downloadsDir(r)) {
			t.Errorf("%q: %v, %d zip requests, downloads %v", c.input, out.err, len(m.zipReqs()), exists(downloadsDir(r)))
		}
	}
}

// TestFetchJSONNeedsAcceptLicense: --json without --accept-license is refused before going online; with nothing to
// download it is fine.
func TestFetchJSONNeedsAcceptLicense(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	o := opts(m)
	o.AcceptLicense, o.JSON = false, true
	out := fetch(t, nil, r, o)
	wantCode(t, out.err, errs.Usage)
	if len(m.all()) != 0 || exists(downloadsDir(r)) || !strings.Contains(out.err.Error(), "--accept-license") {
		t.Fatalf("%d requests", len(m.all()))
	}
	if out := fetch(t, nil, r, opts(m)); out.err != nil {
		t.Fatal(out.err)
	}
	// a root that lacks nothing asks no question, so this layer lets it through (the CLI refuses --json without
	// --accept-license before it gets here: cli.TestComponentsFetchJSONNeedsAcceptLicense)
	n := len(m.all())
	if out := fetch(t, nil, r, o); out.err != nil || len(m.all()) != n {
		t.Fatalf("complete root: %v", out.err)
	}
}

func TestFetchRejectsChangedLicenseHashBeforeDownload(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	o := opts(m)
	o.LicenseSHA256 = strings.Repeat("0", 64)
	out := fetch(t, nil, r, o)
	wantCode(t, out.err, errs.ComponentsRejected)
	if !strings.Contains(errs.As(out.err).Advice, "补全组件") || out.said("同意许可协议") {
		t.Errorf("error %+v, said %q", errs.As(out.err), out.infos)
	}
	if len(m.zipReqs()) != 0 || exists(downloadsDir(r)) {
		t.Fatalf("hash mismatch downloaded files: zip requests=%d downloads=%v", len(m.zipReqs()), exists(downloadsDir(r)))
	}
}

// The manager's consent is to the text its dialog showed, read by License from the source chosen there: with that
// text's SHA-256 the fetch goes on and says the consent was given in the manager (not --accept-license) and where
// the source was chosen.
func TestFetchWithManagerLicenseHash(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	text, from, err := License(context.Background(), SourceSettings{MirrorURLs: []string{m.url()}})
	if err != nil || text != testLicense || from != "自定义镜像（"+m.url()+"）" {
		t.Fatalf("License: %q from %q, %v", text, from, err)
	}
	r := tempRoot(t)
	o := opts(m)
	o.LicenseSHA256 = strings.ToUpper(LicenseSHA256(text))
	o.SourceOrigin = SourceFromManager
	out := fetch(t, nil, r, o)
	if out.err != nil {
		t.Fatal(out.err)
	}
	if !out.said("已在管理器里同意许可协议。") || out.said("--accept-license 同意") || !out.said("（管理器里选的），按顺序试") {
		t.Errorf("said %q", out.infos)
	}
}

// TestFetchUsesProxy: every request of a fetch (manifest and zip) really goes through the proxy hostproxy picks, here
// one named by HTTP_PROXY. net/http reads the proxy variables once per process, so the fetch runs in a child
// (TestFetchUsesProxyChild) whose first request is that fetch's.
func TestFetchUsesProxy(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestFetchUsesProxyChild$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), fetchProxyChildEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "--- PASS: TestFetchUsesProxyChild") {
		t.Fatalf("child: %v\n%s", err, out)
	}
}

const fetchProxyChildEnv = "MODAVD_TEST_FETCH_PROXY_CHILD"

// TestFetchUsesProxyChild is TestFetchUsesProxy's child. The mirror's host does not resolve, so the fetch can only
// succeed when every request is sent to the proxy, in proxy form.
func TestFetchUsesProxyChild(t *testing.T) {
	if os.Getenv(fetchProxyChildEnv) == "" {
		t.Skip("run by TestFetchUsesProxy")
	}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := &mirror{manifest: manifestDoc(testLicense), files: map[string][]byte{"pt.zip": pt}}
	var mu sync.Mutex
	var seen []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, fmt.Sprintf("%s abs=%t", r.Host, r.URL.IsAbs()))
		mu.Unlock()
		m.ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	t.Setenv("HTTP_PROXY", proxy.URL) // before the first request of this process

	r := tempRoot(t)
	out := fetch(t, nil, r, Options{AcceptLicense: true, MirrorURLs: []string{"http://mirror.invalid/repo/"}})
	if out.err != nil {
		t.Fatal(out.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "mirror.invalid abs=true" || seen[1] != "mirror.invalid abs=true" {
		t.Fatalf("requests at the proxy: %q", seen)
	}
	if got := readText(t, filepath.Join(r.SDK(), "platform-tools", "adb.exe")); got != adbBody {
		t.Fatalf("adb.exe = %q", got)
	}
}

// TestFetchManifestLimits: the license manifest as a whole gets ManifestTimeout even while every read brings
// something (a mirror dribbling it), and is refused past maxManifestBytes but taken at exactly that size; a refused
// manifest means no zip is asked for.
func TestFetchManifestLimits(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	doc := manifestDoc(testLicense)

	slow := newMirror(t, map[string][]byte{"pt.zip": pt})
	slow.serveManifest = func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		for k := range len(doc) {
			if _, err := io.WriteString(w, doc[k:k+1]); err != nil || r.Context().Err() != nil {
				return
			}
			fl.Flush()
			time.Sleep(20 * time.Millisecond) // never a stall, but the whole takes about 8 s
		}
	}
	o := opts(slow)
	o.ManifestTimeout = time.Second
	started := time.Now()
	out := fetch(t, nil, tempRoot(t), o)
	wantCode(t, out.err, errs.ComponentsUnavailable)
	if !strings.Contains(out.err.Error(), "整个过程超过") || time.Since(started) > 4*time.Second || len(slow.zipReqs()) != 0 {
		t.Fatalf("after %v: %v, %d zip requests", time.Since(started), out.err, len(slow.zipReqs()))
	}

	padded := func(n int) func(w http.ResponseWriter, r *http.Request) {
		// trailing white space after the root element is still a well-formed document
		return func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, doc)
			io.WriteString(w, strings.Repeat(" ", n-len(doc)))
		}
	}
	big := newMirror(t, map[string][]byte{"pt.zip": pt})
	big.serveManifest = padded(maxManifestBytes + 1)
	out = fetch(t, nil, tempRoot(t), opts(big))
	wantCode(t, out.err, errs.ComponentsUnavailable)
	if !strings.Contains(out.err.Error(), fmt.Sprintf("大得不正常（超过 %d MB）", maxManifestBytes>>20)) || len(big.zipReqs()) != 0 {
		t.Fatalf("%v, %d zip requests", out.err, len(big.zipReqs()))
	}

	full := newMirror(t, map[string][]byte{"pt.zip": pt})
	full.serveManifest = padded(maxManifestBytes)
	if out = fetch(t, nil, tempRoot(t), opts(full)); out.err != nil || out.res.License != testLicense {
		t.Fatalf("a manifest of exactly %d bytes: %v", maxManifestBytes, out.err)
	}
}

// TestFetchPinnedHashes: a zip whose adb.exe is not the pinned one is refused before anything moves; an installed
// adb.exe that is not the pinned one is not "complete" and gets replaced (the old one set aside).
func TestFetchPinnedHashes(t *testing.T) {
	tampered := ptZip(t, "a different adb.exe")
	spec := ptSpec(tampered) // the zip's own SHA-1, but the pins of the real adb
	setup(t, spec)
	m := newMirror(t, map[string][]byte{"pt.zip": tampered})
	r := tempRoot(t)
	out := fetch(t, nil, r, opts(m))
	wantCode(t, out.err, errs.ComponentsRejected)
	if !strings.Contains(out.err.Error(), "解压后的关键文件或版本校验失败") || exists(filepath.Join(r.SDK(), "platform-tools")) ||
		exists(filepath.Join(r.SDK(), ".staging")) {
		t.Fatal(out.err)
	}

	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m = newMirror(t, map[string][]byte{"pt.zip": pt})
	r = tempRoot(t)
	dir := filepath.Join(r.SDK(), "platform-tools")
	putFile(t, filepath.Join(dir, "adb.exe"), "a different adb.exe")
	putFile(t, filepath.Join(dir, "AdbWinApi.dll"), dllBody)
	putFile(t, filepath.Join(dir, "source.properties"), ptProps)
	out = fetch(t, nil, r, opts(m))
	if out.err != nil || out.res.Components[0].Skipped || len(m.zipReqs()) != 1 || readText(t, filepath.Join(dir, "adb.exe")) != adbBody {
		t.Fatalf("%v %+v", out.err, out.res)
	}
	aside := filepath.Join(r.Dir, out.res.Components[0].SetAside)
	if out.res.Components[0].SetAside == "" || readText(t, filepath.Join(aside, "adb.exe")) != "a different adb.exe" {
		t.Fatalf("set aside %q", out.res.Components[0].SetAside)
	}
}

// TestFetchSetsAsideIncompleteDir: an incomplete target is renamed to <target>.incomplete-<time> and kept with
// everything in it, never deleted; a second one gets its own name.
func TestFetchSetsAsideIncompleteDir(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	dir := filepath.Join(r.SDK(), "platform-tools")
	var asides []string
	for k := range 2 {
		putFile(t, filepath.Join(dir, "adb.exe"), "old adb")
		putFile(t, filepath.Join(dir, "my-notes.txt"), fmt.Sprint("notes ", k))
		os.Remove(filepath.Join(dir, "AdbWinApi.dll"))
		out := fetch(t, nil, r, opts(m))
		if out.err != nil {
			t.Fatal(out.err)
		}
		set := out.res.Components[0].SetAside
		if !strings.HasPrefix(set, `sdk\platform-tools.incomplete-`) || !out.warned("已改名为") {
			t.Fatalf("run %d: set aside %q", k, set)
		}
		asides = append(asides, set)
		if readText(t, filepath.Join(dir, "adb.exe")) != adbBody || exists(filepath.Join(dir, "my-notes.txt")) {
			t.Fatalf("run %d: target not replaced", k)
		}
	}
	for k, a := range asides {
		if got := readText(t, filepath.Join(r.Dir, a, "my-notes.txt")); got != fmt.Sprint("notes ", k) {
			t.Errorf("%s: %q", a, got)
		}
	}
	if asides[0] == asides[1] {
		t.Fatal("same name twice")
	}
}

// TestFetchPutsOldDirBack: when the new directory cannot be moved in (a file in it is held open here, without delete
// sharing), the old one that was set aside goes back to its place with everything in it.
func TestFetchPutsOldDirBack(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	dir := filepath.Join(r.SDK(), "platform-tools")
	putFile(t, filepath.Join(dir, "my-notes.txt"), "keep me")
	var held *os.File
	t.Cleanup(func() {
		if held != nil {
			held.Close()
		}
	})
	o := opts(m)
	o.Progress = func(p Progress) {
		if p.Stage == "extracting" && p.Final && held == nil {
			held, _ = os.Open(filepath.Join(r.SDK(), ".staging", "platform-tools", "platform-tools", "adb.exe"))
		}
	}
	out := fetch(t, nil, r, o)
	if held == nil {
		t.Fatal("the staged adb.exe was not there to hold")
	}
	wantCode(t, out.err, errs.Internal)
	if !strings.Contains(out.err.Error(), `挪不进 sdk\platform-tools`) || !out.warned(`已把 platform-tools.incomplete-`) {
		t.Fatalf("%v, warnings %q", out.err, out.warns)
	}
	if readText(t, filepath.Join(dir, "my-notes.txt")) != "keep me" || exists(filepath.Join(dir, "adb.exe")) {
		t.Fatal("the old directory is not back")
	}
	entries, _ := os.ReadDir(r.SDK())
	for _, e := range entries {
		if strings.Contains(e.Name(), ".incomplete-") {
			t.Fatalf("%s left set aside", e.Name())
		}
	}
}

// TestFetchChecksStagedFiles: a zip without a key file, or with an entry outside the archive root, is refused while
// still in sdk\.staging: the existing directory is left as it was, nothing half-installed, no staging left.
func TestFetchChecksStagedFiles(t *testing.T) {
	for _, c := range []struct {
		name string
		zip  func(t *testing.T) []byte
		msg  string
	}{
		{"missing key file", func(t *testing.T) []byte {
			return makeZip(t, zentry{"platform-tools/adb.exe", adbBody}, zentry{"platform-tools/source.properties", ptProps})
		}, "压缩包里缺少 AdbWinApi.dll"},
		{"outside the root", func(t *testing.T) []byte { return ptZip(t, adbBody, zentry{"other/readme.txt", "x"}) },
			"压缩包结构不对：有不在 platform-tools/ 下的条目"},
	} {
		t.Run(c.name, func(t *testing.T) {
			data := c.zip(t)
			setup(t, ptSpec(data))
			m := newMirror(t, map[string][]byte{"pt.zip": data})
			r := tempRoot(t)
			dir := filepath.Join(r.SDK(), "platform-tools")
			putFile(t, filepath.Join(dir, "my-notes.txt"), "keep me")
			out := fetch(t, nil, r, opts(m))
			wantCode(t, out.err, errs.ComponentsRejected)
			if !strings.Contains(out.err.Error(), c.msg) {
				t.Fatal(out.err)
			}
			entries, _ := os.ReadDir(r.SDK())
			if len(entries) != 2 || readText(t, filepath.Join(dir, "my-notes.txt")) != "keep me" || exists(filepath.Join(dir, "adb.exe")) {
				t.Fatalf("sdk now %v", entries)
			}
			if !exists(filepath.Join(downloadsDir(r), "pt.zip")) {
				t.Error("the verified zip should stay for the next run")
			}
		})
	}
}

// TestFetchRefusesZipSlip: an entry that would land outside sdk\.staging is refused and never written.
func TestFetchRefusesZipSlip(t *testing.T) {
	for _, name := range []string{"platform-tools/../../../../evil.txt", `platform-tools\..\..\..\evil.txt`} {
		data := ptZip(t, adbBody, zentry{name, "evil"})
		setup(t, ptSpec(data))
		m := newMirror(t, map[string][]byte{"pt.zip": data})
		r := tempRoot(t)
		out := fetch(t, nil, r, opts(m))
		wantCode(t, out.err, errs.ComponentsRejected)
		if !strings.Contains(out.err.Error(), "压缩包里有非法路径") {
			t.Fatalf("%s: %v", name, out.err)
		}
		for _, p := range []string{filepath.Join(r.Dir, "evil.txt"), filepath.Join(r.SDK(), "evil.txt"),
			filepath.Join(filepath.Dir(r.Dir), "evil.txt"), filepath.Join(r.SDK(), ".staging", "evil.txt")} {
			if exists(p) {
				t.Fatalf("%s written", p)
			}
		}
	}
}

// TestFetchClearsStaleStaging: what a killed run left in sdk\.staging goes, also when nothing has to be downloaded.
func TestFetchClearsStaleStaging(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	stale := filepath.Join(r.SDK(), ".staging", "system-image", "system.img")
	for k := range 2 {
		putFile(t, stale, "half")
		out := fetch(t, nil, r, opts(m))
		if out.err != nil {
			t.Fatal(out.err)
		}
		if exists(filepath.Join(r.SDK(), ".staging")) {
			t.Fatalf("run %d: sdk\\.staging still there", k)
		}
	}
	if !strings.Contains(logText(t, r), `清理了上次遗留的 sdk\.staging`) {
		t.Error("not logged")
	}
}

// TestFetchRefusesNonASCIIPath: before any request or download.
func TestFetchRefusesNonASCIIPath(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t, "模拟器")
	out := fetch(t, nil, r, opts(m))
	wantCode(t, out.err, errs.PrecheckFailed)
	if errs.ExitCode(out.err) != 4 || len(m.all()) != 0 || exists(downloadsDir(r)) ||
		!strings.Contains(out.err.Error(), "例如 D:\\ModAVD") {
		t.Fatal(out.err)
	}
}

// TestFetchFreeSpace: --test-free-space-mb 0 and 1 refuse (0 is a value, not "unset"), as does too little real space;
// a free space that cannot be read stops the run instead of being skipped. All before any request.
func TestFetchFreeSpace(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	for _, mb := range []int64{0, 1, 500} {
		o := opts(m)
		o.TestFreeSpaceMB = &mb
		out := fetch(t, nil, r, o)
		wantCode(t, out.err, errs.PrecheckFailed)
		if !strings.Contains(out.err.Error(), "磁盘空间不够") || !strings.Contains(out.err.Error(), "请换一个空间更大的盘") {
			t.Fatal(out.err)
		}
	}
	diskFree = func(string) (uint64, error) { return 100 << 20, nil }
	wantCode(t, fetch(t, nil, r, opts(m)).err, errs.PrecheckFailed)
	diskFree = func(string) (uint64, error) { return 0, errs.New(errs.Internal, "GetDiskFreeSpaceExW 失败") }
	out := fetch(t, nil, r, opts(m))
	wantCode(t, out.err, errs.Internal)
	if !strings.Contains(out.err.Error(), "剩余空间") {
		t.Fatal(out.err)
	}
	if len(m.all()) != 0 || exists(downloadsDir(r)) {
		t.Fatalf("%d requests", len(m.all()))
	}
	// Like the script, the real free space is read even when a test value replaces it.
	diskFree = func(string) (uint64, error) { return 1 << 20, nil }
	big := int64(1 << 20)
	o := opts(m)
	o.TestFreeSpaceMB = &big
	if out := fetch(t, nil, r, o); out.err != nil {
		t.Fatal(out.err)
	}
}

// TestFetchRefusesOtherFolders: not a ModAVD root (no logs\ written there), and a root whose sdkRoot points elsewhere
// (check looks there; fetch only writes sdk\).
func TestFetchRefusesOtherFolders(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	os.RemoveAll(r.SDK())
	out := fetch(t, nil, r, opts(m))
	wantCode(t, out.err, errs.NotReady)
	if exists(r.LogsDir()) || len(m.all()) != 0 {
		t.Fatal("wrote into a folder that is not a root")
	}
	r = tempRoot(t)
	putFile(t, r.MetadataPath(), `{"sdkRoot": "devsdk"}`)
	out = fetch(t, nil, r, opts(m))
	wantCode(t, out.err, errs.NotReady)
	if !strings.Contains(out.err.Error(), "sdkRoot") || len(m.all()) != 0 {
		t.Fatal(out.err)
	}
}

// TestFetchBusy: a second fetch of the same root stops before touching sdk\.staging.
func TestFetchBusy(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	l, ok, err := win.TryLock(filepath.Join(r.LogsDir(), "fetch-components.lock"))
	if err != nil || !ok {
		t.Fatal(err)
	}
	defer l.Unlock()
	busy := filepath.Join(r.SDK(), ".staging", "platform-tools", "adb.exe")
	putFile(t, busy, "being unpacked")
	out := fetch(t, nil, r, opts(m))
	wantCode(t, out.err, errs.Busy)
	if !exists(busy) || len(m.all()) != 0 {
		t.Fatal("touched the other run's staging")
	}
}

// TestFetchRetryRules: other errors (HTTP 500) get 3 tries on a mirror with a pause; HTTP 4xx goes to the next mirror
// at once.
func TestFetchRetryRules(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	for status, tries := range map[int]int{500: 3, 404: 1} {
		a := newMirror(t, map[string][]byte{"pt.zip": pt})
		a.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
			http.Error(w, "nope", status)
			return true
		}
		b := newMirror(t, map[string][]byte{"pt.zip": pt})
		out := fetch(t, nil, tempRoot(t), opts(a, b))
		if out.err != nil || len(a.zipReqs()) != tries || len(b.zipReqs()) != 1 || !out.warned(fmt.Sprintf("HTTP %d", status)) {
			t.Errorf("HTTP %d: %v, %d tries", status, out.err, len(a.zipReqs()))
		}
		if status == 500 && !out.said("3 秒后重试") {
			t.Error("no retry message")
		}
	}
}

// TestFetchChecksAnswerBeforePart: a 200 of the wrong length (a captive portal), a 206 from the wrong offset or of the
// wrong total never touch the .part; the next mirror resumes it from where it was.
func TestFetchChecksAnswerBeforePart(t *testing.T) {
	pt := ptZip(t, adbBody)
	half := len(pt) / 2
	setup(t, ptSpec(pt))
	for _, c := range []struct {
		name  string
		serve func(w http.ResponseWriter)
		tries int
		msg   string
	}{
		{"portal", func(w http.ResponseWriter) { io.WriteString(w, "<html>captive portal</html>") }, 1, "文件大小和预期不符（服务器给 27 字节"},
		{"wrong start", func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(pt)-1, len(pt)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(pt[5:])
		}, 3, "断点续传的回应不对（HTTP 206"},
		{"wrong total", func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(pt), len(pt)+1))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(pt[half:])
		}, 1, "文件大小和预期不符（Content-Range:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a := newMirror(t, map[string][]byte{"pt.zip": pt})
			a.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool { c.serve(w); return true }
			b := newMirror(t, map[string][]byte{"pt.zip": pt})
			r := tempRoot(t)
			putFile(t, filepath.Join(downloadsDir(r), "pt.zip.part"), string(pt[:half]))
			out := fetch(t, nil, r, opts(a, b))
			if out.err != nil {
				t.Fatal(out.err)
			}
			z := b.zipReqs()
			if len(a.zipReqs()) != c.tries || len(z) != 1 || z[0].rng != fmt.Sprintf("bytes=%d-", half) || !out.warned(c.msg) {
				t.Fatalf("%d tries, next mirror %+v, warnings %q", len(a.zipReqs()), z, out.warns)
			}
		})
	}
}

// TestFetchStallTimeout: a mirror that stops sending is given up after the stall timeout, at once (no retry); the next
// mirror resumes what came.
func TestFetchStallTimeout(t *testing.T) {
	pt := ptZip(t, adbBody)
	half := len(pt) / 2
	setup(t, ptSpec(pt))
	a := newMirror(t, map[string][]byte{"pt.zip": pt})
	a.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		w.Write(data[:half])
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		return true
	}
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	o := opts(a, b)
	o.StallTimeout = 200 * time.Millisecond
	start := time.Now()
	out := fetch(t, nil, tempRoot(t), o)
	if out.err != nil {
		t.Fatal(out.err)
	}
	z := b.zipReqs()
	if len(a.zipReqs()) != 1 || len(z) != 1 || z[0].rng != fmt.Sprintf("bytes=%d-", half) ||
		!out.warned("（收数据，已下 ") || !out.warned("0.2 秒内没有进展") || time.Since(start) > 5*time.Second {
		t.Fatalf("%d tries, %+v, %q, %v", len(a.zipReqs()), z, out.warns, time.Since(start))
	}
}

// TestFetchConnectTimeout: connect, TLS handshake and response headers together get --connect-timeout, for the
// manifest and for a zip, and then the next mirror is used.
func TestFetchConnectTimeout(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	hold := func(c net.Conn, req *http.Request) { time.Sleep(3 * time.Second) }
	// A server that never answers TLS: the manifest times out and that mirror goes last.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns sync.WaitGroup
	t.Cleanup(func() { ln.Close(); conns.Wait() })
	conns.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Go(func() { defer c.Close(); time.Sleep(3 * time.Second) })
		}
	})
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	o := Options{AcceptLicense: true, MirrorURLs: []string{"https://" + ln.Addr().String() + "/", b.url()},
		ConnectTimeout: 300 * time.Millisecond}
	start := time.Now()
	out := fetch(t, nil, tempRoot(t), o)
	if out.err != nil || !out.warned("取许可协议清单 repository2-3.xml（等服务器回应），0.3 秒内没有进展") ||
		!out.said("排到最后") || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("tls: %v %q %v", out.err, out.warns, time.Since(start))
	}
	// A server that sends the manifest but never answers for the zip.
	doc := manifestDoc(testLicense)
	slow := rawServer(t, func(c net.Conn, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, licenseManifest) {
			fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(doc), doc)
			return
		}
		hold(c, req)
	})
	b2 := newMirror(t, map[string][]byte{"pt.zip": pt})
	o = Options{AcceptLicense: true, MirrorURLs: []string{slow, b2.url()}, ConnectTimeout: 300 * time.Millisecond}
	start = time.Now()
	out = fetch(t, nil, tempRoot(t), o)
	if out.err != nil || !out.warned("下载 平台工具（adb）（等服务器回应），0.3 秒内没有进展") || len(b2.zipReqs()) != 1 ||
		time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("zip: %v %q %v", out.err, out.warns, time.Since(start))
	}
}

// TestFetchLicenseUnavailable: no mirror serves the license (an HTTP error, a page that is not the manifest):
// components_unavailable with every mirror's reason, nothing downloaded.
func TestFetchLicenseUnavailable(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	a := newMirror(t, map[string][]byte{"pt.zip": pt})
	a.manifestStatus = http.StatusNotFound
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	b.manifest = "<html>captive portal</html>"
	r := tempRoot(t)
	out := fetch(t, nil, r, opts(a, b))
	wantCode(t, out.err, errs.ComponentsUnavailable)
	msg := out.err.Error()
	if errs.ExitCode(out.err) != 9 || !strings.Contains(msg, "HTTP 404") || !strings.Contains(msg, "清单里没有许可协议") ||
		len(a.zipReqs())+len(b.zipReqs()) != 0 || exists(downloadsDir(r)) || !out.warned("取许可协议失败") {
		t.Fatal(msg)
	}
	logged := logText(t, r)
	if !strings.Contains(logged, "原因：取不到《Android SDK 许可协议》全文") || !strings.Contains(logged, "退出码 9") {
		t.Fatalf("log:\n%s", logged)
	}
}

// TestFetchLicenseMirrorGoesLast: a mirror that could not give the manifest is tried last for the zips.
func TestFetchLicenseMirrorGoesLast(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	a := newMirror(t, map[string][]byte{"pt.zip": pt})
	a.manifestStatus = http.StatusInternalServerError
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	out := fetch(t, nil, tempRoot(t), opts(a, b))
	if out.err != nil || len(a.zipReqs()) != 0 || len(b.zipReqs()) != 1 || !out.said("自定义镜像（"+a.url()+"） 排到最后") {
		t.Fatalf("%v: %d %d", out.err, len(a.zipReqs()), len(b.zipReqs()))
	}
}

// TestFetchProgressThrottled: progress at most every 500 ms (plus a first and a final report), never in the log.
func TestFetchProgressThrottled(t *testing.T) {
	big := make([]byte, 6<<20)
	rand.New(rand.NewSource(1)).Read(big)
	data := makeZip(t, zentry{"platform-tools/adb.exe", adbBody}, zentry{"platform-tools/AdbWinApi.dll", dllBody},
		zentry{"platform-tools/source.properties", ptProps}, zentry{"platform-tools/lib/big.bin", string(big)})
	setup(t, ptSpec(data))
	m := newMirror(t, map[string][]byte{"pt.zip": data})
	m.serve = func(w http.ResponseWriter, r *http.Request, d []byte) bool {
		w.Header().Set("Content-Length", fmt.Sprint(len(d)))
		for k := 0; k < len(d); k += 16 << 10 {
			w.Write(d[k:min(k+16<<10, len(d))])
			w.(http.Flusher).Flush()
			time.Sleep(time.Millisecond)
		}
		return true
	}
	r := tempRoot(t)
	start := time.Now()
	out := fetch(t, nil, r, opts(m))
	elapsed := time.Since(start)
	if out.err != nil {
		t.Fatal(out.err)
	}
	var downloading, finals int
	for _, p := range out.progress {
		if p.Stage == "downloading" {
			downloading++
		}
		if p.Final {
			finals++
		}
	}
	if limit := 2 + int(elapsed/progressEvery); downloading > limit || downloading < 2 || finals != 2 {
		t.Fatalf("%d download reports in %v (limit %d), %d final", downloading, elapsed, limit, finals)
	}
	if strings.Contains(logText(t, r), "剩余") {
		t.Fatal("progress in the log")
	}
}

// TestFetchCanceledWhileUnpacking: Ctrl+C in the middle of an entry (the 4 GB system.img is one) stops the unpack
// there: no component directory, no staging left, the verified zip kept for the next run.
func TestFetchCanceledWhileUnpacking(t *testing.T) {
	big := make([]byte, 3<<20)
	rand.New(rand.NewSource(2)).Read(big)
	small := len(adbBody) + len(dllBody) + len(ptProps)
	// The big entry comes last: only a check between two reads can stop it.
	pt := ptZip(t, adbBody, zentry{"platform-tools/lib/big.bin", string(big)})
	setup(t, ptSpec(pt))
	old := progressEvery
	progressEvery = 0
	t.Cleanup(func() { progressEvery = old })
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := opts(m)
	o.Progress = func(p Progress) {
		if p.Stage == "extracting" && p.Done > int64(small) {
			cancel()
		}
	}
	out := fetch(t, ctx, r, o)
	wantCode(t, out.err, errs.Canceled)
	if exists(filepath.Join(r.SDK(), "platform-tools")) || exists(filepath.Join(r.SDK(), ".staging")) ||
		!exists(filepath.Join(downloadsDir(r), "pt.zip")) {
		t.Fatal("left a half component")
	}
}

// TestFetchRedirectLimit: a redirect loop ends after 10 redirects.
func TestFetchRedirectLimit(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	a := newMirror(t, map[string][]byte{"pt.zip": pt})
	a.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		http.Redirect(w, r, r.URL.Path, http.StatusFound)
		return true
	}
	b := newMirror(t, map[string][]byte{"pt.zip": pt})
	out := fetch(t, nil, tempRoot(t), opts(a, b))
	if out.err != nil || len(a.zipReqs()) > 3*(maxRedirects+1) || !out.warned("重定向超过 10 次") {
		t.Fatalf("%v: %d requests", out.err, len(a.zipReqs()))
	}
}

// TestFetchReusesVerifiedZip: a zip in downloads\ with the right size and SHA-1 is used without a request; a wrong one
// is downloaded again.
func TestFetchReusesVerifiedZip(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	putFile(t, filepath.Join(downloadsDir(r), "pt.zip"), string(pt))
	out := fetch(t, nil, r, opts(m))
	if out.err != nil || len(m.zipReqs()) != 0 || !out.said("校验通过，直接用") {
		t.Fatalf("%v %d", out.err, len(m.zipReqs()))
	}
	r = tempRoot(t)
	bad := append([]byte(nil), pt...)
	bad[10] ^= 1
	putFile(t, filepath.Join(downloadsDir(r), "pt.zip"), string(bad))
	out = fetch(t, nil, r, opts(m))
	if out.err != nil || len(m.zipReqs()) != 1 || !out.warned("校验不通过，重新下载") {
		t.Fatalf("%v %d", out.err, len(m.zipReqs()))
	}
}
