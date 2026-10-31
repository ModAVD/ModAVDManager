//go:build windows

package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// pkgFiles is a ModAVD package of build n: the emulator, a script, a licence and (optionally) more.
func pkgFiles(n int, extra map[string]string) map[string]string {
	files := map[string]string{
		"sdk/emulator/emulator.exe":                                        fmt.Sprintf("emulator mas-%d", n),
		"sdk/emulator/qemu/windows-x86_64/qemu-system-x86_64.exe":          fmt.Sprintf("qemu mas-%d", n),
		"sdk/emulator/qemu/windows-x86_64/qemu-system-x86_64-headless.exe": fmt.Sprintf("qemu headless free-page-reporting mas-%d", n),
		"sdk/emulator/source.properties":                                   fmt.Sprintf("Pkg.Revision=37.2.10\nPkg.BuildId=mas-%d\n", n),
		"tools/modavd-bridge.ps1":                                          fmt.Sprintf("# bridge %d\n", n),
		"说明.txt":                                                           fmt.Sprintf("ModAVD 0.%d.0\n", n),
	}
	for k, v := range extra {
		files[k] = v
	}
	return files
}

func manifestFor(n int, files map[string]string) []byte {
	entries := map[string]any{}
	for rel, body := range files {
		sum := sha256.Sum256([]byte(body))
		entries[rel] = map[string]any{"size": len(body), "sha256": hex.EncodeToString(sum[:])}
	}
	b, _ := json.Marshal(map[string]any{"schema": 1, "version": fmt.Sprintf("v0.%d.0", n), "build": n,
		"upstream": "37.2.10", "platform": "win-x64", "min_automas": nil, "files": entries})
	return b
}

// writeRoot lays out an installed root of build n (with its manifest) and returns it.
func writeRoot(t *testing.T, n int, extra map[string]string) *root.Root {
	t.Helper()
	dir := t.TempDir()
	files := pkgFiles(n, extra)
	for rel, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, manifestName), manifestFor(n, files), 0o644)
	os.WriteFile(filepath.Join(dir, "mas-avd.json"), []byte("{\r\n  \"components\": {\r\n    \"emulator\": \"37.2.10\"\r\n  },\r\n  \"instances\": {\r\n    \"6\": {\r\n      \"memoryMb\": 6144\r\n    }\r\n  }\r\n}"), 0o644)
	os.MkdirAll(filepath.Join(dir, "avd", "mas_6.avd"), 0o755)
	os.WriteFile(filepath.Join(dir, "avd", "mas_6.avd", "userdata-qemu.img"), []byte("game data"), 0o644)
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// zipOf is a package zip of files (+ its manifest unless noManifest).
func zipOf(t *testing.T, n int, files map[string]string, noManifest bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for k := range files {
		names = append(names, k)
	}
	sort.Strings(names)
	add := func(name string, body []byte) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Flags: 0x800})
		if err != nil {
			t.Fatal(err)
		}
		w.Write(body)
	}
	for _, name := range names {
		add(name, []byte(files[name]))
	}
	if !noManifest {
		add(manifestName, manifestFor(n, files))
	}
	zw.Close()
	return buf.Bytes()
}

// fakeSources serves GitHub's latest release (tag v0.<n>.0 with the zip) and MirrorChyan.
type fakeSources struct {
	srv      *httptest.Server
	gh       int
	zip      []byte
	digest   string
	mirror   func(q map[string]string) (int, any) // business code, data
	requests atomic.Int32
}

func newSources(t *testing.T, n int, zipBytes []byte) *fakeSources {
	f := &fakeSources{gh: n, zip: zipBytes}
	sum := sha256.Sum256(zipBytes)
	f.digest = "sha256:" + hex.EncodeToString(sum[:])
	mux := http.NewServeMux()
	mux.HandleFunc("/gh", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		name := fmt.Sprintf("ModAVD-0.%d.0-win-x64.zip", f.gh)
		json.NewEncoder(w).Encode(map[string]any{"tag_name": fmt.Sprintf("v0.%d.0", f.gh),
			"body":     `<!-- modavd: {"version":"v0.` + fmt.Sprint(f.gh) + `.0","min_automas":"5.8.0"} -->` + "\n正文",
			"html_url": "https://github.com/ModAVD/ModAVD/releases/tag/v0." + fmt.Sprint(f.gh) + ".0",
			"assets": []any{map[string]any{"name": name, "browser_download_url": f.srv.URL + "/dl/" + name,
				"size": len(f.zip), "digest": f.digest}}})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		http.ServeContent(w, r, "pkg.zip", zipTime, bytes.NewReader(f.zip))
	})
	mux.HandleFunc("/mc/ModAVD/latest", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		q := map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		code, data := 8001, any(nil)
		if f.mirror != nil {
			code, data = f.mirror(q)
		}
		json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": "", "data": data})
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSources) updater(r *root.Root) *Updater {
	u := New(r, nil, Options{})
	u.githubAPI, u.mirrorAPI, u.httpClient = f.srv.URL+"/gh", f.srv.URL+"/mc", f.srv.Client()
	u.scan = func(bool) ([]processUse, error) { return nil, nil }
	return u
}

