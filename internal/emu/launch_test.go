//go:build windows

package emu

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// Reference lists printed by AUTO-MAS build_launch_args("mas_3", 20030, memory_mb=4096, headless=...).
var (
	masArgsHeadless = []string{"-avd", "mas_3", "-no-snapshot", "-gpu", "host", "-ports", "20030,20031", "-grpc", "20032",
		"-grpc-use-token", "-audio", "none", "-memory", "4096", "-no-window", "-no-boot-anim",
		"-feature", "-QuickbootFileBacked", "-feature", "-VirtioSndCard", "-feature", "VulkanBatchedDescriptorSetUpdate",
		"-qemu", "-global", "virtio-blk-device.write-cache=off", "-global", "virtio-blk-device.config-wce=off",
		"-device", "virtio-balloon-pci,free-page-reporting=on"}
	masArgsWindow = []string{"-avd", "mas_3", "-no-snapshot", "-gpu", "host", "-ports", "20030,20031", "-grpc", "20032",
		"-grpc-use-token", "-audio", "none", "-memory", "4096",
		"-feature", "-QuickbootFileBacked", "-feature", "-VirtioSndCard", "-feature", "VulkanBatchedDescriptorSetUpdate",
		"-qemu", "-global", "virtio-blk-device.write-cache=off", "-global", "virtio-blk-device.config-wce=off",
		"-device", "virtio-balloon-pci,free-page-reporting=on"}
)

func TestBuildLaunchArgs(t *testing.T) {
	if got := buildLaunchArgs("mas_3", 20030, 4096, true); !reflect.DeepEqual(got, masArgsHeadless) {
		t.Errorf("headless:\n got %q\nwant %q", got, masArgsHeadless)
	}
	if got := buildLaunchArgs("mas_3", 20030, 4096, false); !reflect.DeepEqual(got, masArgsWindow) {
		t.Errorf("window:\n got %q\nwant %q", got, masArgsWindow)
	}
	// another instance: only the name, ports and memory move
	got := buildLaunchArgs("mas_0", 20000, 6144, true)
	if got[1] != "mas_0" || got[6] != "20000,20001" || got[8] != "20002" || got[13] != "6144" {
		t.Errorf("mas_0: %q", got)
	}
}

// lastEnv is the value the child sees for key: the last entry of that name, compared case-insensitively (what
// win.StartOrphan / StartDetached hand to the child).
func lastEnv(env []string, key string) (string, bool) {
	for k := len(env) - 1; k >= 0; k-- {
		name, value, _ := strings.Cut(env[k], "=")
		if strings.EqualFold(name, key) {
			return value, true
		}
	}
	return "", false
}

func TestEmulatorEnv(t *testing.T) {
	dir := t.TempDir()
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	// a user's own values must not win
	t.Setenv("android_emu_vk_hide_rebar", "0")
	t.Setenv("__GL_SHADER_DISK_CACHE_PATH", `C:\elsewhere`)
	m := &Manager{Root: r, Log: logx.Nop(), ADB: adb.New(r, logx.Nop())}
	cache := r.ShaderCacheDir()
	env := m.emulatorEnv(cache)
	for key, want := range map[string]string{
		"__GL_SHADER_DISK_CACHE":              "1",
		"__GL_SHADER_DISK_CACHE_PATH":         filepath.Join(dir, "shader-cache"),
		"__GL_SHADER_DISK_CACHE_SKIP_CLEANUP": "1",
		"ANDROID_EMU_VK_HIDE_REBAR":           "1",
		// the private adb environment underneath
		"ANDROID_AVD_HOME":        filepath.Join(dir, "avd"),
		"ANDROID_ADB_SERVER_PORT": strconv.Itoa(r.AdbServerPort()),
	} {
		if got, ok := lastEnv(env, key); !ok || got != want {
			t.Errorf("%s = %q (%v), want %q", key, got, ok, want)
		}
	}
}

func TestLogTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mas_0-20261028-150405.log")
	var b strings.Builder
	for k := 1; k <= 12; k++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", k%3))
		b.WriteString("\r\n\r\n   \n")
	}
	b.WriteString("ERROR | last one")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := logTail(path, 8)
	parts := strings.Split(got, " | ")
	// the last 8 non-empty lines are "line " (k=6) ... (k=12) and "ERROR | last one", which contains the separator;
	// lines are kept as they are (trailing space included), blank ones dropped
	if len(parts) != 9 || parts[len(parts)-1] != "last one" || parts[0] != "line " || parts[1] != "line x" {
		t.Errorf("logTail = %q", got)
	}
	if logTail(filepath.Join(dir, "missing.log"), 8) != "" || logTail("", 8) != "" {
		t.Error("missing file should give an empty tail")
	}

	// only the last 64 KiB are read
	big := filepath.Join(dir, "big.log")
	data := strings.Repeat("filler filler filler\n", 10000) + "tail-a\ntail-b\n"
	if err := os.WriteFile(big, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := logTail(big, 2); got != "tail-a | tail-b" {
		t.Errorf("big logTail = %q", got)
	}
}

func TestOutputLines(t *testing.T) {
	cases := map[string][]string{
		"":             nil,
		"a":            {"a"},
		"a\r\nb\n":     {"a", "b"},
		"a\rb\r\n\nc":  {"a", "b", "", "c"},
		"x\n\n":        {"x", ""},
		"  k: v  \r\n": {"  k: v  "},
	}
	for in, want := range cases {
		if got := outputLines(in); !reflect.DeepEqual(got, want) {
			t.Errorf("outputLines(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFileContains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mas_1-x.log")
	text := "INFO    | something\nINFO    | modavd_ime: host input method pipe service registered\n"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileContains(path, hostImeChannelLine) {
		t.Error("channel line not found")
	}
	if fileContains(path, "nothing like this") || fileContains("", hostImeChannelLine) {
		t.Error("false positive")
	}
}

// TestOtherRootQemu: only a qemu that is instance i by name and ports and that the root check gives to another root.
func TestOtherRootQemu(t *testing.T) {
	c := root.ConsolePort(3)
	ports := strconv.Itoa(c) + "," + strconv.Itoa(c+1)
	qemu := win.Process{PID: 4242, Name: "qemu-system-x86_64-headless.exe", Created: 1}
	exe := `D:\ModAVD\sdk\emulator\qemu\windows-x86_64\qemu-system-x86_64-headless.exe`
	args := []string{exe, "-avd", "mas_3", "-ports", ports, "-grpc", strconv.Itoa(c + 2)}
	notOurs := func(win.Process) bool { return false }
	ours := func(win.Process) bool { return true }
	cases := []struct {
		name    string
		q       win.Process
		cmdline []string
		ours    func(win.Process) bool
		want    bool
	}{
		{"another root's mas_3", qemu, args, notOurs, true},
		{"our own mas_3", qemu, args, ours, false},
		{"windowed qemu", win.Process{PID: 1, Name: "qemu-system-x86_64.exe"}, args, notOurs, true},
		{"not a qemu", win.Process{PID: 1, Name: "adb.exe"}, args, notOurs, false},
		{"another name", qemu, []string{exe, "-avd", "mas_4", "-ports", ports}, notOurs, false},
		{"other ports", qemu, []string{exe, "-avd", "mas_3", "-ports", "5554,5555"}, notOurs, false},
		{"no arguments", qemu, nil, notOurs, false},
	}
	for _, k := range cases {
		if got := otherRootQemu(k.q, k.cmdline, 3, k.ours); got != k.want {
			t.Errorf("%s: otherRootQemu = %v, want %v", k.name, got, k.want)
		}
	}
}

func TestOtherRootBusy(t *testing.T) {
	err := otherRootBusy(3, win.Process{PID: 4242}, `D:\ModAVD\avd\mas_3.avd`)
	if err.Code != errs.Busy || err.Message != `魔改 AVD 实例 3 的端口被另一个根目录的同编号实例占用（pid 4242，D:\ModAVD\avd\mas_3.avd）` ||
		err.Advice == "" {
		t.Errorf("otherRootBusy = %+v", err)
	}
	exe := `D:\ModAVD\sdk\emulator\qemu\windows-x86_64\qemu-system-x86_64-headless.exe`
	if got := qemuWhere(`D:\ModAVD\avd\mas_3.avd`, []string{exe}); got != `D:\ModAVD\avd\mas_3.avd` {
		t.Errorf("qemuWhere with avd.dir = %q", got)
	}
	if got := qemuWhere("", []string{exe, "-avd", "mas_3"}); got != exe {
		t.Errorf("qemuWhere with argv[0] = %q", got)
	}
	if got := qemuWhere("", nil); got != "路径读不到" {
		t.Errorf("qemuWhere with nothing = %q", got)
	}
}

// TestRuntimeAvdDir: the avd.dir of the qemu's own runtime file; a file older than the process (a leftover of an
// earlier process with that PID) does not count.
func TestRuntimeAvdDir(t *testing.T) {
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok || self.Created == 0 {
		t.Fatal("cannot read our own process")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pid_"+strconv.Itoa(os.Getpid())+".ini")
	if got := runtimeAvdDir(dir, self); got != "" {
		t.Errorf("no file: %q", got)
	}
	write := func(text string) {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("avd.name=mas_3\r\n avd.dir = D:\\ModAVD\\avd\\mas_3.avd \r\nport.serial=20030\r\n")
	if got := runtimeAvdDir(dir, self); got != `D:\ModAVD\avd\mas_3.avd` {
		t.Errorf("current file: %q", got)
	}
	old := self.StartedAt().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if got := runtimeAvdDir(dir, self); got != "" {
		t.Errorf("file older than the process: %q", got)
	}
	write("avd.name=mas_3\nport.serial=20030\n")
	if got := runtimeAvdDir(dir, self); got != "" {
		t.Errorf("file without avd.dir: %q", got)
	}
	// another process's file in the same directory is not ours
	other := win.Process{PID: self.PID + 4, Created: self.Created}
	write("avd.dir=D:\\ModAVD\\avd\\mas_3.avd\n")
	if got := runtimeAvdDir(dir, other); got != "" {
		t.Errorf("another PID: %q", got)
	}
}

// TestRuntimeAvdDirDiscoveryDir: otherRootInstance reads the directory adb.QemuBelongsTo reads
// (adb.EmulatorDiscoveryDir: under LOCALAPPDATA, TEMP never matters), so both see the same runtime file.
func TestRuntimeAvdDirDiscoveryDir(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("TEMP", t.TempDir())
	t.Setenv("TMP", t.TempDir())
	want := filepath.Join(local, "Temp", "avd", "running")
	if got := adb.EmulatorDiscoveryDir(); got != want {
		t.Fatalf("EmulatorDiscoveryDir = %q, want %q", got, want)
	}
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("cannot read our own process")
	}
	if err := os.MkdirAll(want, 0o755); err != nil {
		t.Fatal(err)
	}
	ini := filepath.Join(want, "pid_"+strconv.Itoa(os.Getpid())+".ini")
	if err := os.WriteFile(ini, []byte("avd.dir=D:\\ModAVD\\avd\\mas_3.avd\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runtimeAvdDir(adb.EmulatorDiscoveryDir(), self); got != `D:\ModAVD\avd\mas_3.avd` {
		t.Errorf("runtimeAvdDir in the discovery dir = %q", got)
	}
}

// TestOtherRootInstanceFreshRoot runs the real listener / process scan for a root nothing on this machine belongs to:
// whatever it reports has to be a qemu on that instance's ports by name and ports (production instances, if any, are
// exactly that), and it never fails.
func TestOtherRootInstanceFreshRoot(t *testing.T) {
	// Looks at the emulators running on this machine (their lock / runtime files, a real adb server): opt in.
	if os.Getenv("MODAVD_TEST_LIVE") == "" {
		t.Skip("set MODAVD_TEST_LIVE=1 to run tests that look at live emulators or start a real adb server")
	}
	m := markerTestManager(t)
	for _, i := range []int{0, 1, 2, 3, 4, 6, 7, 8, 9} {
		p, where, found := m.otherRootInstance(i)
		if !found {
			continue
		}
		cmdline, err := p.CommandLine()
		if !isQemuName(p.Name) || where == "" || (err == nil && !cmdlineMatches(cmdline, root.AvdName(i), root.ConsolePort(i))) {
			t.Errorf("instance %d: reported %+v (%s)", i, p, where)
		}
		t.Logf("instance %d: another root's qemu pid %d at %s", i, p.PID, where)
	}
}

func TestJoinInts(t *testing.T) {
	if got := joinInts([]int{20030, 20032}, ", "); got != "20030, 20032" {
		t.Errorf("joinInts = %q", got)
	}
}
