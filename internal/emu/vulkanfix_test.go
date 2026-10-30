//go:build windows

package emu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/logx"
	"modavd/internal/vkfix"
	"modavd/internal/win"
)

func TestVulkanFixStatus(t *testing.T) {
	fix := vkfix.FixSHA256
	cases := []struct {
		out        string
		kind, rest string
	}{
		{"MOUNTED " + fix, "MOUNTED", fix},
		{"ALREADY " + fix, "ALREADY", fix},
		{"ALREADY 1234abcd", "ALREADY", "1234abcd"},
		{"VENDOR_PATCHED", "VENDOR_PATCHED", ""},
		{"NEED_UPLOAD", "NEED_UPLOAD", ""},
		{"SKIPPED image 0000", "SKIPPED", "image 0000"},
		{"SKIPPED patched 1111", "SKIPPED", "patched 1111"},
		{"FAILED chcon u:object_r:vendor_file:s0 -> u:object_r:shell_data_file:s0", "FAILED",
			"chcon u:object_r:vendor_file:s0 -> u:object_r:shell_data_file:s0"},
		{"", "", ""},
		{"/system/bin/sh: vulkanfix.sh: not found", "", ""},
	}
	for _, c := range cases {
		// the script prints one status line last; adb output may carry other lines before it
		status := lastNonEmptyLine("some noise\r\n" + c.out + "\r\n")
		if c.out == "" {
			status = lastNonEmptyLine("")
		}
		kind, rest := vulkanFixStatus(status)
		if kind != c.kind || rest != c.rest {
			t.Errorf("vulkanFixStatus(%q) = %q %q; want %q %q", status, kind, rest, c.kind, c.rest)
		}
	}
}

// TestVulkanFixCheckReport: an adopted instance warns only when the fix applies to its image and is not mounted.
func TestVulkanFixCheckReport(t *testing.T) {
	cases := []struct {
		out  string
		warn bool
		has  string
	}{
		{"mounted\r\n", false, "本次开机已挂上"},
		{"image " + vkfix.FixSHA256 + "\r\n", false, "已是补丁版"},
		{"image " + vkfix.OrigSHA256 + "\r\n", true, "要挂就先关掉这个实例再启动"},
		{"image " + strings.Repeat("ab", 32) + "\n", false, "不是预期的镜像版本"},
		{"image \n", true, "读不出"},                           // sha256sum failed
		{"error: device offline\n", true, "device offline"}, // adb failed
		{"", true, "无输出"},
	}
	for _, c := range cases {
		warn, msg := vulkanFixCheckReport(4, c.out)
		if warn != c.warn || !strings.HasPrefix(msg, "实例 4 ") || !strings.Contains(msg, c.has) {
			t.Errorf("vulkanFixCheckReport(%q) = %v %q", c.out, warn, msg)
		}
	}
	// the check reads the same mount test as vulkanfix.sh and never needs root
	if !strings.Contains(vulkanFixCheckScript, "grep -q ' "+vkfix.GuestPath+" ' /proc/self/mountinfo") ||
		strings.Contains(vulkanFixCheckScript, "su ") {
		t.Errorf("check script = %s", vulkanFixCheckScript)
	}
}