// withSettings points the user settings at a temp file.
func withSettings(t *testing.T, source, cdk string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "update-settings.json")
	saved := settingsPath
	settingsPath = func() string { return p }
	t.Cleanup(func() { settingsPath = saved })
	c := cdk
	if _, err := SaveSettings(source, &c); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, r *root.Root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, filepath.FromSlash(rel)))
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func TestParseVersionAndPaths(t *testing.T) {
	for in, want := range map[string]string{"v0.36.0": "v0.36.0", "0.33.1": "v0.33.1", "v37.2.10-mas32": "v0.32.0", " v1.2.3 ": "v1.2.3"} {
		if v, ok := ParseVersion(in); !ok || v.String() != want {
			t.Errorf("ParseVersion(%q) = %v %v", in, v, ok)
		}
	}
	for _, bad := range []string{"", "v0.34.0-beta.1", "33", "v1.2"} {
		if _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	for name, want := range map[string]string{"sdk/emulator/a.exe": "sdk/emulator/a.exe", `tools\x.ps1`: "tools/x.ps1",
		"sdk/": "", "LICENSES/": "", "说明.txt": "说明.txt"} {
		if got, err := packagePath(name, ""); err != nil || got != want {
			t.Errorf("packagePath(%q) = %q %v", name, got, err)
		}
	}
	for _, bad := range []string{"../x", "/x", "a//b", "a/./b", "c:/x", "avd/mas_0.avd/x", "mas-avd.json", "MAS-AVD.JSON",
		"sdk/platform-tools/adb.exe", "sdk/system-images/x", ".update/state.json", "logs/x", "con.txt", "a/b. ", "x\x01"} {
		if _, err := packagePath(bad, ""); errs.CodeOf(err) != errs.UpdateRejected {
			t.Errorf("packagePath(%q) = %v", bad, err)
		}
	}
	if got, err := packagePath("ModAVD-37.2.10-mas32/sdk/emulator/a", "ModAVD-37.2.10-mas32"); err != nil || got != "sdk/emulator/a" {
		t.Errorf("top dir: %q %v", got, err)
	}
}

// TestInstallFromGitHub: check → prepare → install of a newer full package. The root's package files are replaced,
// a file the new package dropped goes, the instances and their data stay, mas-avd.json gets the version and one
// history entry (its other keys and their order kept), .update ends idle and empty.
func TestInstallFromGitHub(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, map[string]string{"tools/old.ps1": "dropped in 37"})
	newFiles := pkgFiles(37, map[string]string{"tools/new.ps1": "new in 37"})
	f := newSources(t, 37, zipOf(t, 37, newFiles, false))
	u := f.updater(r)
	ctx := context.Background()

	st, err := u.Check(ctx)
	if err != nil || !st.UpdateAvailable || st.LatestVersion != "v0.37.0" || st.CurrentVersion != "v0.36.0" || st.RequiredAutoMAS != "5.8.0" {
		t.Fatalf("check: %+v %v", st, err)
	}
	u.Opt.AutoMASVersion = "5.7.9"
	if _, err := u.Install(ctx); errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "AUTO-MAS 5.8.0") {
		t.Fatalf("too old AUTO-MAS: %v", err)
	}
	u.Opt.AutoMASVersion = "5.10.0"
	st, err = u.Prepare(ctx)
	if err != nil || st.Phase != phaseStaged || st.PreparedVersion != "v0.37.0" {
		t.Fatalf("prepare: %+v %v", st, err)
	}
	if read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-36" {
		t.Fatal("prepare touched sdk\\emulator")
	}
	st, err = u.Install(ctx)
	if err != nil || !st.Updated || st.CurrentVersion != "v0.37.0" || st.Phase != phaseIdle {
		t.Fatalf("install: %+v %v", st, err)
	}
	for rel, body := range newFiles {
		if got := read(t, r, rel); got != body {
			t.Errorf("%s = %q", rel, got)
		}
	}
	if lexists(filepath.Join(r.Dir, "tools", "old.ps1")) {
		t.Error("a file the new package dropped is still there")
	}
	if read(t, r, "avd/mas_6.avd/userdata-qemu.img") != "game data" {
		t.Error("instance data touched")
	}
	meta := read(t, r, "mas-avd.json")
	if !strings.Contains(meta, `"emulator": "v0.37.0"`) || !strings.Contains(meta, `"from": "v0.36.0"`) ||
		strings.Index(meta, `"instances"`) > strings.Index(meta, `"updateHistory"`) || !strings.Contains(meta, "\r\n") {
		t.Fatalf("mas-avd.json:\n%s", meta)
	}
	for _, sub := range []string{"pkg", "stage", "previous", "download"} {
		if lexists(filepath.Join(r.Dir, ".update", sub)) {
			t.Errorf(".update\\%s left", sub)
		}
	}
	// nothing newer now; a second install is a no-op, and the history entry is not repeated
	if st, err := u.Install(ctx); err != nil || st.Updated || !strings.Contains(st.Message, "最新") {
		t.Fatalf("second install: %+v %v", st, err)
	}
	if strings.Count(read(t, r, "mas-avd.json"), `"action": "update"`) != 1 {
		t.Fatal("history repeated")
	}
}

