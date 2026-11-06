package report

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// The outputs compared line by line with what report-config.ps1 and collect-info.ps1 print for the same scenario
// (testdata/golden, written by testdata/gen_golden.ps1 from the scripts themselves). Bridge classification lines
// now name the actual numbered instance; the original archive entries remain unchanged.

// testTempDir is a fresh directory, removed afterwards: under $MODAVD_TEST_TMP\report when that is set, otherwise
// the default OS temporary directory. Its 8.3 short names are expanded (%TEMP% is an 8.3 short name on GitHub's runners), as the
// scripts' [IO.Path]::GetFullPath expands them in the paths they print.
func testTempDir(t *testing.T) string {
	t.Helper()
	env := os.Getenv("MODAVD_TEST_TMP")
	if env == "" {
		return longPath(testutil.TempDir(t))
	}
	base := filepath.Join(env, "report")
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	return longPath(testutil.MkdirTemp(t, base, strings.ReplaceAll(t.Name(), "/", "_")+"-"))
}

func goldenPath(scenario string, parts ...string) string {
	return filepath.Join(append([]string{"testdata", "golden", scenario}, parts...)...)
}

var stampLine = regexp.MustCompile(`生成时间：\S+ \S+ ([+-])(\d\d):(\d\d)`)

// useGoldenZone makes time.Local the UTC offset the golden files were written in (their 生成时间 line), so that the
// wall-clock times of the scenario print the same here.
func useGoldenZone(t *testing.T, scenario string) {
	t.Helper()
	b, err := os.ReadFile(goldenPath(scenario, "collect", "summary.txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := stampLine.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no 生成时间 line in the golden summary")
	}
	h, _ := strconv.Atoi(m[2])
	mi, _ := strconv.Atoi(m[3])
	off := h*3600 + mi*60
	if m[1] == "-" {
		off = -off
	}
	old := time.Local
	time.Local = time.FixedZone("golden", off)
	t.Cleanup(func() { time.Local = old })
}

// setLimits lowers the size limits as gen_golden.ps1 does for the scenario.
func setLimits(t *testing.T, sc *scenario) {
	t.Helper()
	if sc.Limits == nil {
		return
	}
	a, b, c := logCapBytes, masLogTailBytes, dumpMaxBytes
	logCapBytes, masLogTailBytes, dumpMaxBytes = sc.Limits.LogCap, sc.Limits.MasTail, sc.Limits.DumpMax
	t.Cleanup(func() { logCapBytes, masLogTailBytes, dumpMaxBytes = a, b, c })
}

// Derived scenarios store only changed generated text. The generator verifies byte equality before omitting a file.
func goldenSource(t *testing.T, name string, parts ...string) string {
	t.Helper()
	p := goldenPath(name, parts...)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	b, err := os.ReadFile("testdata/scenarios.json")
	if err != nil {
		t.Fatal(err)
	}
	var scenarios map[string]json.RawMessage
	if err := json.Unmarshal(b, &scenarios); err != nil {
		t.Fatal(err)
	}
	var sc struct{ Extends string }
	if err := json.Unmarshal(scenarios[name], &sc); err != nil {
		t.Fatal(err)
	}
	if sc.Extends == "" {
		t.Fatalf("no golden for %s", p)
	}
	return goldenSource(t, sc.Extends, parts...)
}

// golden reads a golden file with its placeholders filled in for f; bom says the script wrote a UTF-8 BOM.
func (f *fixture) golden(t *testing.T, scenario string, parts ...string) (text string, bom bool) {
	t.Helper()
	b, err := os.ReadFile(goldenSource(t, scenario, parts...))
	if err != nil {
		t.Fatal(err)
	}
	text = string(b)
	text, bom = strings.CutPrefix(text, "{{BOM}}")
	text = strings.ReplaceAll(text, "{{BASE}}", f.base)
	text = strings.ReplaceAll(text, "{{DRIVE}}", filepath.VolumeName(f.base)[:1])
	text = strings.ReplaceAll(text, "{{ADBPORT}}", strconv.Itoa(f.adbPort))
	return text, bom
}

func lines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// compareLines reports every line that differs (want is the script's).
func compareLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	n := max(len(got), len(want))
	bad := 0
	for i := 0; i < n; i++ {
		var g, w string
		if i < len(got) {
			g = got[i]
		}
		if i < len(want) {
			w = want[i]
		}
		if g != w {
			if bad++; bad <= 15 {
				t.Errorf("%s line %d:\n  got  %q\n  want %q", what, i+1, g, w)
			}
		}
	}
	if len(got) != len(want) {
		t.Errorf("%s: %d lines, the script %d", what, len(got), len(want))
	}
}

