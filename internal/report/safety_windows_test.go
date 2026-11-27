package report

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/root"
)

// The rules of the scripts' headers: read-only, no adb server of our own, never a disk image or AUTO-MAS's
// config / data, a failing section does not stop the rest, nothing personal in report-config.

func summaryOf(t *testing.T, files map[string][]byte) string {
	t.Helper()
	s, ok := files["summary.txt"]
	if !ok {
		t.Fatal("no summary.txt in the zip")
	}
	return strings.TrimPrefix(string(s), "\ufeff")
}

func hasEntryPrefix(names []string, prefix string) bool {
	for _, n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func TestCollectNeverTakesDiskImagesOrMasData(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	guardDials(t, f.adbPort)
	// the fixture has them, next to what is collected
	for _, p := range []string{
		filepath.Join(f.root, "avd", "mas_1.avd", "userdata-qemu.img"), filepath.Join(f.root, "avd", "mas_1.avd", "userdata-qemu.img.qcow2"),
		filepath.Join(f.root, "avd", "mas_1.avd", "snapshots", "default_boot", "ram.bin"), filepath.Join(f.root, "avd", "mas_4.avd", "cache.img"),
		filepath.Join(f.root, "sdk", "system-images", "android-34", "google_apis", "x86_64", "system.img"),
		filepath.Join(f.base, "mas", "config", "accounts.json"), filepath.Join(f.base, "mas", "data", "accounts.db"),
	} {
		if !isFile(p) {
			t.Fatalf("fixture lacks %s", p)
		}
	}
	res, err := collect(context.Background(), f.root, f.host, CollectOptions{
		OutDir: filepath.Join(f.base, "out"), StageDir: filepath.Join(f.base, "stage"), NoExplorer: true, KeepStage: true,
		now: func() time.Time { return f.now }, getenv: f.getenv, pid: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	names, files := readZip(t, res.ZipPath)
	if !hasEntryPrefix(names, "mas/") || !hasEntryPrefix(names, "avd/mas_1.avd/") {
		t.Fatalf("nothing collected: %q", names)
	}
	// not even copied into the stage directory (the zip leaves them out once more)
	if !res.StageKept {
		t.Fatal("stage not kept")
	}
	var staged []string
	err = filepath.WalkDir(res.Stage, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(res.Stage, p)
			staged = append(staged, filepath.ToSlash(rel))
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != len(names) {
		t.Errorf("staged %q, zipped %q", staged, names)
	}
	for _, n := range append(names, staged...) {
		l := strings.ToLower(n)
		if strings.HasSuffix(l, ".img") || strings.HasSuffix(l, ".qcow2") || strings.Contains(l, "snapshots") ||
			strings.Contains(l, "userdata") || strings.Contains(l, "ram.bin") || strings.Contains(l, "accounts") ||
			strings.HasPrefix(l, "mas/config") || strings.HasPrefix(l, "mas/data") || strings.HasPrefix(l, "sdk/") {
			t.Errorf("zip has %s", n)
		}
	}
	for n, b := range files {
		if strings.Contains(string(b), `{"secret": true}`) {
			t.Errorf("%s holds AUTO-MAS's config", n)
		}
	}
	// the names and sizes of the instance directory are listed, its images are not copied
	if l := string(files["avd/mas_1.avd/_files.txt"]); !strings.Contains(l, "userdata-qemu.img ") {
		t.Errorf("_files.txt: %q", l)
	}
}

// panicHost panics in one query, like a bug in a section.
type panicHost struct{ *fakeHost }

func (panicHost) AppEvents(context.Context) ([]AppEvent, error) { panic("boom") }

// report-config reads the emulator of mas-avd.json's sdkRoot when it is set, as the backend runs it.
func TestReportConfigUsesSdkRoot(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	guardDials(t, f.adbPort)
	alt := filepath.Join(f.base, "dev-sdk")
	for name, text := range map[string]string{
		filepath.Join(alt, "emulator", "source.properties"):  "Pkg.BuildId=mas-77\n",
		filepath.Join(alt, "emulator", "emulator-check.exe"): "MZ",
		filepath.Join(f.root, root.MetadataFile):             `{"sdkRoot": " ` + jsonEscape(alt) + ` "}`,
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res, err := BuildConfig(context.Background(), &root.Root{Dir: f.root}, ConfigOptions{Host: f.host, NoClipboard: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.EmulatorBuild != "mas-77" || res.Acceleration != "WHPX(10.0.26200) 可用" {
		t.Errorf("emulator %q, acceleration %q", res.EmulatorBuild, res.Acceleration)
	}
	if ps := f.host.programs(); len(ps) != 1 || !strings.EqualFold(ps[0], filepath.Join(alt, "emulator", "emulator-check.exe")) {
		t.Errorf("ran %q", ps)
	}
}

func TestCollectSectionFailureContinues(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	setLimits(t, f.sc)
	guardDials(t, f.adbPort)
	f.sc.OSErr = "WMI 模拟的错误"
	f.sc.ProcessesErr = "进程表模拟的错误"
	res, err := collect(context.Background(), f.root, panicHost{f.host}, CollectOptions{
		OutDir: filepath.Join(f.base, "out"), StageDir: filepath.Join(f.base, "stage"), NoExplorer: true,
		now: func() time.Time { return f.now }, getenv: f.getenv, pid: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	names, files := readZip(t, res.ZipPath)
	s := summaryOf(t, files)
	for _, want := range []string{
		"== 本机 ==\r\n[本节出错] Windows / CPU / 内存 / 显卡：WMI 模拟的错误\r\n",
		"== 虚拟化相关 Windows 功能", "VirtualMachinePlatform：InstallState=1（已开）",
		"== 正在运行的 qemu-system 进程 ==\r\n[本节出错] 正在运行的模拟器与 adb：进程表模拟的错误\r\n",
		"== 模拟器日志（logs\\） ==", "共复制 11 份", "== 模拟器崩溃转储 ==", "== AUTO-MAS ==\r\n安装目录：",
		"[本节出错] 系统事件日志：内部错误：boom\r\n",
		"== 现场 ==\r\n没有实例在运行，跳过\r\n",
		"出错的节：\r\n  Windows / CPU / 内存 / 显卡：WMI 模拟的错误\r\n  正在运行的模拟器与 adb：进程表模拟的错误\r\n  系统事件日志：内部错误：boom\r\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("summary.txt lacks %q", want)
		}
	}
	if len(res.Problems) != 3 {
		t.Errorf("problems %q", res.Problems)
	}
	for _, p := range []string{"logs/", "crash/", "mas/", "avd/"} {
		if !hasEntryPrefix(names, p) {
			t.Errorf("no %s entries after the failures", p)
		}
	}
}

func TestCollectDetectsRunningInstance(t *testing.T) {
	useGoldenZone(t, "full")
	t.Run("command line", func(t *testing.T) {
		f := newFixture(t, "full", testTempDir(t), "")
		guardDials(t, f.adbPort)
		// only the qemu whose command line is readable: no device-list fallback
		ps := f.sc.Processes[:0]
		for _, p := range f.sc.Processes {
			if p.ProcessId != 4300 {
				ps = append(ps, p)
			}
		}
		f.sc.Processes = ps
		_, names, files := runCollect(t, f)
		s := summaryOf(t, files)
		if strings.Contains(s, "没有实例在运行") || !strings.Contains(s, "== 现场 ==\r\nadb：按 adb 主机协议直接连 -P "+strconv.Itoa(f.adbPort)) ||
			!strings.Contains(s, "\r\nmas_1（pid 4100，emulator-20010）：\r\n  截图：70 B\r\n") || strings.Contains(s, "推算") {
			t.Errorf("live section:\n%s", s[strings.Index(s, "== 现场 =="):])
		}
		for _, e := range []string{"live/screen-mas_1.png", "live/mas_1-getprop.txt", "live/mas_1-window-focus.txt", "live/mas_1-logcat-crash.txt"} {
			if !containsLine(names, e) {
				t.Errorf("no %s", e)
			}
		}
		_, reqs := f.adb.seen()
		if !containsLine(reqs, "emulator-20010 exec:screencap -p") || !containsLine(reqs, "emulator-20010 shell,v2,raw:getprop") {
			t.Errorf("adb requests %q", reqs)
		}
	})
	t.Run("single -port", func(t *testing.T) {
		f := newFixture(t, "full", testTempDir(t), "")
		guardDials(t, f.adbPort)
		ps := f.sc.Processes[:0]
		for _, p := range f.sc.Processes {
			if p.ProcessId != 4300 && p.ProcessId != 4400 {
				ps = append(ps, p)
			}
		}
		f.sc.Processes = ps
		f.sc.Processes[0].CommandLine = `qemu-system-x86_64.exe -avd "MAS_3" -port 20030 -no-window`
		_, _, files := runCollect(t, f)
		if s := summaryOf(t, files); !strings.Contains(s, "\r\nMAS_3（pid 4100，emulator-20030）：\r\n  截图：失败，退出码 0 \r\n") {
			t.Errorf("live section:\n%s", s[strings.Index(s, "== 现场 =="):])
		}
	})
	t.Run("not ours", func(t *testing.T) {
		f := newFixture(t, "full", testTempDir(t), "")
		guardDials(t, f.adbPort)
		f.sc.Processes = f.sc.Processes[2:3] // -avd other_avd
		_, names, files := runCollect(t, f)
		if s := summaryOf(t, files); !strings.Contains(s, "== 现场 ==\r\n没有实例在运行，跳过\r\n") || hasEntryPrefix(names, "live/") {
			t.Errorf("live section:\n%s", s[strings.Index(s, "== 现场 =="):])
		}
		if c, _ := f.adb.seen(); c != 0 {
			t.Errorf("%d connections to the adb server", c)
		}
	})
}

func TestNoAdbServerWhenNoneListens(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	f.sc.Listeners = nil
	dialed := guardDials(t, f.adbPort)

	res, err := BuildConfig(context.Background(), &root.Root{Dir: f.root}, ConfigOptions{Host: f.host, NoClipboard: true})
	if err != nil {
		t.Fatal(err)
	}
	if !containsLine(res.Lines, "实例：3 个（实例 1，6 GB；实例 10，3 GB；实例 3，4 GB）") || !containsLine(res.Lines, configNotOpen) {
		t.Errorf("report-config:\n%s", res.Text)
	}

	_, names, files := runCollect(t, f)
	s := summaryOf(t, files)
	if !strings.Contains(s, fmt.Sprintf("== 现场 ==\r\n端口 %d 上没有 adb server 在监听，跳过（不自己起 server）\r\n", f.adbPort)) ||
		hasEntryPrefix(names, "live/") {
		t.Errorf("live section:\n%s", s[strings.Index(s, "== 现场 =="):])
	}
	if len(*dialed) != 0 {
		t.Errorf("dialed %q", *dialed)
	}
	if c, _ := f.adb.seen(); c != 0 {
		t.Errorf("%d connections to the adb server", c)
	}
	for _, p := range f.host.programs() {
		if strings.EqualFold(filepath.Base(p), "adb.exe") {
			t.Errorf("started %s", p)
		}
	}
}

// privacyEnv sets USERNAME, COMPUTERNAME and USERPROFILE to known values and returns them with the real ones.
func privacyEnv(t *testing.T, base string) (fake, real []string) {
	t.Helper()
	for _, k := range []string{"USERNAME", "COMPUTERNAME", "USERPROFILE"} {
		if v := os.Getenv(k); len(v) >= 4 {
			real = append(real, v)
		}
	}
	user, pc := "zzuser7731", "ZZPC-7731"
	profile := filepath.Join(base, "Users", user)
	t.Setenv("USERNAME", user)
	t.Setenv("COMPUTERNAME", pc)
	t.Setenv("USERPROFILE", profile)
	return []string{user, pc, profile}, real
}

func assertPrivate(t *testing.T, res *ConfigResult, secrets ...string) {
	t.Helper()
	j, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{res.Text, strings.Join(res.Lines, "\n"), string(j)} {
		for _, s := range secrets {
			if strings.Contains(strings.ToLower(out), strings.ToLower(s)) {
				t.Errorf("report-config shows %q:\n%s", s, out)
			}
		}
	}
}

// failRunHost fails to start any program, with the path in the error as exec does.
type failRunHost struct{ *fakeHost }

func (failRunHost) Run(_ context.Context, exe string, _ []string, _ time.Duration) (RunResult, error) {
	return RunResult{}, &exec.Error{Name: exe, Err: errors.New("模拟的启动失败")}
}

func TestReportConfigPrivacy(t *testing.T) {
	useGoldenZone(t, "full")
	base := testTempDir(t)
	fake, _ := privacyEnv(t, base)
	// the root under the user's profile, in a folder named after the computer
	f := newFixture(t, "full", base, filepath.Join("Users", fake[0], fake[1], "ModAVD"))
	guardDials(t, f.adbPort)
	for _, h := range []Host{f.host, failRunHost{f.host}} {
		res, err := BuildConfig(context.Background(), &root.Root{Dir: f.root}, ConfigOptions{Host: h, NoClipboard: true})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(res.Emulator, "mas-36，放在 ") || len(res.Instances) != 4 {
			t.Errorf("unexpected report:\n%s", res.Text)
		}
		if _, ok := h.(failRunHost); ok && res.Acceleration != "检查失败（模拟的启动失败）" {
			t.Errorf("acceleration %q", res.Acceleration)
		}
		assertPrivate(t, res, append(fake, f.root, f.base)...)
	}
}

// realHostNoAdb is this computer, but with no TCP listeners: the test never asks a real adb server.
type realHostNoAdb struct{ Host }

func (realHostNoAdb) Listeners() ([]Listener, error) { return nil, nil }

// The real Windows queries (WMI through Windows PowerShell, the registry, the drive): the lines are there and hold
// nothing personal, neither the values set here nor this computer's own.
func TestReportConfigRealHost(t *testing.T) {
	if testing.Short() {
		t.Skip("asks Windows PowerShell for WMI")
	}
	base := testTempDir(t)
	fake, real := privacyEnv(t, base)
	dir := filepath.Join(base, "Users", fake[0], fake[1], "ModAVD")
	if err := os.MkdirAll(filepath.Join(dir, "sdk", "emulator"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sdk", "emulator", "source.properties"), []byte("Pkg.Revision=37.2.10\r\nPkg.BuildId=mas-99\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dialed := guardDials(t)
	h, err := newHost()
	if err != nil {
		t.Fatal(err)
	}
	res, err := BuildConfig(context.Background(), &root.Root{Dir: dir}, ConfigOptions{Host: realHostNoAdb{h}, NoClipboard: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("\n%s", res.Text)
	for _, p := range []string{"系统：", "CPU：", "内存：", "显卡：", "模拟器：mas-99，放在 "} {
		found := false
		for _, l := range res.Lines {
			if strings.HasPrefix(l, p) && !strings.Contains(l, "读不到（") {
				found = true
			}
		}
		if !found {
			t.Errorf("no %q line", p)
		}
	}
	for _, l := range []string{configTitle, "硬件加速：读不到", configNotOpen} {
		if !containsLine(res.Lines, l) {
			t.Errorf("no %q line", l)
		}
	}
	if len(*dialed) != 0 {
		t.Errorf("dialed %q", *dialed)
	}
	assertPrivate(t, res, append(append(fake, real...), dir)...)
}
