package report

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"modavd/internal/jsonord"
	"modavd/internal/root"
)

func TestFormatN(t *testing.T) {
	// '{0:N<d>}' -f v on Windows PowerShell 5.1
	for _, c := range []struct {
		v    float64
		d    int
		want string
	}{
		{206.38, 0, "206"}, {2.5, 0, "3"}, {3.5, 0, "4"}, {0.05, 1, "0.1"}, {1234567, 0, "1,234,567"},
		{4.8828125, 1, "4.9"}, {63.11, 2, "63.11"}, {0, 1, "0.0"}, {999.96, 1, "1,000.0"}, {-0.04, 1, "0.0"},
	} {
		if got := formatN(c.v, c.d); got != c.want {
			t.Errorf("formatN(%v, %d) = %q, want %q", c.v, c.d, got, c.want)
		}
	}
	for n, want := range map[float64]string{0: "0 B", 1000: "1000 B", 1024: "1.0 KB", 5000: "4.9 KB", 6000: "5.9 KB",
		3 << 20: "3.0 MB", 221600000000: "206.38 GB"} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%v) = %q, want %q", n, got, want)
		}
	}
}

func TestCultureSort(t *testing.T) {
	// Sort-Object on Windows PowerShell 5.1 (the zip entry order of collect-info.ps1)
	in := []string{"mas-avd.json", "mas/app.log", "logs/mas_1-20260106-080000.log", "logs/mas_10-20260114-090000.log",
		"live/screen-mas_1.png", "live/mas_3-getprop.txt", "logs/logcat-mas_1-20260113-080000.txt",
		"avd/mas_1.ini", "avd/mas_1.avd/_files.txt", "COM.MIHOYO.HKRPG", "com.example.zeta", "org.modavd.ime"}
	want := []string{"avd/mas_1.avd/_files.txt", "avd/mas_1.ini", "com.example.zeta", "COM.MIHOYO.HKRPG",
		"live/mas_3-getprop.txt", "live/screen-mas_1.png", "logs/logcat-mas_1-20260113-080000.txt",
		"logs/mas_10-20260114-090000.log", "logs/mas_1-20260106-080000.log", "mas/app.log", "mas-avd.json", "org.modavd.ime"}
	got := append([]string(nil), in...)
	sortCulture(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// mas-avd.json's adbServerPort as collect-info.ps1's Resolve-MetadataPort takes it, and as its summary line shows a
// value it does not take (ConvertTo-Json -Compress).
func TestScriptPort(t *testing.T) {
	for raw, want := range map[string]int{
		`20068`: 20068, `"20068"`: 20068, `" 20068 "`: 20068, `20068.9`: 20068, `"+20068"`: 20068, `20052`: 20052,
		`20050`: 20050, `20010`: 0, `20012`: 0, `20092`: 0, `20093`: 20093, `1023`: 0, `1024`: 1024, `65535`: 65535,
		`65536`: 0, `5037`: 0, `"5037"`: 0, `true`: 0, `"0x4E84"`: 0, `"20068.0"`: 0, `null`: 0, `[20068]`: 0, `{}`: 0,
	} {
		v, err := jsonord.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := scriptPort(root.PortPlan{Base: root.DefaultPortBase}, v)
		if !ok {
			got = 0
		}
		if got != want {
			t.Errorf("%s: %d, want %d", raw, got, want)
		}
	}
	// running instances: with portBase 21000, the same-named qemu of another root (on its ports) is not this root's
	for _, c := range []struct {
		base, port int
		avd        string
		want       bool
	}{
		{root.DefaultPortBase, 20060, "mas_6", false}, {root.DefaultPortBase, 21060, "mas_6", false},
		{21000, 21060, "mas_6", false}, {21000, 20060, "mas_6", true}, {21000, 20060, "MAS_6", true},
		{21000, 0, "mas_6", true}, {21000, 20060, "mas_x", false},
	} {
		if got := otherRootsInstance(root.PortPlan{Base: c.base}, c.avd, c.port); got != c.want {
			t.Errorf("otherRootsInstance(%d, %s, %d) = %v", c.base, c.avd, c.port, got)
		}
	}
	// report-config's index of a device: counted in the root's plan, so the production root's emulator-20060 (listed
	// by its servers, which report-config also asks) is no instance of a root with portBase 21000
	for _, c := range []struct {
		base, port int
		want       string
		ok         bool
	}{
		{root.DefaultPortBase, 20060, "6", true}, {root.DefaultPortBase, 20065, "", false},
		{21000, 21060, "6", true}, {21000, 20060, "", false}, {21000, 21000, "0", true},
	} {
		if got, ok := configIndex(root.PortPlan{Base: c.base}, c.port); got != c.want || ok != c.ok {
			t.Errorf("configIndex(%d, %d) = %q %v", c.base, c.port, got, ok)
		}
	}
	// a root with portBase 21000: its own instance ports are refused, the default plan's are not (ModAVD/ModAVD#75)
	for raw, want := range map[string]int{`20010`: 20010, `21010`: 0, `21092`: 0, `21093`: 21093} {
		v, _ := jsonord.Parse([]byte(raw))
		if got, ok := scriptPort(root.PortPlan{Base: 21000}, v); (ok && got != want) || (!ok && want != 0) {
			t.Errorf("plan 21000: %s: %d %v, want %d", raw, got, ok, want)
		}
	}
	for raw, want := range map[string]string{`"5037"`: `"5037"`, `5037`: `5037`, `1.5e3`: `1500`, `[1]`: `1`,
		`[1,"a<b"]`: `[1,"a\u003cb"]`, `true`: `true`, `null`: ``, `{"a":1}`: `{"a":1}`} {
		v, err := jsonord.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := psJSON(v); got != want {
			t.Errorf("psJSON(%s) = %s, want %s", raw, got, want)
		}
	}
}

func TestForbiddenEntry(t *testing.T) {
	for _, name := range []string{"mas/config/accounts.json", "mas/data/x.db", "avd/mas_1.avd/snapshots/a",
		"avd/mas_1.avd/userdata-qemu.img", "x.qcow2", "avd/mas_1.avd/encryptionkey.IMG"} {
		if !forbiddenEntry(name) {
			t.Errorf("%s was not filtered", name)
		}
	}
	for _, name := range []string{"summary.txt", "logs/mas_1-20260101-000000.log", "avd/mas_1.ini", "mas/app.log"} {
		if forbiddenEntry(name) {
			t.Errorf("%s was filtered", name)
		}
	}
}

func TestZipStageOrderAndFilter(t *testing.T) {
	stage := t.TempDir()
	for _, name := range []string{"mas-avd.json", "mas/app.log", "logs/a.log", "avd/mas_1.ini", "summary.txt",
		"avd/mas_1.avd/config.ini", "avd/mas_1.avd/userdata-qemu.img", "mas/config/account", "mas/data/account"} {
		p := filepath.Join(stage, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dst := filepath.Join(t.TempDir(), "x.zip")
	entries, err := zipStage(dst, stage)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"summary.txt", "avd/mas_1.avd/config.ini", "avd/mas_1.ini", "logs/a.log", "mas/app.log", "mas-avd.json"}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("entries = %q, want %q", entries, want)
	}
	z, err := zip.OpenReader(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	var names []string
	for _, f := range z.File {
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("zip = %q", names)
	}
}

// The games of a running instance, asked of an adb server that answers the host protocol.
func TestGamesOverAdbHostProtocol(t *testing.T) {
	exit1 := 1
	adb := startFakeAdb(t, scenarioAdb{
		Devices: "emulator-20020\tdevice\n",
		Serials: map[string]scenarioSerial{"emulator-20020": {Features: "shell_v2", Shell: map[string]scenarioShell{
			"getprop sys.boot_completed": {Out: "1\r\n"},
			"pm list packages -3":        {Out: "package:com.shenlan.m.reverse1999\npackage:de.jrpie.android.launcher\npackage:com.hypergryph.arknights.bilibili\n"},
			"dumpsys package com.shenlan.m.reverse1999 2>/dev/null | grep -m1 versionName=":         {Out: "      versionName=1.9.0\n"},
			"dumpsys package com.hypergryph.arknights.bilibili 2>/dev/null | grep -m1 versionName=": {Err: "Broken pipe\n", Exit: &exit1},
		}}},
	})
	dialed := guardDials(t, adb.port())
	ctx := context.Background()
	list, err := adbDevices(ctx, adb.port())
	if err != nil || list != "emulator-20020\tdevice\n" {
		t.Fatalf("devices %q, %v", list, err)
	}
	got := gamesOf(ctx, configTarget{port: adb.port(), serial: "emulator-20020"})
	want := []string{"明日方舟（B 服）", "重返未来：1999 1.9.0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("games %q, want %q", got, want)
	}
	if got := gamesOf(ctx, configTarget{port: adb.port(), serial: "emulator-20030"}); len(got) != 1 ||
		got[0] != "读不到（adb server 返回 FAIL：device 'emulator-20030' not found）" {
		t.Errorf("unknown device: %q", got)
	}
	_, reqs := adb.seen()
	for _, r := range reqs {
		if !strings.HasPrefix(r, "host:") && !strings.HasPrefix(r, "emulator-20020 shell:") {
			t.Errorf("request %q", r)
		}
	}
	if _, err := dialAdb(ctx, 5037); err == nil {
		t.Error("dialAdb went to 5037")
	}
	for _, d := range *dialed {
		if strings.HasSuffix(d, ":5037") {
			t.Errorf("dialed %s", d)
		}
	}
}

// The live commands as adb.exe runs them: the shell protocol (stdout, stderr and exit code apart) when the device
// has shell_v2, the old one (merged, exit code 0) when not; exec-out raw.
func TestAdbLiveCommands(t *testing.T) {
	exit3 := 3
	adb := startFakeAdb(t, scenarioAdb{Serials: map[string]scenarioSerial{
		"emulator-20010": {Features: "cmd,shell_v2", Shell: map[string]scenarioShell{
			"getprop": {Out: "a\n", Err: "b\n", Exit: &exit3},
			`export ANDROID_LOG_TAGS="''"; exec logcat '-d' '-b' 'crash'`: {Out: "crash\n"},
		}, Exec: map[string]struct{ B64 string }{"screencap -p": {B64: "iVBORw=="}}},
		"emulator-20030": {Features: "cmd", Shell: map[string]scenarioShell{"getprop": {Out: "a\n", Err: "b\n", Exit: &exit3}}},
	}})
	guardDials(t, adb.port())
	ctx := context.Background()
	r := adbLive(ctx, adb.port(), "emulator-20010", time.Minute, nil, "shell", "getprop")
	if string(r.Stdout) != "a\n" || r.Stderr != "b\n" || r.ExitCode != 3 || r.TimedOut {
		t.Errorf("v2 shell: %+v", r)
	}
	r = adbLive(ctx, adb.port(), "emulator-20010", time.Minute, nil, "logcat", "-d", "-b", "crash")
	if string(r.Stdout) != "crash\n" || r.ExitCode != 0 {
		t.Errorf("logcat: %+v", r)
	}
	r = adbLive(ctx, adb.port(), "emulator-20030", time.Minute, nil, "shell", "getprop")
	if string(r.Stdout) != "a\nb\n" || r.Stderr != "" || r.ExitCode != 0 {
		t.Errorf("old shell: %+v", r)
	}
	var png strings.Builder
	r = adbLive(ctx, adb.port(), "emulator-20010", time.Minute, &png, "exec-out", "screencap", "-p")
	if png.String() != "\x89PNG" || r.ExitCode != 0 {
		t.Errorf("exec-out: %q %+v", png.String(), r)
	}
	r = adbLive(ctx, adb.port(), "emulator-20099", time.Minute, nil, "shell", "getprop")
	if r.ExitCode != 1 || !strings.Contains(r.Stderr, "not found") {
		t.Errorf("unknown device: %+v", r)
	}
}
