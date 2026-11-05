//go:build windows

package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/components"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// componentsRoot is a ModAVD root (sdk\emulator\emulator.exe) under $MODAVD_TEST_TMP\cli when set, else the OS temp directory.
func componentsRoot(t *testing.T) *root.Root {
	t.Helper()
	var dir string
	if env := os.Getenv("MODAVD_TEST_TMP"); env != "" {
		base := filepath.Join(env, "cli")
		if err := os.MkdirAll(base, 0o755); err != nil {
			t.Fatal(err)
		}
		dir = testutil.MkdirTemp(t, base, "t-")
	} else {
		dir = testutil.TempDir(t)
	}
	for k := 0; k < len(dir); k++ {
		if dir[k] >= 0x80 {
			t.Skip("临时目录的路径里有非 ASCII 字符；把 MODAVD_TEST_TMP 设成只含英文的目录再测")
		}
	}
	for name, body := range map[string]string{`sdk\emulator\emulator.exe`: "MZ", `sdk\platform-tools\adb.exe`: "MZ",
		`sdk\platform-tools\source.properties`: "Pkg.Revision=37.0.1\n"} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (h *harness) useRoot(r *root.Root) *int {
	calls := new(int)
	h.resolve = func(string) (*root.Root, error) { *calls++; return r, nil }
	return calls
}

// TestComponentsStatus: status is the default and returns check's component list, every field present (build too).
func TestComponentsStatus(t *testing.T) {
	r := componentsRoot(t)
	h := newHarness()
	h.useRoot(r)
	res := h.run(t, nil, "--json", "components")
	if res.code != 0 {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	data := envelope(t, res.stdout)["data"].(map[string]any)
	comps := data["components"].([]any)
	if data["root"] != r.Dir || len(comps) != 5 {
		t.Fatalf("%v", data)
	}
	for _, c := range comps {
		m := c.(map[string]any)
		for _, k := range []string{"id", "name", "version", "build", "installed", "optional", "local_sdk"} {
			if _, ok := m[k]; !ok {
				t.Errorf("%v has no %s", m["id"], k)
			}
		}
	}
	if pt := comps[0].(map[string]any); pt["id"] != "platform-tools" || pt["installed"] != true || pt["version"] != "37.0.1" {
		t.Errorf("platform-tools %v", pt)
	}
	res = h.run(t, nil, "components", "status")
	if res.code != 0 || !strings.Contains(res.stdout, "✓ 平台工具（adb） 37.0.1") || !strings.Contains(res.stdout, "✗ 系统镜像 Android 14 Google APIs x86_64 未安装") {
		t.Fatalf("human: %q", res.stdout)
	}
}

// TestComponentsArgumentsBeforeRoot: a wrong action, fetch options on status and every bad fetch option are refused
// before the root is looked up.
func TestComponentsArgumentsBeforeRoot(t *testing.T) {
	for _, args := range [][]string{
		{"components", "install"},
		{"components", "status", "--accept-license"},
		{"components", "fetch", "--connect-timeout", "0"},
		{"components", "fetch", "--connect-timeout", "601"},
		{"components", "fetch", "--accept-license", "--connect-timeout", "18446744074"},
		{"components", "fetch", "--accept-license", "--stall-timeout", "9223372036854775807"},
		{"components", "fetch", "--accept-license", "--manifest-timeout", "18446744074"},
		{"components", "fetch", "--stall-timeout", "601"},
		{"components", "fetch", "--manifest-timeout", "1801"},
		{"components", "fetch", "--manifest-timeout", "0"},
		{"components", "fetch", "--mirror", "aliyun"},
		{"components", "fetch", "--mirror-url", "ftp://example.invalid/"},
		{"components", "fetch", "--test-free-space-mb", "-1"},
		{"components", "fetch"}, // --json without --accept-license
		{"components", "fetch", "--keep-downloads"},
	} {
		h := newHarness()
		calls := new(int)
		h.resolve = func(string) (*root.Root, error) {
			*calls++
			return nil, errs.New(errs.Internal, "unexpected root lookup")
		}
		res := h.run(t, nil, append([]string{"--json"}, args...)...)
		code := envelope(t, res.stdout)["error"].(map[string]any)["code"]
		if res.code != 2 || (code != errs.Usage && code != errs.InvalidArgument) || *calls != 0 {
			t.Errorf("%q: exit %d %v, root looked up %d times", args, res.code, code, *calls)
		}
	}
}

// TestComponentsFetchOptions: every option reaches the fetch (--mirror-url repeated and comma-separated, --mirror in
// any case, --test-free-space-mb 0 as a value); the result is the envelope's data; the license notice is shown
// whatever the level, progress only every 2 s (and the final line).
func TestComponentsFetchOptions(t *testing.T) {
	r := componentsRoot(t)
	h := newHarness()
	h.useRoot(r)
	var got components.Options
	h.fetch = func(ctx context.Context, rr *root.Root, o components.Options) (*components.Result, error) {
		got = o
		o.Notice("《许可协议》全文\n")
		for _, final := range []bool{false, false, true} {
			o.Progress(components.Progress{Stage: "downloading", Message: "进度行", Final: final})
		}
		return &components.Result{Root: rr.Dir, License: "terms", LicenseSource: "m", Downloads: []string{"pt.zip"},
			Components: []components.ComponentResult{{ID: "platform-tools", Name: "平台工具（adb）", Version: "37.0.1",
				Installed: true, Downloaded: true, SetAside: `sdk\platform-tools.incomplete-20261101-120000`}}}, nil
	}
	res := h.run(t, nil, "--json", "components", "fetch", "--accept-license", "--keep-downloads", "--mirror", "TENCENT",
		"--mirror-url", "http://a.invalid/", "--mirror-url", "http://b.invalid/,http://c.invalid/", "--connect-timeout", "7",
		"--stall-timeout", "8", "--manifest-timeout", "9", "--test-free-space-mb", "0")
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.stdout)
	}
	if !got.AcceptLicense || !got.JSON || !got.KeepDownloads || got.Mirror != "TENCENT" ||
		strings.Join(got.MirrorURLs, " ") != "http://a.invalid/ http://b.invalid/,http://c.invalid/" ||
		got.ConnectTimeout != 7*time.Second || got.StallTimeout != 8*time.Second || got.ManifestTimeout != 9*time.Second ||
		got.TestFreeSpaceMB == nil || *got.TestFreeSpaceMB != 0 || got.Input == nil {
		t.Fatalf("options %+v", got)
	}
	data := envelope(t, res.stdout)["data"].(map[string]any)
	c := data["components"].([]any)[0].(map[string]any)
	if data["license"] != "terms" || data["license_source"] != "m" || data["downloads"].([]any)[0] != "pt.zip" ||
		c["set_aside"] != `sdk\platform-tools.incomplete-20261101-120000` || c["downloaded"] != true || c["skipped"] != false {
		t.Fatalf("data %v", data)
	}
	var notices, progress int
	for _, line := range strings.Split(strings.TrimSpace(res.stderr), "\n") {
		var l map[string]any
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			t.Fatalf("stderr line is not JSON: %q", line)
		}
		switch l["msg"] {
		case "《许可协议》全文":
			notices++
		case "进度行":
			progress++
		}
	}
	if notices != 1 || progress != 2 {
		t.Fatalf("%d notices, %d progress lines: %s", notices, progress, res.stderr)
	}
	// --quiet hides progress, never the license.
	res = h.run(t, nil, "--quiet", "components", "fetch", "--accept-license")
	if res.code != 0 || !strings.Contains(res.stderr, "《许可协议》全文") || strings.Contains(res.stderr, "进度行") {
		t.Fatalf("quiet: %q", res.stderr)
	}
	if !strings.Contains(res.stdout, "已装好：平台工具（adb）（版本 37.0.1）") || !strings.Contains(res.stdout, "原来不完整的目录改名为") ||
		!strings.Contains(res.stdout, "完成，可以在 AUTO-MAS 里添加这个文件夹了。") {
		t.Fatalf("summary %q", res.stdout)
	}
}