// TestInstallBlockedKeepsStage: a running program of the root stops the swap before anything moves; the verified
// stage stays for the next install.
func TestInstallBlockedKeepsStage(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	u.scan = func(bool) ([]processUse, error) {
		return []processUse{{PID: 4242, Name: "qemu-system-x86_64.exe"}}, nil
	}
	_, err := u.Install(context.Background())
	if errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "已下载并核对好") || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("blocked: %v", err)
	}
	if st := u.Status(); st.Phase != phaseStaged || st.PreparedVersion != "v0.37.0" || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-36" {
		t.Fatalf("after: %+v", st)
	}
	u.scan = func(bool) ([]processUse, error) { return nil, nil }
	n := f.requests.Load()
	if st, err := u.Install(context.Background()); err != nil || !st.Updated {
		t.Fatalf("install after: %+v %v", st, err)
	}
	if f.requests.Load()-n > 1 {
		t.Error("the staged package was downloaded again")
	}
}

// TestRecoverEveryStep: a swap cut off after each of its steps is finished or undone so the root is one whole
// version again, the way AUTO-MAS's recover() decides.
func TestRecoverEveryStep(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	cases := []struct {
		name string
		cut  func(t *testing.T, u *Updater)
		want int // build afterwards
	}{
		{"begin written, nothing moved", func(t *testing.T, u *Updater) { u.cutSwap(t, 0) }, 36},
		{"old sdk\\emulator moved away", func(t *testing.T, u *Updater) { u.cutSwap(t, 1) }, 37},
		{"new sdk\\emulator in, root files not yet", func(t *testing.T, u *Updater) { u.cutSwap(t, 2) }, 37},
		{"old moved, stage lost", func(t *testing.T, u *Updater) { u.cutSwap(t, 1); removeTree(u.l.stage()) }, 36},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := writeRoot(t, 36, nil)
			f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
			u := f.updater(r)
			if _, err := u.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			c.cut(t, u)
			msg, err := u.recover()
			if err != nil || msg == "" {
				t.Fatalf("recover: %q %v", msg, err)
			}
			if got := read(t, r, "sdk/emulator/emulator.exe"); got != fmt.Sprintf("emulator mas-%d", c.want) {
				t.Fatalf("sdk\\emulator after recover (%s): %q", msg, got)
			}
			if got := read(t, r, "说明.txt"); got != fmt.Sprintf("ModAVD 0.%d.0\n", c.want) {
				t.Fatalf("root file after recover (%s): %q", msg, got)
			}
			if p := phaseOf(u.l.readState()); p == phaseSwapping || p == phasePreparing {
				t.Fatalf("phase %s after recover", p)
			}
		})
	}
	// preparing: dropped
	r := writeRoot(t, 36, nil)
	u := New(r, nil, Options{})
	u.l.beginStaging(jsonord.NewObject().Obj())
	if msg, err := u.recover(); err != nil || !strings.Contains(msg, "上次准备新版没做完") || phaseOf(u.l.readState()) != phaseIdle {
		t.Fatalf("preparing: %q %v", msg, err)
	}
}