// TestVulkanFixMountAllowed: an owed fix is mounted only when nothing anybody uses is in front: the default home app,
// SystemUI, or no resumed activity at all. Everything else, unreadable output included, defers it.
func TestVulkanFixMountAllowed(t *testing.T) {
	const (
		homeLight = "home: de.jrpie.android.launcher/de.jrpie.android.launcher.ui.HomeActivity\r\n"
		homePixel = "home: com.google.android.apps.nexuslauncher/com.android.launcher3.uioverrides.QuickstepLauncher\r\n"
	)
	cases := []struct {
		name    string
		out     string
		allowed bool
		front   string
	}{
		{"light launcher in front", "top:   topResumedActivity=ActivityRecord{1d5b2a5 u0 de.jrpie.android.launcher/.ui.HomeActivity t5}\r\n" +
			homeLight, true, "de.jrpie.android.launcher"},
		{"pixel launcher (recents) in front", "top: mResumedActivity: ActivityRecord{77 u0 com.google.android.apps.nexuslauncher/" +
			"com.android.launcher3.uioverrides.QuickstepLauncher t2}\n" + homePixel, true, "com.google.android.apps.nexuslauncher"},
		{"game in front", "top:     mResumedActivity: ActivityRecord{9f1 u0 com.miHoYo.hkrpg/com.mihoyo.combosdk.ComboSDKActivity t12}\r\n" +
			homeLight, false, "com.miHoYo.hkrpg"},
		{"settings in front", "top: topResumedActivity=ActivityRecord{3 u0 com.android.settings/.Settings t9}\n" + homeLight,
			false, "com.android.settings"},
		{"systemui in front", "top: topResumedActivity=ActivityRecord{4 u0 com.android.systemui/.SomeActivity t3}\n" + homeLight,
			true, "com.android.systemui"},
		{"nothing resumed", "top:   topResumedActivity=null\nhome: \n", true, "无"},
		// grep -m1 ends dumpsys early: its "Broken pipe" comes on a line of its own
		{"broken pipe noise", "Broken pipe\r\ntop:   topResumedActivity=ActivityRecord{1 u0 de.jrpie.android.launcher/.ui.HomeActivity t5}\r\n" +
			"Broken pipe\r\n" + homeLight, true, "de.jrpie.android.launcher"},
		// the home app in front but no default home known: cannot tell it is home
		{"home unresolved", "top: topResumedActivity=ActivityRecord{1 u0 com.android.launcher3/.Launcher t1}\nhome: No activity found\n",
			false, "com.android.launcher3"},
		{"home tag missing", "top: topResumedActivity=ActivityRecord{1 u0 de.jrpie.android.launcher/.ui.HomeActivity t5}\n",
			false, "de.jrpie.android.launcher"},
		// no default home: the resolver; android's chooser activities can sit over a game, so it is no home screen
		{"resolver as home", "top: topResumedActivity=ActivityRecord{2 u0 android/com.android.internal.app.ChooserActivity t8}\n" +
			"home: android/com.android.internal.app.ResolverActivity\n", false, "android"},
		{"dumpsys gave nothing", "top: \r\n" + homeLight, false, ""},
		{"no component in the record", "top: topResumedActivity=ActivityRecord{1 u0 t1}\n" + homeLight, false, ""},
		{"not a package name", "top: topResumedActivity=ActivityRecord{1 u0 9bad/.A t1}\n" + homeLight, false, ""},
		{"adb failed", "error: device 'emulator-20030' not found\r\n", false, ""},
		{"no output", "", false, ""},
	}
	for _, c := range cases {
		allowed, front := vulkanFixMountAllowed(c.out)
		if allowed != c.allowed || front != c.front {
			t.Errorf("%s: vulkanFixMountAllowed = %v %q, want %v %q", c.name, allowed, front, c.allowed, c.front)
		}
	}
}