// licenseServer serves only repository2-3.xml (zips are 404) and counts requests.
func licenseServer(t *testing.T) (*httptest.Server, *int32) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		if strings.HasSuffix(r.URL.Path, "/repository2-3.xml") {
			io.WriteString(w, `<sdk-repository><license id="android-sdk-license">CLI TEST TERMS</license></sdk-repository>`)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// TestComponentsFetchJSONNeedsAcceptLicense: --json without --accept-license is a usage error (exit 2) before the
// root is looked up or any request is made, also when the root lacks nothing (the fetch itself would then succeed
// without asking): the same command line gets the same answer whatever the root holds.
func TestComponentsFetchJSONNeedsAcceptLicense(t *testing.T) {
	srv, n := licenseServer(t)
	h := newHarness()
	calls := h.useRoot(componentsRoot(t))
	fetched := false
	h.fetch = func(context.Context, *root.Root, components.Options) (*components.Result, error) {
		fetched = true
		return &components.Result{}, nil // a root that lacks nothing
	}
	res := h.run(t, nil, "--json", "components", "fetch", "--mirror-url", srv.URL)
	e := envelope(t, res.stdout)["error"].(map[string]any)
	if res.code != 2 || e["code"] != errs.Usage || !strings.Contains(e["message"].(string), "--accept-license") ||
		atomic.LoadInt32(n) != 0 || *calls != 0 || fetched {
		t.Fatalf("exit %d %v, %d requests, root looked up %d times, fetched %v", res.code, e, atomic.LoadInt32(n),
			*calls, fetched)
	}
	// without --json the question is asked instead, so nothing is refused here
	if res := h.run(t, nil, "components", "fetch", "--mirror-url", srv.URL); res.code != 0 || !fetched {
		t.Fatalf("without --json: exit %d, fetched %v", res.code, fetched)
	}
}

// TestComponentsFetchQuestion: the real fetch through the CLI: with --quiet the license and the question still show,
// stdin answers; n is license_declined (exit 2, nothing downloaded), y goes on to the downloads (404 here: exit 9).
// With --json every stderr line stays JSON.
func TestComponentsFetchQuestion(t *testing.T) {
	srv, _ := licenseServer(t)
	r := componentsRoot(t)
	h := newHarness()
	h.useRoot(r)
	h.stdin = "n\n"
	res := h.run(t, nil, "--quiet", "components", "fetch", "--mirror-url", srv.URL, "--test-free-space-mb", "100000")
	if res.code != 2 || !strings.Contains(res.stderr, "CLI TEST TERMS") || !strings.Contains(res.stderr, "是否同意这份许可协议") ||
		!strings.Contains(res.stderr, "错误：没有同意许可协议") {
		t.Fatalf("n: exit %d %q", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "downloads")); err == nil {
		t.Fatal("declined, but downloads exists")
	}
	h.stdin = "y\n"
	res = h.run(t, nil, "components", "fetch", "--mirror-url", srv.URL, "--test-free-space-mb", "100000")
	if res.code != 9 || !strings.Contains(res.stderr, "所有镜像都没下成") || !strings.Contains(res.stderr, "HTTP 404") {
		t.Fatalf("y: exit %d %q", res.code, res.stderr)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "downloads", "android-sdk-license.txt")); err != nil {
		t.Fatal("accepted, but no license copy")
	}
	res = h.run(t, nil, "--json", "components", "fetch", "--accept-license", "--mirror-url", srv.URL, "--test-free-space-mb", "100000")
	if e := envelope(t, res.stdout)["error"].(map[string]any); res.code != 9 || e["code"] != errs.ComponentsUnavailable {
		t.Fatalf("json: exit %d %v", res.code, e)
	}
	for _, line := range strings.Split(strings.TrimSpace(res.stderr), "\n") {
		if !json.Valid([]byte(line)) {
			t.Fatalf("stderr line is not JSON: %q", line)
		}
	}
	if !strings.Contains(res.stderr, "CLI TEST TERMS") {
		t.Fatal("license not shown with --json")
	}
}