// modavdLine is what modavd writes on purpose instead of a line of collect-info.ps1's summary.txt:
//   - the PowerShell that ran the script: modavd's version and Go version;
//   - the adb.exe the live section runs: modavd speaks the adb host protocol itself (no adb.exe, never a server of
//     its own), to the server that listens on the port.
func (f *fixture) modavdLine(line string) string {
	if line == "PowerShell：{{PSVERSION}}" {
		return "modavd：" + buildinfo.Version + "（" + runtime.Version() + "）"
	}
	if strings.HasPrefix(line, "adb：") && strings.Contains(line, "  -P "+strconv.Itoa(f.adbPort)+"（") {
		var pid uint32
		for _, l := range f.sc.Listeners {
			if string(l.LocalPort) == strconv.Itoa(f.adbPort) {
				pid = l.OwningProcess
			}
		}
		return fmt.Sprintf("adb：按 adb 主机协议直接连 -P %d（pid %d 的 adb server；不调用 adb.exe，不会起 server）", f.adbPort, pid)
	}
	return line
}

// crashSource is the file the zip entry crash/<db>/<name> was copied from: <name> anywhere under <db> in one of the
// two crash folders (%LOCALAPPDATA%\Temp\AndroidEmulator, %TEMP%\AndroidEmulator); "" when there is none.
func (f *fixture) crashSource(entry string) string {
	db, name, _ := strings.Cut(strings.TrimPrefix(entry, "crash/"), "/")
	var dirs []string
	if v := f.env["LOCALAPPDATA"]; v != "" {
		dirs = append(dirs, filepath.Join(v, "Temp"))
	}
	if v := f.env["TEMP"]; v != "" {
		dirs = append(dirs, v)
	}
	for _, dir := range dirs {
		found := ""
		filepath.WalkDir(filepath.Join(dir, "AndroidEmulator", db), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.EqualFold(d.Name(), name) {
				found = p
				return filepath.SkipAll
			}
			return nil
		})
		if found != "" {
			return found
		}
	}
	return ""
}

func containsLine(ls []string, s string) bool {
	for _, l := range ls {
		if l == s {
			return true
		}
	}
	return false
}

func readZip(t *testing.T, path string) (names []string, files map[string][]byte) {
	t.Helper()
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	files = map[string][]byte{}
	for _, e := range z.File {
		r, err := e.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, e.Name)
		files[e.Name] = b
	}
	return names, files
}

func runCollect(t *testing.T, f *fixture) (*CollectResult, []string, map[string][]byte) {
	t.Helper()
	res, err := collect(context.Background(), f.root, f.host, CollectOptions{
		OutDir: filepath.Join(f.base, "out"), StageDir: filepath.Join(f.base, "stage"), NoExplorer: true,
		now: func() time.Time { return f.now }, getenv: f.getenv, pid: 4321,
	})
	if err != nil {
		t.Fatal(err)
	}
	names, files := readZip(t, res.ZipPath)
	return res, names, files
}