// cutSwap runs swapIn's steps up to rename k (0: state written, 1: old emulator moved, 2: new moved in) and stops, as
// if the process died there.
func (u *Updater) cutSwap(t *testing.T, k int) {
	t.Helper()
	l := u.l
	st := l.readState()
	plan, _ := l.planRootFiles()
	l.backupRootFiles(plan)
	st.Set("phase", jsonord.NewString(phaseSwapping))
	st.Set("plan", plan.value())
	st.Set("swap", stepVal(stepBegin))
	st.Set("from", jsonord.NewString("v0.36.0"))
	l.writeState(st)
	if k >= 1 {
		os.MkdirAll(l.previous(), 0o755)
		if err := rename(l.emulator(), l.previousEmulator()); err != nil {
			t.Fatal(err)
		}
		st.Set("swap", stepVal(stepOldMoved))
		l.writeState(st)
	}
	if k >= 2 {
		if err := rename(l.stageEmulator(), l.emulator()); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBadPackages: a package that fails its checks is never staged, and the download is dropped.
func TestBadPackages(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	cases := map[string]func() (map[string]string, bool){
		"file not in manifest": func() (map[string]string, bool) {
			return pkgFiles(37, nil), false
		},
		"no manifest": func() (map[string]string, bool) { return pkgFiles(37, nil), true },
		"wrong build": func() (map[string]string, bool) { return pkgFiles(36, nil), false },
		"protected":   func() (map[string]string, bool) { return pkgFiles(37, map[string]string{"avd/x.ini": "x"}), false },
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			r := writeRoot(t, 36, nil)
			files, noManifest := mk()
			z := zipOf(t, 37, files, noManifest)
			if name == "file not in manifest" {
				var buf bytes.Buffer
				zw := zip.NewWriter(&buf)
				zr, _ := zip.NewReader(bytes.NewReader(z), int64(len(z)))
				for _, zf := range zr.File {
					w, _ := zw.CreateHeader(&zip.FileHeader{Name: zf.Name, Method: zip.Deflate, Flags: 0x800})
					rc, _ := zf.Open()
					var b bytes.Buffer
					b.ReadFrom(rc)
					rc.Close()
					w.Write(b.Bytes())
				}
				w, _ := zw.Create("tools/extra.ps1")
				w.Write([]byte("not listed"))
				zw.Close()
				z = buf.Bytes()
			}
			f := newSources(t, 37, z)
			u := f.updater(r)
			_, err := u.Prepare(context.Background())
			if errs.CodeOf(err) != errs.UpdateRejected {
				t.Fatalf("prepare: %v", err)
			}
			if st := u.Status(); st.Phase != phaseIdle || st.PreparedVersion != "" || lexists(u.l.downloadAt("ModAVD-0.37.0-win-x64.zip")) {
				t.Fatalf("after: %+v", st)
			}
		})
	}
}

// TestMirrorChyan: with the CDK the download request carries it and the local version (incremental); a CDK error
// falls back to GitHub with a notice; the check never sends the CDK.
func TestMirrorChyan(t *testing.T) {
	withSettings(t, SourceMirrorChyan, "cdk-secret-1")
	r := writeRoot(t, 36, nil)
	newFiles := pkgFiles(37, map[string]string{"tools/new.ps1": "new"})
	full := zipOf(t, 37, newFiles, false)
	f := newSources(t, 37, full)
	var seen []map[string]string
	f.mirror = func(q map[string]string) (int, any) {
		seen = append(seen, q)
		if q["cdk"] == "" {
			return 0, map[string]any{"version_name": "v0.37.0", "release_note": ""}
		}
		return 7002, nil
	}
	u := f.updater(r)
	if st, err := u.Check(context.Background()); err != nil || st.LatestSource != SourceMirrorChyan || st.LatestVersion != "v0.37.0" {
		t.Fatalf("check: %+v %v", st, err)
	}
	if seen[0]["cdk"] != "" || seen[0]["os"] != "win" || seen[0]["arch"] != "x64" || seen[0]["current_version"] != "v0.36.0" {
		t.Fatalf("check query %v", seen[0])
	}
	st, err := u.Install(context.Background())
	if err != nil || !st.Updated || len(st.Notices) != 1 || !strings.Contains(st.Notices[0], "填入的 CDK 错误") {
		t.Fatalf("install with a bad CDK: %+v %v", st, err)
	}
	if last := seen[len(seen)-1]; last["cdk"] != "cdk-secret-1" || last["current_version"] != "v0.36.0" {
		t.Fatalf("download query %v", last)
	}
	b, _ := os.ReadFile(filepath.Join(r.Dir, ".update", "state.json"))
	if bytes.Contains(b, []byte("cdk-secret-1")) || strings.Contains(fmt.Sprint(st), "cdk-secret-1") {
		t.Fatal("the CDK leaked")
	}
}

func TestSettings(t *testing.T) {
	withSettings(t, SourceMirrorChyan, "abc-123")
	s := LoadSettings()
	if s.Source != SourceMirrorChyan || s.CDK() != "abc-123" {
		t.Fatalf("%+v %q", s, s.CDK())
	}
	b, _ := os.ReadFile(settingsPath())
	if bytes.Contains(b, []byte("abc-123")) {
		t.Fatal("CDK in clear")
	}
	empty := ""
	if s, err := SaveSettings(SourceGitHub, &empty); err != nil || s.HasCDK() || LoadSettings().Source != SourceGitHub {
		t.Fatalf("clear: %+v %v", s, err)
	}
	if _, err := SaveSettings("ftp", nil); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("bad source: %v", err)
	}
	bad := "a b&c"
	if _, err := SaveSettings("", &bad); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("bad cdk: %v", err)
	}
}

var zipTime = time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
