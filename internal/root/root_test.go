package root

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

// ---- helpers -----------------------------------------------------------------------------------------------------

func newRoot(t *testing.T) *Root {
	t.Helper()
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if errs.CodeOf(err) != code {
		t.Fatalf("want %s, got %v (%s)", code, err, errs.CodeOf(err))
	}
}

// python runs a script with the Python 3 used for reference outputs ($MODAVD_TEST_PYTHON, otherwise python on PATH);
// skipped when there is none. The script gets the input file and an output file.
func python(t *testing.T, script string, input any) []byte {
	t.Helper()
	py := os.Getenv("MODAVD_TEST_PYTHON")
	if py == "" {
		found, err := exec.LookPath("python")
		if err != nil {
			t.Skipf("no python on PATH (set MODAVD_TEST_PYTHON): %v", err)
		}
		py = found
	}
	if _, err := os.Stat(py); err != nil {
		t.Skipf("no Python at %s", py)
	}
	dir := t.TempDir()
	in, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	inPath, outPath, scriptPath := filepath.Join(dir, "in.json"), filepath.Join(dir, "out.json"), filepath.Join(dir, "s.py")
	writeFile(t, inPath, in)
	writeFile(t, scriptPath, []byte(script))
	if out, err := exec.Command(py, "-I", scriptPath, inPath, outPath).CombinedOutput(); err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	return readFile(t, outPath)
}

// ---- ports -------------------------------------------------------------------------------------------------------

func TestPortsAndNames(t *testing.T) {
	if ConsolePort(3) != 20030 || AdbPort(3) != 20031 || GrpcPort(3) != 20032 || AvdName(3) != "mas_3" ||
		Serial(3) != "emulator-20030" {
		t.Fatal("port plan")
	}
	for i, want := range []bool{true, true, true, true, true, false, true, true, true, true, false} {
		if ValidIndex(i) != want {
			t.Errorf("ValidIndex(%d)", i)
		}
	}
	if ValidIndex(-1) {
		t.Error("ValidIndex(-1)")
	}
	if i, ok := IndexOfConsolePort(20090); !ok || i != 9 {
		t.Error("20090")
	}
	for _, p := range []int{19990, 20001, 20100, 5554} {
		if _, ok := IndexOfConsolePort(p); ok {
			t.Errorf("IndexOfConsolePort(%d)", p)
		}
	}
	ports := InstancePorts()
	if len(ports) != 27 || !ports[20000] || !ports[20092] || ports[20050] || ports[20051] || ports[20049] {
		t.Fatalf("InstancePorts: %d", len(ports))
	}
}

// ---- resolve -----------------------------------------------------------------------------------------------------