func TestComponentPackage(t *testing.T) {
	cases := []struct {
		in  string
		pkg string
		ok  bool
	}{
		{"com.android.launcher3/.Launcher", "com.android.launcher3", true},
		{"ActivityRecord{c5a u0 com.x_y.Z9/.A t9}", "com.x_y.Z9", true},
		{"{com.a/.B}", "com.a", true},
		{"android/com.android.internal.app.ResolverActivity", "android", true},
		{"No activity found", "", false},
		{"9bad/.A", "9bad", false},
		{"/.A", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		if pkg, ok := componentPackage(c.in); pkg != c.pkg || ok != c.ok {
			t.Errorf("componentPackage(%q) = %q %v, want %q %v", c.in, pkg, ok, c.pkg, c.ok)
		}
	}
}

func TestForegroundProbeScript(t *testing.T) {
	for _, s := range []string{
		`echo "top: $(dumpsys activity activities | grep -m1 -E 'topResumedActivity|mResumedActivity')"`,
		`echo "home: $(cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.HOME | tail -n1)"`,
	} {
		if !strings.Contains(foregroundProbeScript, s) {
			t.Errorf("probe lacks %s: %s", s, foregroundProbeScript)
		}
	}
	// read-only, no root
	if strings.Contains(foregroundProbeScript, "su ") {
		t.Errorf("probe uses su: %s", foregroundProbeScript)
	}
}

// TestVulkanFixDeferredReport: an owed fix left out because the guest is in use stays owed only while the image needs
// it and it is not mounted (or that cannot be read); the warning names what is in the way and the actual deadline.
func TestVulkanFixDeferredReport(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.Local)
	until := now.Add(25*time.Minute + 30*time.Second)
	const (
		front   = "实例 3 的客体 Vulkan 驱动补丁还没挂：现在有应用在前台，挂上要重启 zygote；关掉应用后在 18:25 前再 start 一次，过了这个时间就等下次开机自动挂"
		install = "实例 3 的客体 Vulkan 驱动补丁还没挂：现在有安装正在进行，挂上要重启 zygote；等安装完在 18:25 前再 start 一次，过了这个时间就等下次开机自动挂"
	)
	game := foregroundBlocker("com.miHoYo.hkrpg")
	cases := []struct {
		name    string
		out     string
		b       mountBlocker
		until   time.Time
		pending bool
		msg     string // exact when it starts with 实例, else a part of it
	}{
		{"missing, game in front", "image " + vkfix.OrigSHA256 + "\r\n", game, until, true, front + "（前台: com.miHoYo.hkrpg）"},
		{"missing, front unreadable", "image " + vkfix.OrigSHA256 + "\n", foregroundBlocker(""), until, true,
			front + "（读不出前台是什么，按有应用在用处理）"},
		{"check unreadable", "error: device offline\n", game, until, true,
			front + "（前台: com.miHoYo.hkrpg；这次开机挂没挂也读不出: error: device offline）"},
		{"check gave nothing", "", game, until, true, front + "（前台: com.miHoYo.hkrpg；这次开机挂没挂也读不出: 无输出）"},
		{"missing, install running", "image " + vkfix.OrigSHA256 + "\n", installBlocker(nil), until, true, install},
		{"install lock unreadable", "image " + vkfix.OrigSHA256 + "\n", installBlocker(errors.New("拒绝访问。")), until, true,
			install + "（安装锁读不了（拒绝访问。），按有安装正在进行处理）"},
		{"install running, check unreadable", "", installBlocker(nil), until, true, install + "（这次开机挂没挂也读不出: 无输出）"},
		{"deadline passed", "image " + vkfix.OrigSHA256 + "\n", game, now.Add(-5 * time.Minute), true,
			"实例 3 的客体 Vulkan 驱动补丁还没挂：现在有应用在前台，挂上要重启 zygote；这次开机接着做设置的期限（17:55）已过，" +
				"等下次开机自动挂（前台: com.miHoYo.hkrpg）"},
		{"deadline unknown", "image " + vkfix.OrigSHA256 + "\n", game, time.Time{}, true,
			"实例 3 的客体 Vulkan 驱动补丁还没挂：现在有应用在前台，挂上要重启 zygote；关掉应用后再 start 一次，或下次开机自动挂（前台: com.miHoYo.hkrpg）"},
		{"already mounted", "mounted\r\n", game, until, false, "本次开机已挂上"},
		{"patched image", "image " + vkfix.FixSHA256 + "\n", installBlocker(nil), until, false, "已是补丁版"},
		{"unexpected image", "image " + strings.Repeat("cd", 32) + "\n", foregroundBlocker(""), until, false, "不是预期的镜像版本"},
	}
	for _, c := range cases {
		pending, msg := vulkanFixDeferredReport(3, c.out, c.b, c.until, now)
		okMsg := msg == c.msg
		if !strings.HasPrefix(c.msg, "实例") {
			okMsg = strings.HasPrefix(msg, "实例 3 ") && strings.Contains(msg, c.msg)
		}
		if pending != c.pending || !okMsg {
			t.Errorf("%s: vulkanFixDeferredReport = %v %q", c.name, pending, msg)
		}
	}
}