// shortname: %TEMP% is %LOCALAPPDATA%\Temp by an 8.3 short name; the script finds its crash dumps once, by the long
// name.
func TestCollectMatchesScript(t *testing.T) {
	for _, name := range collectScenarios(t) {
		t.Run(name, func(t *testing.T) {
			useGoldenZone(t, name)
			f := newFixture(t, name, testTempDir(t), "")
			setLimits(t, f.sc)
			guardDials(t, f.adbPort)
			res, names, files := runCollect(t, f)

			want, _ := f.golden(t, name, "collect-entries.txt")
			compareLines(t, "zip entries", names, lines(want))

			// summary.txt: the script's lines, but for modavdLine's
			wantSummary, bom := f.golden(t, name, "collect", "summary.txt")
			got := string(files["summary.txt"])
			if gotBOM := strings.HasPrefix(got, "\ufeff"); gotBOM != bom {
				t.Errorf("summary.txt BOM %v, the script %v", gotBOM, bom)
			}
			var wantLines []string
			for _, l := range lines(wantSummary) {
				wantLines = append(wantLines, f.modavdLine(l))
			}
			// ModAVD adds a diagnostic state line for each canonical instance.
			var scriptLines []string
			var stateLines []string
			for _, line := range lines(strings.TrimPrefix(got, "\ufeff")) {
				if strings.HasSuffix(line, "：诊断模式 关") {
					stateLines = append(stateLines, line)
					continue
				}
				scriptLines = append(scriptLines, line)
			}
			entries, err := os.ReadDir(filepath.Join(f.root, "avd"))
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			var expectedStates []string
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".ini") {
					continue
				}
				name := strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
				if _, ok := diagnosticIndex(name); ok {
					expectedStates = append(expectedStates, name+"：诊断模式 关")
				}
			}
			sort.Strings(stateLines)
			sort.Strings(expectedStates)
			compareLines(t, "diagnostic state", stateLines, expectedStates)
			compareLines(t, "summary.txt", scriptLines, wantLines)
			if !strings.HasSuffix(got, "\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
				t.Error("summary.txt lines do not all end with CRLF")
			}

			// Compare every generated text entry, including unchanged references inherited from a base scenario.
			for _, entry := range names {
				if !(strings.HasSuffix(entry, "_files.txt") || strings.HasPrefix(entry, "live/") && strings.HasSuffix(entry, ".txt")) {
					continue
				}
				want, bom := f.golden(t, name, "collect", filepath.FromSlash(entry))
				got := files[entry]
				if bom {
					want = "\ufeff" + want
				}
				if string(got) != want {
					t.Errorf("%s:\n got  %q\n want %q", entry, got, want)
				}
			}

			// copies: byte for byte, AUTO-MAS logs only their tail, the screenshot as the device sent it
			for entry, b := range files {
				var src string
				switch {
				case strings.HasPrefix(entry, "logs/"), strings.HasPrefix(entry, "avd/") && !strings.HasSuffix(entry, "_files.txt"):
					src = filepath.Join(f.root, filepath.FromSlash(entry))
				case entry == root.MetadataFile:
					src = filepath.Join(f.root, entry)
				case strings.HasPrefix(entry, "mas/"):
					src = filepath.Join(f.base, "mas", "debug", strings.TrimPrefix(entry, "mas/"))
				case strings.HasPrefix(entry, "crash/"):
					if src = f.crashSource(entry); src == "" {
						t.Errorf("%s: no such dump in the scenario's crash folders", entry)
						continue
					}
				default:
					continue
				}
				want, err := os.ReadFile(src)
				if err != nil {
					t.Errorf("%s: %v", entry, err)
					continue
				}
				if f.sc.Limits != nil && int64(len(want)) > f.sc.Limits.MasTail && strings.HasPrefix(entry, "mas/") {
					want = want[int64(len(want))-f.sc.Limits.MasTail:]
				}
				if !bytes.Equal(b, want) {
					t.Errorf("%s is not a copy of %s", entry, src)
				}
			}
			if png, ok := files["live/screen-mas_1.png"]; ok {
				want, _ := base64.StdEncoding.DecodeString(f.sc.Adb.Serials["emulator-20010"].Exec["screencap -p"].B64)
				if !bytes.Equal(png, want) {
					t.Error("live/screen-mas_1.png is not the device's screenshot")
				}
			}

			// the end of the console output: zip path, the privacy note word for word
			console, _ := f.golden(t, name, "collect-console.txt")
			if !containsLine(lines(console), "文件："+res.ZipPath) {
				t.Errorf("zip %s, the script printed:\n%s", res.ZipPath, console)
			}
			if !strings.Contains(console, PrivacyNote) || res.PrivacyNote != PrivacyNote {
				t.Error("the privacy note is not the one collect-info.ps1 prints")
			}
			if exists(res.Stage) || res.StageKept {
				t.Errorf("stage %s left behind", res.Stage)
			}
			for _, p := range f.host.programs() {
				if strings.EqualFold(filepath.Base(p), "adb.exe") {
					t.Errorf("started %s", p)
				}
			}
		})
	}
}

func TestReportConfigMatchesScript(t *testing.T) {
	for _, name := range []string{"full", "bare", "shortname"} {
		t.Run(name, func(t *testing.T) {
			useGoldenZone(t, name)
			f := newFixture(t, name, testTempDir(t), "")
			dialed := guardDials(t, f.adbPort)
			var copied []string
			r := &root.Root{Dir: f.root}
			res, err := BuildConfig(context.Background(), r, ConfigOptions{Host: f.host, clipboard: func(s string) error {
				copied = append(copied, s)
				return failIf(f.sc.ClipboardErr)
			}})
			if err != nil {
				t.Fatal(err)
			}
			// the script prints a blank line, the lines, a blank line and what happened to the clipboard
			want, _ := f.golden(t, name, "report-config.txt")
			wl := lines(want)
			if len(wl) < 3 || wl[0] != "" || wl[len(wl)-2] != "" {
				t.Fatalf("unexpected golden shape: %q", wl)
			}
			compareLines(t, "report-config", res.Lines, wl[1:len(wl)-2])
			if res.Message != wl[len(wl)-1] {
				t.Errorf("message %q, the script %q", res.Message, wl[len(wl)-1])
			}
			if res.Text != strings.Join(wl[1:len(wl)-2], "\r\n") || len(copied) != 1 || copied[0] != res.Text {
				t.Errorf("text / clipboard %q, %q", res.Text, copied)
			}
			for _, d := range *dialed {
				if d != "127.0.0.1:"+strconv.Itoa(f.adbPort) {
					t.Errorf("dialed %s", d)
				}
			}
			if name == "bare" && len(*dialed) != 0 {
				t.Errorf("no adb server listens, yet dialed %v", *dialed)
			}
		})
	}
}