func TestResolve(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "ModAVD")
	writeFile(t, filepath.Join(rootDir, "tools", "modavd.exe"), nil)
	if err := os.MkdirAll(filepath.Join(rootDir, "sdk"), 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(base, "other")
	writeFile(t, filepath.Join(other, MetadataFile), []byte("{}"))

	check := func(flag, env, exe, want string) {
		t.Helper()
		r, err := resolve(flag, env, exe)
		if err != nil {
			t.Fatalf("resolve(%q, %q, %q): %v", flag, env, exe, err)
		}
		if r.Dir != want {
			t.Fatalf("resolve(%q, %q, %q) = %s, want %s", flag, env, exe, r.Dir, want)
		}
	}
	exeInTools := filepath.Join(rootDir, "tools", "modavd.exe")
	check(other, rootDir, exeInTools, other)                         // flag wins
	check(" \""+other+`\"`, "", "", other)                           // quotes / trailing backslash-quote
	check("", rootDir+`\`, exeInTools, rootDir)                      // env next
	check("", "", exeInTools, rootDir)                               // exe in <root>\tools
	check("", "", filepath.Join(rootDir, "modavd.exe"), rootDir)     // exe in <root>
	check("", "", filepath.Join(other, "modavd.exe"), other)         // mas-avd.json alone counts
	check(filepath.Join(rootDir, "sdk", ".."), "", "", rootDir)      // cleaned
	check(`  `, "", filepath.Join(rootDir, "sdk", "x.exe"), rootDir) // blank flag ignored; parent of exe dir

	_, err := resolve(filepath.Join(base, "missing"), "", "")
	wantCode(t, err, errs.NotFound)
	if !strings.Contains(err.Error(), "--root 指定的魔改 AVD 根目录不存在") {
		t.Fatalf("message: %v", err)
	}
	_, err = resolve("", filepath.Join(base, "missing"), "")
	wantCode(t, err, errs.NotFound)
	if !strings.Contains(err.Error(), "MODAVD_ROOT") {
		t.Fatalf("message: %v", err)
	}
	_, err = resolve(filepath.Join(other, MetadataFile), "", "")
	wantCode(t, err, errs.NotFound)
	_, err = resolve("", "", filepath.Join(base, "x", "y", "modavd.exe"))
	wantCode(t, err, errs.Usage)
	if !strings.Contains(err.Error(), "--root") {
		t.Fatalf("advice: %v", err)
	}
	if _, err := New(filepath.Join(base, "missing")); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("New missing: %v", err)
	}
}

func TestPaths(t *testing.T) {
	r := newRoot(t)
	d := r.Dir
	for _, c := range [][2]string{
		{r.SDK(), d + `\sdk`},
		{r.RuntimeSDK(), d + `\sdk`},
		{r.EmulatorExe(), d + `\sdk\emulator\emulator.exe`},
		{r.AdbExe(), d + `\sdk\platform-tools\adb.exe`},
		{r.QemuHeadlessExe(), d + `\sdk\emulator\qemu\windows-x86_64\qemu-system-x86_64-headless.exe`},
		{r.AvdHome(), d + `\avd`},
		{r.LogsDir(), d + `\logs`},
		{r.ShaderCacheDir(), d + `\shader-cache`},
		{r.GuestPatchDir(), d + `\guest-patches`},
		{r.ComponentsDir(), d + `\components`},
		{r.MetadataPath(), d + `\mas-avd.json`},
		{r.LauncherAPK(), d + `\components\de.jrpie.android.launcher_57.apk`},
		{r.ImeAPK(), d + `\components\modavd-ime.apk`},
		{r.ManifestPath(), d + `\modavd-manifest.json`},
	} {
		if c[0] != c[1] {
			t.Errorf("got %s want %s", c[0], c[1])
		}
	}
	for meta, want := range map[string]string{
		`{"sdkRoot": "  C:/sdk-src/sdk-mas35/  "}`: `C:\sdk-src\sdk-mas35`,
		`{"sdkRoot": "dev-sdk"}`:                   d + `\dev-sdk`,
		`{"sdkRoot": "   "}`:                       "",
		`{"sdkRoot": 5}`:                           "",
		`{"sdkRoot": null}`:                        "",
		`[1]`:                                      "",
	} {
		writeFile(t, r.MetadataPath(), []byte(meta))
		if got := r.LocalSDKRoot(); got != want {
			t.Errorf("%s: LocalSDKRoot %q want %q", meta, got, want)
		}
	}
	writeFile(t, r.MetadataPath(), []byte(`{"sdkRoot": "C:\\sdk-src\\sdk-mas35"}`))
	if r.AdbExe() != `C:\sdk-src\sdk-mas35\platform-tools\adb.exe` || r.SDK() != d+`\sdk` {
		t.Errorf("runtime sdk: %s", r.AdbExe())
	}
}

// ---- metadata ----------------------------------------------------------------------------------------------------

func TestReadMetadata(t *testing.T) {
	r := newRoot(t)
	v, err := r.ReadMetadata()
	if err != nil || v.Obj().Len() != 0 {
		t.Fatalf("missing file: %v", err)
	}
	writeFile(t, r.MetadataPath(), []byte("\xef\xbb\xbf{\"a\": 1}"))
	if v, err := r.ReadMetadata(); err != nil || !v.Obj().Has("a") {
		t.Fatalf("BOM: %v", err)
	}
	for _, bad := range []string{`{"a": `, `[1, 2]`, `"x"`, "\xff", ""} {
		writeFile(t, r.MetadataPath(), []byte(bad))
		_, err := r.ReadMetadata()
		wantCode(t, err, errs.MetadataUnreadable)
		if !strings.Contains(err.Error(), r.MetadataPath()) {
			t.Errorf("message lacks path: %v", err)
		}
		if r.MetadataLoose().Obj().Len() != 0 {
			t.Error("loose read not empty")
		}
	}
	os.Remove(r.MetadataPath())
	if err := os.Mkdir(r.MetadataPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = r.ReadMetadata()
	wantCode(t, err, errs.MetadataUnreadable)
}

const sampleMetadata = "\xef\xbb\xbf{\n  \"components\": {\"emulator\": \"37.2.10\"},\n  \"zeta\": [1e3, -0, {}],\n" +
	"  \"instances\": {\"0\": {\"memoryMb\": 6144, \"initialized\": true}},\n  \"sdkRoot\": \"\"\n}\n"

func TestUpdateMetadataFormatAndAbort(t *testing.T) {
	r := newRoot(t)
	writeFile(t, r.MetadataPath(), []byte(sampleMetadata))
	err := r.UpdateMetadata(func(o *jsonord.Obj) error {
		o.Set("adbServerPort", jsonord.NewInt(20089))
		o.Set("名称", jsonord.NewString("魔改 \"AVD\""))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\r\n  \"components\": {\r\n    \"emulator\": \"37.2.10\"\r\n  },\r\n  \"zeta\": [\r\n    1000.0,\r\n    0,\r\n" +
		"    {}\r\n  ],\r\n  \"instances\": {\r\n    \"0\": {\r\n      \"memoryMb\": 6144,\r\n      \"initialized\": true\r\n" +
		"    }\r\n  },\r\n  \"sdkRoot\": \"\",\r\n  \"adbServerPort\": 20089,\r\n  \"名称\": \"魔改 \\\"AVD\\\"\"\r\n}"
	if got := string(readFile(t, r.MetadataPath())); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	before := readFile(t, r.MetadataPath())
	sentinel := errors.New("stop")
	if err := r.UpdateMetadata(func(o *jsonord.Obj) error { o.Set("x", nil); return sentinel }); err != sentinel {
		t.Fatalf("fn error: %v", err)
	}
	if !bytes.Equal(before, readFile(t, r.MetadataPath())) {
		t.Fatal("aborted update wrote")
	}
	// An unreadable file is never overwritten.
	writeFile(t, r.MetadataPath(), []byte(`{"broken": `))
	err = r.UpdateMetadata(func(o *jsonord.Obj) error { t.Fatal("fn called"); return nil })
	wantCode(t, err, errs.MetadataUnreadable)
	err = r.UpdateInstanceMeta(1, map[string]*jsonord.Value{"memoryMb": jsonord.NewInt(4096)})
	wantCode(t, err, errs.MetadataUnreadable)
	if string(readFile(t, r.MetadataPath())) != `{"broken": ` {
		t.Fatal("unreadable file overwritten")
	}
	entries, _ := os.ReadDir(r.Dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
}

// TestMetadataMatchesPython: what UpdateMetadata writes is byte for byte what AUTO-MAS's write_metadata writes for
// the same data (Python text mode on Windows: CRLF).
func TestMetadataMatchesPython(t *testing.T) {
	r := newRoot(t)
	writeFile(t, r.MetadataPath(), []byte(sampleMetadata))
	if err := r.UpdateMetadata(func(o *jsonord.Obj) error { return nil }); err != nil {
		t.Fatal(err)
	}
	script := `import json, sys
from pathlib import Path
src = json.load(open(sys.argv[1], encoding="utf-8"))["src"]
data = json.loads(src.encode("latin-1").decode("utf-8-sig"))
Path(sys.argv[2]).write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
`
	// Bytes travel as latin-1 code points so the BOM survives the JSON input file.
	want := python(t, script, map[string]string{"src": string([]rune(latin1(sampleMetadata)))})
	if got := readFile(t, r.MetadataPath()); !bytes.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func latin1(s string) string {
	var sb strings.Builder
	for i := 0; i < len(s); i++ {
		sb.WriteRune(rune(s[i]))
	}
	return sb.String()
}

func TestReplaceRetriesWhileTargetIsOpen(t *testing.T) {
	r := newRoot(t)
	writeFile(t, r.MetadataPath(), []byte(`{}`))
	// Open the target the way Python does (no FILE_SHARE_DELETE): the rename is refused until it is closed.
	p, _ := syscall.UTF16PtrFromString(r.MetadataPath())
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(600 * time.Millisecond)
		syscall.CloseHandle(h)
	}()
	start := time.Now()
	if err := r.UpdateMetadata(func(o *jsonord.Obj) error { o.Set("k", jsonord.NewInt(1)); return nil }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatalf("rename did not wait for the reader (%v)", time.Since(start))
	}
	if !strings.Contains(string(readFile(t, r.MetadataPath())), `"k": 1`) {
		t.Fatal("not written")
	}
}

func TestMetadataLock(t *testing.T) {
	r := newRoot(t)
	var wg sync.WaitGroup
	const n = 16
	for k := 0; k < n; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := r.UpdateMetadata(func(o *jsonord.Obj) error {
				c, _ := o.Get("count").Int()
				time.Sleep(2 * time.Millisecond)
				o.Set("count", jsonord.NewInt(c+1))
				return nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if c, _ := r.MetadataLoose().Obj().Get("count").Int(); c != n {
		t.Fatalf("lost updates: count %d", c)
	}

	held, ok, err := lockFile(r.MetadataPath()+".lock", time.Second)
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	old := metadataLockWait
	metadataLockWait = 300 * time.Millisecond
	defer func() { metadataLockWait = old }()
	err = r.UpdateMetadata(func(o *jsonord.Obj) error { return nil })
	wantCode(t, err, errs.Busy)
	held.unlock()
	if err := r.UpdateMetadata(func(o *jsonord.Obj) error { return nil }); err != nil {
		t.Fatalf("after unlock: %v", err)
	}
}

func TestInstanceMeta(t *testing.T) {
	r := newRoot(t)
	writeFile(t, r.MetadataPath(), []byte(`{"addedAt": "x", "instances": {"0": {"memoryMb": 4096, "keep": [1]}, "1": 5}}`))
	if m := r.InstanceMeta(0); m.Len() != 2 || !m.Has("keep") {
		t.Fatalf("InstanceMeta(0): %v", m.Keys())
	}
	if r.InstanceMeta(1).Len() != 0 || r.InstanceMeta(7).Len() != 0 {
		t.Fatal("non-object / missing meta not empty")
	}
	m := r.InstanceMeta(0)
	m.Set("changed", jsonord.NewInt(1))
	if r.InstanceMeta(0).Has("changed") {
		t.Fatal("InstanceMeta is not a copy")
	}

	// First-boot style write: existing key in place, new keys in AUTO-MAS's keyword order.
	err := r.UpdateInstanceMeta(0, map[string]*jsonord.Value{
		"initErrors": jsonord.NewArray(), "softwareRenderer": jsonord.NewBool(false), "renderer": jsonord.NewString("GLES"),
		"launcher": jsonord.NewString("l"), "initializedAt": jsonord.NewString("t"), "initialized": jsonord.NewBool(true),
		"zz": jsonord.NewInt(1), "aa": nil, "memoryMb": jsonord.NewInt(6144),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(r.InstanceMeta(0).Keys(), ",")
	if want := "memoryMb,keep,initialized,initializedAt,launcher,renderer,softwareRenderer,initErrors,aa,zz"; got != want {
		t.Fatalf("keys %s\nwant %s", got, want)
	}
	if v, _ := r.InstanceMeta(0).Get("memoryMb").Int(); v != 6144 {
		t.Fatal("memoryMb not replaced")
	}
	// "1" was not an object: replaced in place by a fresh one.
	if err := r.UpdateInstanceMeta(1, map[string]*jsonord.Value{"memoryMb": jsonord.NewInt(3072)}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.MetadataLoose().Obj().Get("instances").Obj().Keys(), ","); got != "0,1" {
		t.Fatalf("instances order %s", got)
	}
	err = r.EditInstanceMeta(3, func(meta *jsonord.Obj) error {
		meta.Set("memoryMb", jsonord.NewInt(5120))
		meta.Set("initialized", jsonord.NewBool(false))
		meta.Set("createdAt", jsonord.NewString("now"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.InstanceMeta(3).Keys(), ","); got != "memoryMb,initialized,createdAt" {
		t.Fatalf("edit order %s", got)
	}

	if err := r.RemoveInstanceMeta(1); err != nil {
		t.Fatal(err)
	}
	if r.MetadataLoose().Obj().Get("instances").Obj().Has("1") {
		t.Fatal("not removed")
	}
	before := readFile(t, r.MetadataPath())
	fi1, _ := os.Stat(r.MetadataPath())
	time.Sleep(20 * time.Millisecond)
	if err := r.RemoveInstanceMeta(8); err != nil {
		t.Fatal(err)
	}
	fi2, _ := os.Stat(r.MetadataPath())
	if !bytes.Equal(before, readFile(t, r.MetadataPath())) || !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Fatal("removing an absent instance wrote the file")
	}

	// No instances key at all: appended at the end.
	writeFile(t, r.MetadataPath(), []byte(`{"a": 1, "instances": [], "b": 2}`))
	if err := r.UpdateInstanceMeta(2, map[string]*jsonord.Value{"memoryMb": jsonord.NewInt(3072)}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.MetadataLoose().Obj().Keys(), ","); got != "a,instances,b" {
		t.Fatalf("top keys %s", got)
	}
}

// TestPortsMatchPython: adbServerPort / scriptAdbServerPort follow AUTO-MAS's _metadata_port (int() included).
func TestPortsMatchPython(t *testing.T) {
	values := []string{`null`, `20089`, `"20089"`, `" 20089 "`, `"20_089"`, `"+20089"`, `"2e4"`, `20089.0`, `20089.9`,
		`2e4`, `true`, `[20089]`, `"20089.0"`, `5037`, `20001`, `20050`, `20049`, `20048`, `1023`, `1024`, `65535`,
		`65536`, `-20089`, `"0x4e39"`, `"020089"`, `""`, `{}`, `20092`, `20093`}
	type pair struct{ A, S string }
	var cases []pair
	for _, a := range values {
		for _, s := range []string{"", `20049`, `20089`, `"20050"`, `20049.5`, a} {
			cases = append(cases, pair{a, s})
		}
	}
	script := `import json, sys
MAX, PORT_BASE, PORT_STEP = 9, 20000, 10
def valid(i): return 0 <= i <= MAX and i != 5
ports = set()
for i in range(MAX + 1):
    if valid(i):
        c = PORT_BASE + PORT_STEP * i
        ports.update((c, c + 1, c + 2))
def metadata_port(meta, key):
    raw = meta.get(key)
    try:
        port = int(raw)
    except (TypeError, ValueError):
        return None
    if not 1024 <= port <= 65535 or port == 5037 or port in ports:
        return None
    return port
out = []
for case in json.load(open(sys.argv[1], encoding="utf-8")):
    meta = {}
    if case["A"]: meta["adbServerPort"] = json.loads(case["A"])
    if case["S"]: meta["scriptAdbServerPort"] = json.loads(case["S"])
    p = metadata_port(meta, "adbServerPort")
    private = p if p is not None else 20050
    s = metadata_port(meta, "scriptAdbServerPort")
    script = s if (s is not None and s != private) else (20049 if 20049 != private else 20048)
    out.append([private, script])
json.dump(out, open(sys.argv[2], "w", encoding="utf-8"))
`
	var want [][2]int
	if err := json.Unmarshal(python(t, script, cases), &want); err != nil {
		t.Fatal(err)
	}
	r := newRoot(t)
	for k, c := range cases {
		meta := "{"
		if c.A != "" {
			meta += `"adbServerPort": ` + c.A
		}
		if c.S != "" {
			if c.A != "" {
				meta += ", "
			}
			meta += `"scriptAdbServerPort": ` + c.S
		}
		meta += "}"
		writeFile(t, r.MetadataPath(), []byte(meta))
		if got := [2]int{r.AdbServerPort(), r.ScriptAdbServerPort()}; got != want[k] {
			t.Errorf("%s: got %v want %v", meta, got, want[k])
		}
	}
	os.Remove(r.MetadataPath())
	if r.AdbServerPort() != DefaultAdbServerPort || r.ScriptAdbServerPort() != DefaultScriptAdbServerPort {
		t.Fatal("defaults")
	}
}

// ---- python text helpers -----------------------------------------------------------------------------------------

func TestPyCompatMatchesPython(t *testing.T) {
	inputs := [][]byte{
		[]byte("a=1\nb = 2 \r\nc\r=3\x0bd=4\x0ce=5\x1cf=6\x1dg=7\x1eh=8\u0085i=9\u2028j=10\u2029k=11\n\nl=\n=m\n"),
		[]byte("\xff\xfe\xe4\xb8\xe4\xb8\xad\xed\xa0\x80\xf0\x9f\x98\xf0\x9f\x98\x80\xc0\xaf\xf4\x90\x80\x80\xe0\x80\xf8"),
		[]byte("  \t\x1c\x1fkey\u3000=\u00a0value\u2003 \x85\n"),
		[]byte("Pkg.BuildId=mas-35\nPkg.Revision = 37.2.10\r\nPkg.BuildId= mas-36 "),
		[]byte("no newline at end"),
		[]byte(""),
		[]byte("\n"),
		[]byte("\r\r\n\n"),
		[]byte("x=\xe4\xb8\n\xe4\xb8=y\n"),
	}
	ints := []string{"0", "4096", " 4096 ", "+5", "-5", "1_000", "1__0", "_1", "1_", "+_1", "", " ", "4096M", "0x10",
		"007", "9223372036854775807", "9223372036854775808", "-9223372036854775808", "-9223372036854775809", "1.0",
		"\u00a012\u3000", "\x1c3\x1f", "-0", "--1", "1 2"}
	var b64 []string
	for _, in := range inputs {
		b64 = append(b64, base64.StdEncoding.EncodeToString(in))
	}
	script := `import base64, json, sys
inp = json.load(open(sys.argv[1], encoding="utf-8"))
out = {"decode": [], "lines": [], "strip": [], "kv": [], "int": []}
for b in inp["bytes"]:
    s = base64.b64decode(b).decode("utf-8", errors="replace")
    out["decode"].append(s)
    out["lines"].append(s.splitlines())
    out["strip"].append(s.strip())
    kv = {}
    for line in s.splitlines():
        key, sep, value = line.partition("=")
        if sep:
            kv[key.strip()] = value.strip()
    out["kv"].append([[k, v] for k, v in kv.items()])
for s in inp["ints"]:
    try:
        v = int(s)
        out["int"].append(str(v) if -2**63 <= v < 2**63 else None)
    except ValueError:
        out["int"].append(None)
json.dump(out, open(sys.argv[2], "w", encoding="utf-8"), ensure_ascii=True)
`
	var want struct {
		Decode []string     `json:"decode"`
		Lines  [][]string   `json:"lines"`
		Strip  []string     `json:"strip"`
		KV     [][][]string `json:"kv"`
		Int    []*string    `json:"int"`
	}
	if err := json.Unmarshal(python(t, script, map[string]any{"bytes": b64, "ints": ints}), &want); err != nil {
		t.Fatal(err)
	}
	for k, in := range inputs {
		s := DecodeUTF8Replace(in)
		if s != want.Decode[k] {
			t.Errorf("decode %d: %q want %q", k, s, want.Decode[k])
		}
		if got := SplitLines(s); fmt.Sprintf("%q", got) != fmt.Sprintf("%q", want.Lines[k]) {
			t.Errorf("lines %d: %q want %q", k, got, want.Lines[k])
		}
		if got := PyStrip(s); got != want.Strip[k] {
			t.Errorf("strip %d: %q want %q", k, got, want.Strip[k])
		}
		keys, values := ReadKeyValues(in, false)
		var kv [][]string
		for _, key := range keys {
			kv = append(kv, []string{key, values[key]})
		}
		if fmt.Sprintf("%q", kv) != fmt.Sprintf("%q", want.KV[k]) {
			t.Errorf("kv %d: %q want %q", k, kv, want.KV[k])
		}
	}
	for k, s := range ints {
		v, ok := PyInt(s)
		got := "<nil>"
		if ok {
			got = fmt.Sprint(v)
		}
		w := "<nil>"
		if want.Int[k] != nil {
			w = *want.Int[k]
		}
		if got != w {
			t.Errorf("PyInt(%q) = %s want %s", s, got, w)
		}
	}
	if keys, _ := ReadKeyValues([]byte("=x\na=1"), true); strings.Join(keys, ",") != "a" {
		t.Errorf("dropEmptyKeys: %v", keys)
	}
}

// ---- components --------------------------------------------------------------------------------------------------

// fakeSDK lays out the key files of every component; mark controls whether the headless qemu is "ours".
func fakeSDK(t *testing.T, sdk string, buildID string, mark bool) {
	t.Helper()
	writeFile(t, filepath.Join(sdk, "platform-tools", "adb.exe"), []byte("MZ"))
	writeFile(t, filepath.Join(sdk, "platform-tools", "source.properties"), []byte("Pkg.Revision=37.0.1\n"))
	emu := filepath.Join(sdk, "emulator")
	writeFile(t, filepath.Join(emu, "emulator.exe"), []byte("MZ"))
	writeFile(t, filepath.Join(emu, "qemu", "windows-x86_64", "qemu-system-x86_64.exe"), []byte("MZ"))
	body := []byte(strings.Repeat("x", 100) + "virtio-balloon-pci")
	if mark {
		body = append(body, []byte("\x00"+SelfBuiltMark+"\x00")...)
	}
	writeFile(t, filepath.Join(emu, "qemu", "windows-x86_64", "qemu-system-x86_64-headless.exe"), body)
	props := "Pkg.UserSrc=false\nPkg.Revision=37.2.10\nPkg.Path=emulator\nPkg.Desc=Android Emulator\n"
	if buildID != "" {
		props += "Pkg.BuildId=" + buildID + "\n"
	}
	writeFile(t, filepath.Join(emu, "source.properties"), []byte(props))
	img := filepath.Join(sdk, SystemImageDir)
	for _, f := range []string{"system.img", "vendor.img", "kernel-ranchu", "ramdisk.img"} {
		writeFile(t, filepath.Join(img, f), []byte("x"))
	}
	writeFile(t, filepath.Join(img, "source.properties"), []byte("Pkg.Revision=14\n"))
}

func TestComponentsAndEnsureMetadata(t *testing.T) {
	r := newRoot(t)
	comps := r.Components()
	if len(comps) != 5 {
		t.Fatalf("%d components", len(comps))
	}
	var ids []string
	for _, c := range comps {
		ids = append(ids, c.ID)
		if c.Installed {
			t.Errorf("%s installed in an empty root", c.ID)
		}
	}
	if strings.Join(ids, ",") != "platform-tools,emulator,system-image,launcher,ime" {
		t.Fatalf("ids %v", ids)
	}
	if got := strings.Join(r.Missing(), "、"); got != PlatformToolsName+"、"+EmulatorName+"、"+SystemImageName {
		t.Fatalf("missing %s", got)
	}

	fakeSDK(t, r.SDK(), "mas-35", true)
	writeFile(t, r.LauncherAPK(), make([]byte, LauncherAPKSize))
	writeFile(t, r.ImeAPK(), []byte("PK"))
	if m := r.Missing(); len(m) != 0 {
		t.Fatalf("missing %v", m)
	}
	comps = r.Components()
	want := []ComponentStatus{
		{ID: "platform-tools", Name: PlatformToolsName, Version: "37.0.1", Installed: true},
		{ID: "emulator", Name: EmulatorName, Version: "37.2.10", Build: "0.35.0", Installed: true},
		{ID: "system-image", Name: SystemImageName, Version: "14", Installed: true},
		{ID: "launcher", Name: LauncherName, Version: "0.2.12", Installed: true, Optional: true},
		{ID: "ime", Name: ImeName, Installed: true, Optional: true},
	}
	for k := range want {
		if comps[k] != want[k] {
			t.Errorf("component %d: %+v\nwant %+v", k, comps[k], want[k])
		}
	}

	wrote, err := r.EnsureMetadata()
	if err != nil || !wrote {
		t.Fatalf("EnsureMetadata: %v %v", wrote, err)
	}
	data := string(readFile(t, r.MetadataPath()))
	prefix := "{\r\n  \"components\": {\r\n    \"platform-tools\": \"37.0.1\",\r\n    \"emulator\": \"37.2.10\",\r\n" +
		"    \"system-image\": \"14\",\r\n    \"launcher\": \"0.2.12\"\r\n  },\r\n  \"addedAt\": \""
	if !strings.HasPrefix(data, prefix) || !strings.HasSuffix(data, "\"\r\n}") || len(data) != len(prefix)+19+4 {
		t.Fatalf("EnsureMetadata wrote %q", data)
	}
	if _, err := time.ParseInLocation("2006-01-02T15:04:05", data[len(prefix):len(prefix)+19], time.Local); err != nil {
		t.Fatalf("addedAt: %v", err)
	}
	if wrote, err := r.EnsureMetadata(); err != nil || wrote {
		t.Fatalf("second EnsureMetadata: %v %v", wrote, err)
	}

	// Launcher with the wrong size, emulator too old, not ours, unknown build.
	writeFile(t, r.LauncherAPK(), []byte("short"))
	if r.Components()[3].Installed {
		t.Error("short launcher counted")
	}
	fakeSDK(t, r.SDK(), "mas-24", true)
	if c := r.Components()[1]; c.Installed || c.Build != "0.24.0" {
		t.Errorf("old build: %+v", c)
	}
	fakeSDK(t, r.SDK(), "mas-35", false)
	if c := r.Components()[1]; c.Installed || c.Build != "" || c.Version != "37.2.10" {
		t.Errorf("not self-built: %+v", c)
	}
	fakeSDK(t, r.SDK(), "", true)
	if c := r.Components()[1]; c.Installed || c.Build != "" {
		t.Errorf("no build id: %+v", c)
	}
	os.Remove(filepath.Join(r.SDK(), "emulator", "emulator.exe"))
	if c := r.Components()[1]; c.Installed || c.Version != "" || r.EmulatorPresent() || r.EmulatorSelfBuilt() {
		t.Errorf("emulator.exe missing: %+v", c)
	}

	// sdkRoot moves the SDK components; launcher / ime stay in the root.
	dev := filepath.Join(t.TempDir(), "sdk-mas36")
	fakeSDK(t, dev, "mas-36", true)
	writeFile(t, r.MetadataPath(), []byte(`{"sdkRoot": `+jsonString(dev)+`}`))
	comps = r.Components()
	if !comps[0].LocalSDK || !comps[1].LocalSDK || !comps[2].LocalSDK || comps[3].LocalSDK || comps[4].LocalSDK ||
		!comps[1].Installed || comps[1].Build != "0.36.0" {
		t.Errorf("local sdk: %+v", comps)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestMissingError(t *testing.T) {
	r := newRoot(t)
	e := r.MissingError([]string{EmulatorName, PlatformToolsName})
	if e.Code != errs.NotReady {
		t.Fatal(e.Code)
	}
	want := "这个目录里缺少：Android 模拟器（emulator）、平台工具（adb）。" + PackageAdvice + "；" + FetchComponentsAdvice
	if e.Error() != want {
		t.Fatalf("got %s\nwant %s", e.Error(), want)
	}
	if got := r.MissingError([]string{EmulatorName}).Error(); got != "这个目录里缺少：Android 模拟器（emulator）。"+PackageAdvice {
		t.Fatalf("emulator only: %s", got)
	}
	if got := r.MissingError([]string{SystemImageName}).Error(); got != "这个目录里缺少：系统镜像 Android 14 Google APIs x86_64。"+FetchComponentsAdvice {
		t.Fatalf("image only: %s", got)
	}
	if !strings.Contains(FetchComponentsAdvice, "再运行一次 modavd check") || strings.Contains(FetchComponentsAdvice, "点「检查」") {
		t.Fatal("advice wording")
	}
	if got := r.MissingError(nil).Error(); !strings.HasPrefix(got, "这个目录里缺少：平台工具（adb）、") {
		t.Fatalf("nil list: %s", got)
	}
}

func TestSelfBuiltMarkAcrossChunks(t *testing.T) {
	old := selfBuiltChunk
	defer func() { selfBuiltChunk = old }()
	dir := t.TempDir()
	mark := SelfBuiltMark
	for chunk := 1; chunk <= 3*len(mark); chunk++ {
		selfBuiltChunk = chunk
		for offset := 0; offset < 2*len(mark)+3; offset++ {
			path := filepath.Join(dir, fmt.Sprintf("q-%d-%d.exe", chunk, offset))
			body := strings.Repeat("a", offset) + mark + strings.Repeat("b", 5)
			writeFile(t, path, []byte(body))
			if !HasSelfBuiltMark(path) {
				t.Fatalf("chunk %d offset %d: mark not found", chunk, offset)
			}
			broken := filepath.Join(dir, fmt.Sprintf("n-%d-%d.exe", chunk, offset))
			writeFile(t, broken, []byte(strings.Repeat("a", offset)+mark[:len(mark)-1]+"X"+mark[1:]))
			if HasSelfBuiltMark(broken) {
				t.Fatalf("chunk %d offset %d: false positive", chunk, offset)
			}
		}
	}
	selfBuiltChunk = old
	// The cache follows the file: rewriting it (new mtime / size) scans again.
	path := filepath.Join(dir, "cached.exe")
	writeFile(t, path, []byte("xx"+mark))
	if !HasSelfBuiltMark(path) {
		t.Fatal("mark")
	}
	writeFile(t, path, []byte("no mark here at all"))
	os.Chtimes(path, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if HasSelfBuiltMark(path) {
		t.Fatal("stale cache")
	}
	if HasSelfBuiltMark(filepath.Join(dir, "missing.exe")) || HasSelfBuiltMark(dir) {
		t.Fatal("missing file / directory counted")
	}
}

func TestModBuildAndVersionText(t *testing.T) {
	r := newRoot(t)
	if _, ok := r.ModBuild(); ok || r.VersionText() != "" || r.EmulatorVersion() != "" {
		t.Fatal("empty root")
	}
	props := filepath.Join(r.SDK(), "emulator", "source.properties")
	for id, want := range map[string]int{"mas-35": 35, "  mas-32  ": 32, "mas-007": 7, "mas-": -1, "mas-35b": -1,
		"35": -1, "MAS-35": -1, "mas-x": -1, "mas-99999999999999999999": -1} {
		writeFile(t, props, []byte("Pkg.Revision=37.2.10\r\nPkg.BuildId="+id+"\r\n"))
		got, ok := r.ModBuild()
		if !ok {
			got = -1
		}
		if got != want {
			t.Errorf("BuildId %q: %d want %d", id, got, want)
		}
	}
	if r.EmulatorVersion() != "37.2.10" {
		t.Fatal("EmulatorVersion")
	}
	writeFile(t, props, []byte("Pkg.BuildId=mas-35\n"))
	manifest := r.ManifestPath()
	for content, want := range map[string]string{
		"":                                      "0.35.0", // no manifest file (removed below)
		`{"version": "v0.35.0"}`:                "0.35.0",
		`{"version": "v0.35.2"}`:                "0.35.2",
		"\xef\xbb\xbf{\"version\": \"0.35.1\"}": "0.35.1",
		`{"version": " v0.035.01 "}`:            "0.35.1",
		`{"version": "v0.34.1"}`:                "0.35.0", // manifest older than the emulator: the emulator wins
		`{"version": "v1.35.0"}`:                "1.35.0",
		`{"version": "v37.2.10-mas35"}`:         "0.35.0",
		`{"version": "v37.2.10-mas34"}`:         "0.35.0",
		`{"version": 35}`:                       "0.35.0",
		`{"version": "v0.35"}`:                  "0.35.0",
		`[1]`:                                   "0.35.0",
		`{"version": `:                          "0.35.0",
	} {
		if content == "" {
			os.Remove(manifest)
		} else {
			writeFile(t, manifest, []byte(content))
		}
		if got := r.VersionText(); got != want {
			t.Errorf("manifest %q: %q want %q", content, got, want)
		}
	}
	writeFile(t, props, []byte("Pkg.BuildId=12345\n"))
	writeFile(t, manifest, []byte(`{"version": "v0.35.0"}`))
	if got := r.VersionText(); got != "" {
		t.Fatalf("no build: %q", got)
	}
	for text, want := range map[string][3]int{"v0.32.1": {0, 32, 1}, "0.33.0": {0, 33, 0}, "v37.2.10-mas32": {0, 32, 0}} {
		a, b, c, ok := ParseVersion(text)
		if !ok || [3]int{a, b, c} != want {
			t.Errorf("ParseVersion(%s)", text)
		}
	}
	for _, bad := range []string{"", "v0.32", "x0.32.1", "v0.32.1-rc1", "37.2.10-mas", "v0.32.1.0"} {
		if _, _, _, ok := ParseVersion(bad); ok {
			t.Errorf("ParseVersion(%q) ok", bad)
		}
	}
}

// TestProductionRootReadOnly reads (never writes) the production root when it is there.
func TestProductionRootReadOnly(t *testing.T) {
	const prod = `D:\ModAVD`
	if _, err := os.Stat(filepath.Join(prod, MetadataFile)); err != nil {
		t.Skip("no production root")
	}
	r, err := New(prod)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(r.MetadataPath())
	v, err := r.ReadMetadata()
	if err != nil {
		t.Fatal(err)
	}
	// What UpdateMetadata would write for it is the file AUTO-MAS wrote (unless someone edited it by hand since).
	if raw := readFile(t, r.MetadataPath()); bytes.Equal(metadataBytes(v), raw) {
		t.Logf("mas-avd.json: writer output is byte-identical (%d bytes)", len(raw))
	} else {
		t.Logf("note: mas-avd.json is not what AUTO-MAS writes for its content (edited by hand?)")
	}
	b, ok := r.ModBuild()
	t.Logf("build %d %v, version %q, emulator %q, adb %d / %d, missing %v", b, ok, r.VersionText(),
		r.EmulatorVersion(), r.AdbServerPort(), r.ScriptAdbServerPort(), r.Missing())
	for _, c := range r.Components() {
		t.Logf("%+v", c)
	}
	if ok && !r.EmulatorSelfBuilt() {
		t.Error("production emulator not recognised as self-built")
	}
	after, _ := os.Stat(r.MetadataPath())
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("production mas-avd.json changed")
	}
}