// fakeVulkanGuest is a guest for mountVulkanFix: canned answers per command, every command recorded by a short name,
// and for each vulkanfix.sh run whether the guest lock was held meanwhile.
type fakeVulkanGuest struct {
	t        *testing.T
	lock     string   // guestLockPath of the instance
	probes   []string // successive foregroundProbeScript outputs
	fixes    []string // successive vulkanfix.sh status lines
	check    string   // vulkanFixCheckScript output
	onPush   func()   // runs during the push of the patched library
	commands []string
	locked   []bool
}

const fakeVulkanRemote = "/data/local/tmp/vulkanfix.sh"

func (f *fakeVulkanGuest) io() vulkanFixIO {
	return vulkanFixIO{
		guestIO: guestIO{shell: f.shell, adb: f.adb},
		remote:  fakeVulkanRemote,
		library: func() string { return `D:\r\guest-patches\` + vkfix.CacheName },
		waitBack: func(string) bool {
			f.commands = append(f.commands, "wait")
			return true
		},
	}
}

func (f *fakeVulkanGuest) shell(command string, _ time.Duration) adb.Result {
	switch command {
	case "pidof com.android.systemui":
		f.commands = append(f.commands, "pidof")
		return adb.Result{Out: "1234\r\n"}
	case foregroundProbeScript:
		f.commands = append(f.commands, "probe")
		if len(f.probes) == 0 {
			f.t.Error("probe asked for more often than expected")
			return adb.Result{Out: ""}
		}
		out := f.probes[0]
		f.probes = f.probes[1:]
		return adb.Result{Out: out}
	case vulkanFixCheckScript:
		f.commands = append(f.commands, "check")
		return adb.Result{Out: f.check + "\r\n"}
	case "su 0 sh " + fakeVulkanRemote + " " + vkfix.OrigSHA256 + " " + vkfix.FixSHA256:
		f.commands = append(f.commands, "fix")
		// a second handle conflicts like another process does (win.TryLock): busy = somebody holds it
		l, free, err := win.TryLock(f.lock)
		if err != nil {
			f.t.Fatal(err)
		}
		l.Unlock()
		f.locked = append(f.locked, !free)
		if len(f.fixes) == 0 {
			f.t.Error("vulkanfix.sh ran more often than expected")
			return adb.Result{Out: "FAILED test"}
		}
		out := f.fixes[0]
		f.fixes = f.fixes[1:]
		return adb.Result{Out: out + "\r\n"}
	}
	f.t.Errorf("unexpected command %q", command)
	return adb.Result{Code: 1, Out: "unexpected command"}
}

func (f *fakeVulkanGuest) adb(_ time.Duration, args ...string) adb.Result {
	if len(args) == 3 && args[0] == "push" && args[2] == vkfix.UploadPath {
		f.commands = append(f.commands, "push")
		if f.onPush != nil {
			f.onPush()
		}
		return adb.Result{Out: "1 file pushed"}
	}
	f.t.Errorf("unexpected adb %q", args)
	return adb.Result{Code: 1, Out: "unexpected adb"}
}

// TestMountVulkanFix walks the mount against a fake guest. A launched boot mounts without a foreground question but
// still under the guest lock (an install of another process defers it); an owed one passes owedMountGate before every
// vulkanfix.sh run that may mount (the guest lock, held over that run and freed right after it, then the foreground
// probe), so a game launched or an install started while the patched library was being uploaded still defers the
// zygote restart.
func TestMountVulkanFix(t *testing.T) {
	const (
		gameFront = "top:     mResumedActivity: ActivityRecord{9f1 u0 com.miHoYo.hkrpg/.Game t12}\r\n" +
			"home: de.jrpie.android.launcher/de.jrpie.android.launcher.ui.HomeActivity\r\n"
		homeFront = "top:   topResumedActivity=ActivityRecord{1 u0 de.jrpie.android.launcher/.ui.HomeActivity t5}\r\n" +
			"home: de.jrpie.android.launcher/de.jrpie.android.launcher.ui.HomeActivity\r\n"
		mounted = "MOUNTED " + vkfix.FixSHA256
	)
	until := time.Now().Add(20 * time.Minute)
	retry := "在 " + clockText(until, time.Now()) + " 前再 start 一次，过了这个时间就等下次开机自动挂"
	cases := []struct {
		name               string
		owed               bool
		installing         bool // an install holds the guest lock from the start
		installDuringPush  bool // an install takes it while the library is uploaded
		probes, fixes      []string
		check              string
		commands           []string
		locked             []bool
		restarted, pending bool
		level              logx.Level
		has                []string
	}{
		{name: "launched, cached in the guest", fixes: []string{mounted},
			commands: []string{"pidof", "fix", "wait"}, locked: []bool{true}, restarted: true,
			level: logx.Info, has: []string{"客体 Vulkan 驱动补丁已挂上"}},
		{name: "launched, upload", fixes: []string{"NEED_UPLOAD", mounted},
			commands: []string{"pidof", "fix", "push", "fix", "wait"}, locked: []bool{true, true}, restarted: true,
			level: logx.Info, has: []string{"第 3 步（挂载）"}},
		// a launched boot asks no foreground question, but an install of another process defers the mount the same way
		{name: "launched, install running", installing: true, check: "image " + vkfix.OrigSHA256,
			commands: []string{"pidof", "check"}, pending: true,
			level: logx.Warn, has: []string{"现在有安装正在进行，挂上要重启 zygote；等安装完" + retry}},
		{name: "owed, home in front, cached", owed: true, probes: []string{homeFront}, fixes: []string{mounted},
			commands: []string{"pidof", "probe", "fix", "wait"}, locked: []bool{true}, restarted: true,
			level: logx.Info, has: []string{"没有应用在用，也没有安装在进行，接着挂"}},
		{name: "owed, game in front", owed: true, probes: []string{gameFront}, check: "image " + vkfix.OrigSHA256,
			commands: []string{"pidof", "probe", "check"}, pending: true,
			level: logx.Warn, has: []string{"现在有应用在前台，挂上要重启 zygote；关掉应用后" + retry, "（前台: com.miHoYo.hkrpg）"}},
		{name: "owed, game in front, fix mounted already", owed: true, probes: []string{gameFront}, check: "mounted",
			commands: []string{"pidof", "probe", "check"}, level: logx.Info, has: []string{"本次开机已挂上"}},
		{name: "owed, install running", owed: true, installing: true, check: "image " + vkfix.OrigSHA256,
			commands: []string{"pidof", "check"}, pending: true,
			level: logx.Warn, has: []string{"现在有安装正在进行，挂上要重启 zygote；等安装完" + retry}},
		{name: "owed, install running, fix mounted already", owed: true, installing: true, check: "mounted",
			commands: []string{"pidof", "check"}, level: logx.Info, has: []string{"本次开机已挂上"}},
		{name: "owed, upload, game launched meanwhile", owed: true, probes: []string{homeFront, gameFront},
			fixes: []string{"NEED_UPLOAD"}, commands: []string{"pidof", "probe", "fix", "push", "probe"},
			locked: []bool{true}, pending: true,
			level: logx.Warn, has: []string{"现在有应用在前台，挂上要重启 zygote；关掉应用后" + retry, "（前台: com.miHoYo.hkrpg）"}},
		{name: "owed, upload, still home", owed: true, probes: []string{homeFront, homeFront},
			fixes: []string{"NEED_UPLOAD", mounted}, commands: []string{"pidof", "probe", "fix", "push", "probe", "fix", "wait"},
			locked: []bool{true, true}, restarted: true, level: logx.Info, has: []string{"第 3 步（挂载）"}},
		{name: "owed, upload, install started meanwhile", owed: true, installDuringPush: true, probes: []string{homeFront},
			fixes: []string{"NEED_UPLOAD"}, commands: []string{"pidof", "probe", "fix", "push"},
			locked: []bool{true}, pending: true,
			level: logx.Warn, has: []string{"现在有安装正在进行，挂上要重启 zygote；等安装完" + retry}},
		{name: "owed, upload, mount failed", owed: true, probes: []string{homeFront, homeFront},
			fixes: []string{"NEED_UPLOAD", "FAILED mount 0000"}, commands: []string{"pidof", "probe", "fix", "push", "probe", "fix"},
			locked: []bool{true, true}, level: logx.Warn, has: []string{"客体 Vulkan 驱动补丁没挂: FAILED mount 0000"}},
	}
	for _, c := range cases {
		m := markerTestManager(t)
		var lines []logLine
		m.Log = logx.New(func(level logx.Level, msg string) { lines = append(lines, logLine{level, msg}) })
		f := &fakeVulkanGuest{t: t, lock: m.guestLockPath(3), probes: c.probes, fixes: c.fixes, check: c.check}
		var install *win.FileLock
		takeInstall := func() {
			l, ok, err := win.TryLock(m.guestLockPath(3))
			if err != nil || !ok {
				t.Fatalf("%s: install lock: %v %v", c.name, ok, err)
			}
			install = l
		}
		if c.installing {
			takeInstall()
		}
		if c.installDuringPush {
			f.onPush = takeInstall
		}

		restarted, pending := m.mountVulkanFix(context.Background(), 3, f.io(), c.owed, until)
		install.Unlock()

		if restarted != c.restarted || pending != c.pending {
			t.Errorf("%s: restarted %v pending %v", c.name, restarted, pending)
		}
		if !reflect.DeepEqual(f.commands, c.commands) {
			t.Errorf("%s: commands %q, want %q", c.name, f.commands, c.commands)
		}
		if !reflect.DeepEqual(f.locked, c.locked) {
			t.Errorf("%s: guest lock held during the runs %v, want %v", c.name, f.locked, c.locked)
		}
		if len(f.probes) != 0 || len(f.fixes) != 0 {
			t.Errorf("%s: answers left over: probes %d, fixes %d", c.name, len(f.probes), len(f.fixes))
		}
		// whatever happened, the guest lock is free again
		if l, ok, err := win.TryLock(m.guestLockPath(3)); err != nil || !ok {
			t.Errorf("%s: guest lock still held after the mount: %v %v", c.name, ok, err)
		} else {
			l.Unlock()
		}
		var found bool
		for _, l := range lines {
			if l.level == c.level && strings.HasPrefix(l.msg, "实例 3 ") {
				all := true
				for _, s := range c.has {
					all = all && strings.Contains(l.msg, s)
				}
				found = found || all
			}
			if l.level == logx.Warn && c.level != logx.Warn {
				t.Errorf("%s: unexpected warning %q", c.name, l.msg)
			}
		}
		if !found {
			t.Errorf("%s: no %v line with %q in %v", c.name, c.level, c.has, lines)
		}
	}
}

func TestVulkanFixConstants(t *testing.T) {
	// what vulkanfix.sh is called with and what it expects (u= in the script)
	if vkfix.UploadPath != "/data/local/tmp/vulkanfix-upload.so" || vkfix.GuestPath != "/vendor/lib64/libvulkan_enc.so" ||
		vkfix.CacheName != "libvulkan_enc.khrfix-bindlock2.so" {
		t.Error("vkfix constants changed")
	}
}

func TestWriteFileReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, vkfix.CacheName)
	if err := writeFileReplace(path, []byte("first"), "1-0"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileReplace(path, []byte("second"), "2-0"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "second" {
		t.Errorf("content = %q", b)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}

	// the target held open without delete sharing (Go's own open): the rename keeps failing, the temp file goes
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	err = writeFileReplace(path, []byte("third"), "3-0")
	f.Close()
	if err == nil {
		t.Log("rename over an open file succeeded on this system")
	}
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left after a failed rename: %s", e.Name())
		}
	}
}
