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
	"modavd/internal/avd"
	"modavd/internal/guestpatch"
	"modavd/internal/logx"
	"modavd/internal/ndkfix"
	"modavd/internal/testutil"
	"modavd/internal/vkfix"
	"modavd/internal/win"
)

var (
	gfVK  = guestpatch.Vulkan()
	gfNDK = guestpatch.NDK()
	gfAll = []guestpatch.Profile{gfVK, gfNDK}
)

// gfLine is p's status line of libfix.sh (or of the check script).
func gfLine(p guestpatch.Profile, s string) string { return p.GuestPath + " " + s + "\r\n" }

func TestLibFixKind(t *testing.T) {
	fix := ndkfix.FixSHA256
	cases := []struct {
		status     string
		kind, rest string
	}{
		{"MOUNTED " + fix, "MOUNTED", fix},
		{"ALREADY " + fix, "ALREADY", fix},
		{"ALREADY 1234abcd", "ALREADY", "1234abcd"},
		{"IMAGE_PATCHED", "IMAGE_PATCHED", ""},
		{"ABSENT", "ABSENT", ""},
		{"NEED_UPLOAD", "NEED_UPLOAD", ""},
		{"READY", "READY", ""},
		{"SKIPPED image 0000", "SKIPPED", "image 0000"},
		{"SKIPPED patched 1111", "SKIPPED", "patched 1111"},
		{"FAILED chcon u:object_r:system_lib_file:s0 -> u:object_r:shell_data_file:s0", "FAILED",
			"chcon u:object_r:system_lib_file:s0 -> u:object_r:shell_data_file:s0"},
		{"", "", ""},
		{"MOUNTEDX", "", ""},
		{"/system/bin/sh: libfix.sh: not found", "", ""},
	}
	for _, c := range cases {
		if kind, rest := libFixKind(c.status); kind != c.kind || rest != c.rest {
			t.Errorf("libFixKind(%q) = %q %q; want %q %q", c.status, kind, rest, c.kind, c.rest)
		}
	}
}

// TestLibFixStatus: one status per target, adb noise and the script's last line ignored, CR stripped.
func TestLibFixStatus(t *testing.T) {
	out := "some noise\r\n" + gfLine(gfVK, "READY") + gfLine(gfNDK, "NEED_UPLOAD") + "UNCHANGED\r\n"
	want := map[string]string{gfVK.GuestPath: "READY", gfNDK.GuestPath: "NEED_UPLOAD"}
	if got := libFixStatus(out, gfAll); !reflect.DeepEqual(got, want) {
		t.Errorf("libFixStatus = %v", got)
	}
	// a target not asked about is not read; a later line wins
	out = gfLine(gfVK, "MOUNTED x") + gfLine(gfNDK, "READY") + gfLine(gfNDK, "MOUNTED y")
	if got := libFixStatus(out, []guestpatch.Profile{gfNDK}); !reflect.DeepEqual(got, map[string]string{gfNDK.GuestPath: "MOUNTED y"}) {
		t.Errorf("libFixStatus(ndk) = %v", got)
	}
}

// TestLibFixMountCommand: the script's mount arguments, five per library in the order libfix.sh reads them.
func TestLibFixMountCommand(t *testing.T) {
	got := libFixMountCommand("/data/local/tmp/libfix.sh", []guestpatch.Profile{gfNDK})
	want := "su 0 sh /data/local/tmp/libfix.sh mount /system/lib64/libndk_translation.so " +
		"/data/local/tmp/libndk_translation.hintnop1.so /data/local/tmp/ndkfix-upload.so " + ndkfix.OrigSHA256 + " " +
		ndkfix.FixSHA256
	if got != want {
		t.Errorf("command\n got %s\nwant %s", got, want)
	}
	if got := libFixMountCommand("r", gfAll); strings.Count(got, " ") != 4+2*5 {
		t.Errorf("two libraries: %s", got)
	}
}

// TestGuestFixCheckReport: an adopted instance warns only when a fix applies to its image and is not mounted.
func TestGuestFixCheckReport(t *testing.T) {
	cases := []struct {
		p    guestpatch.Profile
		line string
		warn bool
		has  string
	}{
		{gfVK, "mounted", false, "客体 Vulkan 驱动补丁: 本次开机已挂上"},
		{gfNDK, "mounted", false, "ARM 转译层补丁: 本次开机已挂上"},
		{gfNDK, "image " + ndkfix.FixSHA256, false, "镜像自带的ARM 转译层库已是补丁版"},
		{gfVK, "image " + vkfix.OrigSHA256, true, "客体 Vulkan 驱动补丁这次开机没挂上：实例已在运行"},
		{gfNDK, "image " + ndkfix.OrigSHA256, true, "要挂就先关掉这个实例再启动"},
		{gfNDK, "image " + strings.Repeat("ab", 32), false, "ARM 转译层库不是预期的镜像版本"},
		{gfNDK, "absent", false, "镜像里没有ARM 转译层库（/system/lib64/libndk_translation.so）"},
		{gfNDK, "image ", true, "读不出"}, // sha256sum failed
		{gfNDK, "", true, "读不出ARM 转译层补丁这次开机挂没挂"},
	}
	for _, c := range cases {
		r := guestFixCheckReport(4, c.line, "error: device offline", c.p)
		if r.warn != c.warn || !strings.HasPrefix(r.msg, "实例 4 ") || !strings.Contains(r.msg, c.has) {
			t.Errorf("guestFixCheckReport(%s, %q) = %v %q", c.p.Name, c.line, r.warn, r.msg)
		}
	}
	// adb failed: every library unreadable, the message carries adb's text
	rs := guestFixCheckReports(4, "error: device offline\n", gfAll)
	if len(rs) != 2 || !rs[0].warn || !rs[1].warn || !strings.Contains(rs[1].msg, "device offline") {
		t.Errorf("adb failed: %v", rs)
	}
	// the check reads the same mount test as libfix.sh, covers every target and never needs root
	s := guestFixCheckScript(gfAll)
	if !strings.HasPrefix(s, "for t in "+vkfix.GuestPath+" "+ndkfix.GuestPath+"; do ") ||
		!strings.Contains(s, `grep -qF " $t " /proc/self/mountinfo`) || strings.Contains(s, "su ") {
		t.Errorf("check script = %s", s)
	}
}

// TestGuestFixMountAllowed: an owed fix is mounted only when nothing anybody uses is in front: the default home app,
// SystemUI, or no resumed activity at all. Everything else, unreadable output included, defers it.
func TestGuestFixMountAllowed(t *testing.T) {
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
		allowed, front := guestFixMountAllowed(c.out)
		if allowed != c.allowed || front != c.front {
			t.Errorf("%s: guestFixMountAllowed = %v %q, want %v %q", c.name, allowed, front, c.allowed, c.front)
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

// TestGuestFixDeferredReport: owed fixes left out because the guest is in use stay owed only while the image needs
// one and it is not mounted (or that cannot be read); one warning names those, what is in the way and the deadline.
func TestGuestFixDeferredReport(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.Local)
	until := now.Add(25*time.Minute + 30*time.Second)
	const (
		both    = "实例 3 的客体 Vulkan 驱动补丁、ARM 转译层补丁还没挂："
		front   = "现在有应用在前台，挂上要重启 zygote；关掉应用后在 18:25 前再 start 一次，过了这个时间就等下次开机自动挂"
		install = "现在有安装正在进行，挂上要重启 zygote；等安装完在 18:25 前再 start 一次，过了这个时间就等下次开机自动挂"
	)
	orig := gfLine(gfVK, "image "+vkfix.OrigSHA256) + gfLine(gfNDK, "image "+ndkfix.OrigSHA256)
	game := foregroundBlocker("com.miHoYo.hkrpg")
	cases := []struct {
		name    string
		out     string
		b       mountBlocker
		until   time.Time
		pending bool
		warn    string   // the one warning, exact ("" none)
		infos   []string // parts of the info lines, in order
	}{
		{"both missing, game in front", orig, game, until, true, both + front + "（前台: com.miHoYo.hkrpg）", nil},
		{"both missing, front unreadable", orig, foregroundBlocker(""), until, true,
			both + front + "（读不出前台是什么，按有应用在用处理）", nil},
		// an instance an older modavd booted: Vulkan mounted, the translator not
		{"translator missing", gfLine(gfVK, "mounted") + gfLine(gfNDK, "image "+ndkfix.OrigSHA256), game, until, true,
			"实例 3 的ARM 转译层补丁还没挂：" + front + "（前台: com.miHoYo.hkrpg）", []string{"客体 Vulkan 驱动补丁: 本次开机已挂上"}},
		{"check unreadable", "error: device offline\n", game, until, true,
			both + front + "（前台: com.miHoYo.hkrpg；这次开机挂没挂也读不出: error: device offline）", nil},
		{"check gave nothing", "", game, until, true, both + front + "（前台: com.miHoYo.hkrpg；这次开机挂没挂也读不出: 无输出）", nil},
		{"missing, install running", orig, installBlocker(nil), until, true, both + install, nil},
		{"install lock unreadable", orig, installBlocker(errors.New("拒绝访问。")), until, true,
			both + install + "（安装锁读不了（拒绝访问。），按有安装正在进行处理）", nil},
		{"deadline passed", orig, game, now.Add(-5 * time.Minute), true,
			both + "现在有应用在前台，挂上要重启 zygote；这次开机接着做设置的期限（17:55）已过，等下次开机自动挂（前台: com.miHoYo.hkrpg）", nil},
		{"deadline unknown", orig, game, time.Time{}, true,
			both + "现在有应用在前台，挂上要重启 zygote；关掉应用后再 start 一次，或下次开机自动挂（前台: com.miHoYo.hkrpg）", nil},
		{"both mounted already", gfLine(gfVK, "mounted") + gfLine(gfNDK, "mounted"), game, until, false, "",
			[]string{"客体 Vulkan 驱动补丁: 本次开机已挂上", "ARM 转译层补丁: 本次开机已挂上"}},
		{"patched or unexpected images", gfLine(gfVK, "image "+vkfix.FixSHA256) + gfLine(gfNDK, "image "+strings.Repeat("cd", 32)),
			installBlocker(nil), until, false, "", []string{"已是补丁版", "不是预期的镜像版本"}},
		{"no translator in the image", gfLine(gfVK, "mounted") + gfLine(gfNDK, "absent"), game, until, false, "",
			[]string{"本次开机已挂上", "镜像里没有ARM 转译层库"}},
	}
	for _, c := range cases {
		pending, reports := guestFixDeferredReport(3, c.out, c.b, c.until, now, gfAll)
		var warns, infos []string
		for _, r := range reports {
			if r.warn {
				warns = append(warns, r.msg)
			} else {
				infos = append(infos, r.msg)
			}
		}
		if pending != c.pending {
			t.Errorf("%s: pending %v", c.name, pending)
		}
		if c.warn == "" && len(warns) != 0 || c.warn != "" && (len(warns) != 1 || warns[0] != c.warn) {
			t.Errorf("%s: warnings %q, want %q", c.name, warns, c.warn)
		}
		if len(infos) != len(c.infos) {
			t.Errorf("%s: infos %q, want %q", c.name, infos, c.infos)
			continue
		}
		for k, s := range c.infos {
			if !strings.HasPrefix(infos[k], "实例 3 ") || !strings.Contains(infos[k], s) {
				t.Errorf("%s: info %q lacks %q", c.name, infos[k], s)
			}
		}
	}
}

// fakeGuest is a guest for mountGuestFixes: canned answers per command, every command recorded by a short name, the
// libraries of every libfix.sh run, and for each run whether the guest lock was held meanwhile.
type fakeGuest struct {
	t        *testing.T
	lock     string          // guestLockPath of the instance
	probes   []string        // successive foregroundProbeScript outputs
	fixes    []string        // successive libfix.sh outputs
	check    string          // guestFixCheckScript output
	pushFail map[string]bool // upload paths whose push fails
	onPush   func()          // runs during each push of a patched library
	commands []string
	runs     [][]string // the targets of each libfix.sh run
	pushes   []string   // the upload paths pushed to
	locked   []bool
}

const fakeLibFixRemote = "/data/local/tmp/libfix.sh"

func (f *fakeGuest) io() guestFixIO {
	return guestFixIO{
		guestIO: guestIO{shell: f.shell, adb: f.adb},
		remote:  fakeLibFixRemote,
		library: func(p guestpatch.Profile) string { return `D:\r\guest-patches\` + p.CacheName },
		waitBack: func(string) bool {
			f.commands = append(f.commands, "wait")
			return true
		},
	}
}

func (f *fakeGuest) shell(command string, _ time.Duration) adb.Result {
	switch {
	case command == "pidof com.android.systemui":
		f.commands = append(f.commands, "pidof")
		return adb.Result{Out: "1234\r\n"}
	case command == foregroundProbeScript:
		f.commands = append(f.commands, "probe")
		if len(f.probes) == 0 {
			f.t.Error("probe asked for more often than expected")
			return adb.Result{Out: ""}
		}
		out := f.probes[0]
		f.probes = f.probes[1:]
		return adb.Result{Out: out}
	case command == guestFixCheckScript(gfAll):
		f.commands = append(f.commands, "check")
		return adb.Result{Out: f.check}
	case strings.HasPrefix(command, "su 0 sh "+fakeLibFixRemote+" mount "):
		f.commands = append(f.commands, "fix")
		args := strings.Fields(strings.TrimPrefix(command, "su 0 sh "+fakeLibFixRemote+" mount "))
		var targets []string
		for len(args) >= 5 {
			var p guestpatch.Profile
			for _, q := range gfAll {
				if q.GuestPath == args[0] {
					p = q
				}
			}
			if want := []string{p.GuestPath, p.CachePath, p.UploadPath, p.OrigSHA256, p.FixSHA256}; !reflect.DeepEqual(args[:5], want) {
				f.t.Errorf("libfix.sh arguments %q, want %q", args[:5], want)
			}
			targets = append(targets, args[0])
			args = args[5:]
		}
		if len(args) != 0 {
			f.t.Errorf("libfix.sh arguments left over: %q", args)
		}
		f.runs = append(f.runs, targets)
		// a second handle conflicts like another process does (win.TryLock): busy = somebody holds it
		l, free, err := win.TryLock(f.lock)
		if err != nil {
			f.t.Fatal(err)
		}
		l.Unlock()
		f.locked = append(f.locked, !free)
		if len(f.fixes) == 0 {
			f.t.Error("libfix.sh ran more often than expected")
			return adb.Result{Out: "UNCHANGED"}
		}
		out := f.fixes[0]
		f.fixes = f.fixes[1:]
		return adb.Result{Out: out}
	}
	f.t.Errorf("unexpected command %q", command)
	return adb.Result{Code: 1, Out: "unexpected command"}
}

func (f *fakeGuest) adb(_ time.Duration, args ...string) adb.Result {
	if len(args) == 3 && args[0] == "push" {
		for _, p := range gfAll {
			if args[1] == `D:\r\guest-patches\`+p.CacheName && args[2] == p.UploadPath {
				f.commands = append(f.commands, "push")
				f.pushes = append(f.pushes, args[2])
				if f.onPush != nil {
					f.onPush()
				}
				if f.pushFail[args[2]] {
					return adb.Result{Code: 1, Out: "adb: error: failed to copy: No space left on device"}
				}
				return adb.Result{Out: "1 file pushed"}
			}
		}
	}
	f.t.Errorf("unexpected adb %q", args)
	return adb.Result{Code: 1, Out: "unexpected adb"}
}

// TestMountGuestFixes walks the mount against a fake guest. Both libraries go through one libfix.sh run and one
// zygote restart; a library that needs an upload holds back the mount of all of them until the upload is there (one
// push per library, one second run, still one restart). A launched boot mounts without a foreground question but
// still under the guest lock (an install of another process defers it); an owed one passes owedMountGate before every
// libfix.sh run that may mount (the guest lock, held over that run and freed right after it, then the foreground
// probe), so a game launched or an install started while a patched library was being uploaded still defers the
// zygote restart.
func TestMountGuestFixes(t *testing.T) {
	const (
		gameFront = "top:     mResumedActivity: ActivityRecord{9f1 u0 com.miHoYo.hkrpg/.Game t12}\r\n" +
			"home: de.jrpie.android.launcher/de.jrpie.android.launcher.ui.HomeActivity\r\n"
		homeFront = "top:   topResumedActivity=ActivityRecord{1 u0 de.jrpie.android.launcher/.ui.HomeActivity t5}\r\n" +
			"home: de.jrpie.android.launcher/de.jrpie.android.launcher.ui.HomeActivity\r\n"
		both = "客体 Vulkan 驱动补丁、ARM 转译层补丁"
	)
	var (
		vk, ndk     = gfVK.GuestPath, gfNDK.GuestPath
		mountedVK   = gfLine(gfVK, "MOUNTED "+gfVK.FixSHA256)
		mountedNDK  = gfLine(gfNDK, "MOUNTED "+gfNDK.FixSHA256)
		mountedBoth = mountedVK + mountedNDK + "RESTARTED\r\n"
		uploadNDK   = gfLine(gfVK, "READY") + gfLine(gfNDK, "NEED_UPLOAD") + "UNCHANGED\r\n"
		uploadBoth  = gfLine(gfVK, "NEED_UPLOAD") + gfLine(gfNDK, "NEED_UPLOAD") + "UNCHANGED\r\n"
		origBoth    = gfLine(gfVK, "image "+gfVK.OrigSHA256) + gfLine(gfNDK, "image "+gfNDK.OrigSHA256)
		mountedOld  = gfLine(gfVK, "mounted") + gfLine(gfNDK, "mounted")
		bothRuns    = []string{vk, ndk}
	)
	until := time.Now().Add(20 * time.Minute)
	retry := "在 " + clockText(until, time.Now()) + " 前再 start 一次，过了这个时间就等下次开机自动挂"
	cases := []struct {
		name               string
		owed               bool
		installing         bool // an install holds the guest lock from the start
		installDuringPush  bool // an install takes it while a library is uploaded
		pushFail           []string
		probes, fixes      []string
		check              string
		commands           []string
		runs               [][]string
		pushes             []string
		locked             []bool
		restarted, pending bool
		level              logx.Level
		has                []string
	}{
		{name: "launched, cached in the guest", fixes: []string{mountedBoth},
			commands: []string{"pidof", "fix", "wait"}, runs: [][]string{bothRuns}, locked: []bool{true}, restarted: true,
			level: logx.Info, has: []string{"客体库补丁已挂上（" + both + "），zygote 重启后系统界面已回来"}},
		{name: "launched, both uploaded", fixes: []string{uploadBoth, mountedBoth},
			commands: []string{"pidof", "fix", "push", "push", "fix", "wait"}, runs: [][]string{bothRuns, bothRuns},
			pushes: []string{gfVK.UploadPath, gfNDK.UploadPath}, locked: []bool{true, true}, restarted: true,
			level: logx.Info, has: []string{"第 3 步（挂载）"}},
		{name: "launched, translator uploaded", fixes: []string{uploadNDK, mountedBoth},
			commands: []string{"pidof", "fix", "push", "fix", "wait"}, runs: [][]string{bothRuns, bothRuns},
			pushes: []string{gfNDK.UploadPath}, locked: []bool{true, true}, restarted: true,
			level: logx.Info, has: []string{"ARM 转译层补丁已挂上（sha256 " + gfNDK.FixSHA256 + "）"}},
		// the translator's push fails: the second run mounts the Vulkan driver alone
		{name: "launched, translator push failed", pushFail: []string{gfNDK.UploadPath},
			fixes: []string{uploadNDK, mountedVK + "RESTARTED\r\n"}, commands: []string{"pidof", "fix", "push", "fix", "wait"},
			runs: [][]string{bothRuns, {vk}}, pushes: []string{gfNDK.UploadPath}, locked: []bool{true, true}, restarted: true,
			level: logx.Warn, has: []string{"ARM 转译层补丁没挂: 推送补丁库失败: adb: error: failed to copy"}},
		{name: "launched, every push failed", pushFail: []string{gfVK.UploadPath, gfNDK.UploadPath}, fixes: []string{uploadBoth},
			commands: []string{"pidof", "fix", "push", "push"}, runs: [][]string{bothRuns},
			pushes: []string{gfVK.UploadPath, gfNDK.UploadPath}, locked: []bool{true},
			level: logx.Warn, has: []string{"客体 Vulkan 驱动补丁没挂: 推送补丁库失败"}},
		// another image release: the translator is left alone, the Vulkan driver still mounted
		{name: "launched, translator of another image",
			fixes:    []string{mountedVK + gfLine(gfNDK, "SKIPPED image "+strings.Repeat("ab", 32)) + "RESTARTED\r\n"},
			commands: []string{"pidof", "fix", "wait"}, runs: [][]string{bothRuns}, locked: []bool{true}, restarted: true,
			level: logx.Warn, has: []string{"ARM 转译层补丁没挂: SKIPPED image abab"}},
		{name: "launched, no translator in the image", fixes: []string{mountedVK + gfLine(gfNDK, "ABSENT") + "RESTARTED\r\n"},
			commands: []string{"pidof", "fix", "wait"}, runs: [][]string{bothRuns}, locked: []bool{true}, restarted: true,
			level: logx.Info, has: []string{"镜像里没有ARM 转译层库（/system/lib64/libndk_translation.so），不用挂"}},
		{name: "launched, something else mounted", fixes: []string{gfLine(gfVK, "ALREADY "+gfVK.FixSHA256) +
			gfLine(gfNDK, "ALREADY 1234") + "UNCHANGED\r\n"}, commands: []string{"pidof", "fix"}, runs: [][]string{bothRuns},
			locked: []bool{true}, level: logx.Warn, has: []string{"ARM 转译层库上已经挂着别的文件（sha256 1234）"}},
		{name: "launched, script printed nothing for the translator", fixes: []string{mountedVK + "RESTARTED\r\n"},
			commands: []string{"pidof", "fix", "wait"}, runs: [][]string{bothRuns}, locked: []bool{true}, restarted: true,
			level: logx.Warn, has: []string{"ARM 转译层补丁没挂: 客体脚本没有报这个库的结果"}},
		// a launched boot asks no foreground question, but an install of another process defers the mount the same way
		{name: "launched, install running", installing: true, check: origBoth,
			commands: []string{"pidof", "check"}, pending: true,
			level: logx.Warn, has: []string{"的" + both + "还没挂：现在有安装正在进行，挂上要重启 zygote；等安装完" + retry}},
		{name: "owed, home in front, cached", owed: true, probes: []string{homeFront}, fixes: []string{mountedBoth},
			commands: []string{"pidof", "probe", "fix", "wait"}, runs: [][]string{bothRuns}, locked: []bool{true}, restarted: true,
			level: logx.Info, has: []string{"没有应用在用，也没有安装在进行，接着挂"}},
		{name: "owed, game in front", owed: true, probes: []string{gameFront}, check: origBoth,
			commands: []string{"pidof", "probe", "check"}, pending: true,
			level: logx.Warn, has: []string{both + "还没挂：现在有应用在前台，挂上要重启 zygote；关掉应用后" + retry, "（前台: com.miHoYo.hkrpg）"}},
		// the Vulkan driver of an older modavd's start is mounted, the translator is not: still owed
		{name: "owed, game in front, translator missing", owed: true, probes: []string{gameFront},
			check: gfLine(gfVK, "mounted") + gfLine(gfNDK, "image "+gfNDK.OrigSHA256), commands: []string{"pidof", "probe", "check"},
			pending: true, level: logx.Warn, has: []string{"实例 3 的ARM 转译层补丁还没挂：现在有应用在前台"}},
		{name: "owed, game in front, fixes mounted already", owed: true, probes: []string{gameFront}, check: mountedOld,
			commands: []string{"pidof", "probe", "check"}, level: logx.Info, has: []string{"ARM 转译层补丁: 本次开机已挂上"}},
		{name: "owed, install running", owed: true, installing: true, check: origBoth,
			commands: []string{"pidof", "check"}, pending: true,
			level: logx.Warn, has: []string{"现在有安装正在进行，挂上要重启 zygote；等安装完" + retry}},
		{name: "owed, install running, fixes mounted already", owed: true, installing: true, check: mountedOld,
			commands: []string{"pidof", "check"}, level: logx.Info, has: []string{"本次开机已挂上"}},
		{name: "owed, upload, game launched meanwhile", owed: true, probes: []string{homeFront, gameFront},
			fixes: []string{uploadNDK}, commands: []string{"pidof", "probe", "fix", "push", "probe"}, runs: [][]string{bothRuns},
			pushes: []string{gfNDK.UploadPath}, locked: []bool{true}, pending: true,
			level: logx.Warn, has: []string{both + "还没挂：现在有应用在前台，挂上要重启 zygote；关掉应用后" + retry, "（前台: com.miHoYo.hkrpg）"}},
		{name: "owed, upload, still home", owed: true, probes: []string{homeFront, homeFront},
			fixes: []string{uploadNDK, mountedBoth}, commands: []string{"pidof", "probe", "fix", "push", "probe", "fix", "wait"},
			runs: [][]string{bothRuns, bothRuns}, locked: []bool{true, true}, restarted: true, level: logx.Info,
			has: []string{"第 3 步（挂载）"}},
		{name: "owed, upload, install started meanwhile", owed: true, installDuringPush: true, probes: []string{homeFront},
			fixes: []string{uploadNDK}, commands: []string{"pidof", "probe", "fix", "push"}, runs: [][]string{bothRuns},
			locked: []bool{true}, pending: true,
			level: logx.Warn, has: []string{"现在有安装正在进行，挂上要重启 zygote；等安装完" + retry}},
		{name: "owed, upload, mount failed", owed: true, probes: []string{homeFront, homeFront},
			fixes:    []string{uploadBoth, gfLine(gfVK, "FAILED mount 0000") + gfLine(gfNDK, "FAILED mount 0000") + "UNCHANGED\r\n"},
			commands: []string{"pidof", "probe", "fix", "push", "push", "probe", "fix"}, runs: [][]string{bothRuns, bothRuns},
			locked: []bool{true, true}, level: logx.Warn, has: []string{"客体 Vulkan 驱动补丁没挂: FAILED mount 0000"}},
	}
	for _, c := range cases {
		m := markerTestManager(t)
		var lines []logLine
		m.Log = logx.New(func(level logx.Level, msg string) { lines = append(lines, logLine{level, msg}) })
		f := &fakeGuest{t: t, lock: m.guestLockPath(3), probes: c.probes, fixes: c.fixes, check: c.check,
			pushFail: map[string]bool{}}
		for _, p := range c.pushFail {
			f.pushFail[p] = true
		}
		var install *win.FileLock
		takeInstall := func() {
			if install != nil {
				return
			}
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

		restarted, pending := m.mountGuestFixes(context.Background(), 3, f.io(), gfAll, c.owed, until)
		install.Unlock()

		if restarted != c.restarted || pending != c.pending {
			t.Errorf("%s: restarted %v pending %v", c.name, restarted, pending)
		}
		if !reflect.DeepEqual(f.commands, c.commands) {
			t.Errorf("%s: commands %q, want %q", c.name, f.commands, c.commands)
		}
		if !reflect.DeepEqual(f.runs, c.runs) {
			t.Errorf("%s: libfix.sh runs %q, want %q", c.name, f.runs, c.runs)
		}
		if c.pushes != nil && !reflect.DeepEqual(f.pushes, c.pushes) {
			t.Errorf("%s: pushes %q, want %q", c.name, f.pushes, c.pushes)
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

// TestGuestLibFixes: the patches follow the instance's configured guest ABI; ARM64 and unknown or unreadable ABIs
// get none (and say why).
func TestGuestLibFixes(t *testing.T) {
	m := markerTestManager(t)
	write := func(i int, body string) {
		p := avd.ConfigPath(m.Root, i)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(1, "abi.type=x86_64\nhw.cpu.arch=x86_64\n")
	write(2, "abi.type=arm64-v8a\nhw.cpu.arch=arm64\n")
	write(3, "hw.cpu.arch=mips\nabi.type=mips\n")
	ps, err := m.guestLibFixes(&avd.Instance{Index: 1, Config: mustIni("abi.type=x86_64\nhw.cpu.arch=x86_64\n")})
	if err != nil || len(ps) != 2 || ps[0].GuestPath != vkfix.GuestPath || ps[1].GuestPath != ndkfix.GuestPath {
		t.Errorf("x86_64: %v %v", ps, err)
	}
	if ps, err := m.guestLibFixes(&avd.Instance{Index: 2, Config: mustIni("abi.type=arm64-v8a\nhw.cpu.arch=arm64\n")}); ps != nil || err == nil || !strings.Contains(err.Error(), "没有 ARM 转译层") {
		t.Errorf("arm64: %v %v", ps, err)
	}
	if ps, err := m.guestLibFixes(&avd.Instance{Index: 3, Config: mustIni("hw.cpu.arch=mips\nabi.type=mips\n")}); ps != nil || err == nil || !strings.Contains(err.Error(), `"mips"`) {
		t.Errorf("mips: %v %v", ps, err)
	}
	if ps, err := m.guestLibFixes(&avd.Instance{Index: 4, Config: mustIni("hw.cpu.arch=x86_64\n")}); err != nil || len(ps) != 2 {
		t.Errorf("missing abi.type defaults to x86_64: %v %v", ps, err)
	}
}

func mustIni(body string) *avd.Ini {
	cfg := avd.NewIni()
	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			cfg.Set(k, v)
		}
	}
	return cfg
}

func TestGuestFixConstants(t *testing.T) {
	// the upload name is AUTO-MAS vulkanfix.sh's (u= there); the cache name is vkfix's (it has the template fix)
	if gfVK.CachePath != "/data/local/tmp/libvulkan_enc.khrfix-bindlock2-dstpl1.so" ||
		gfVK.UploadPath != "/data/local/tmp/vulkanfix-upload.so" || gfVK.GuestPath != "/vendor/lib64/libvulkan_enc.so" ||
		gfVK.CacheName != "libvulkan_enc.khrfix-bindlock2-dstpl1.so" {
		t.Errorf("vulkan profile changed: %+v", gfVK)
	}
	if gfNDK.CachePath != "/data/local/tmp/"+ndkfix.CacheName || gfNDK.GuestPath != "/system/lib64/libndk_translation.so" {
		t.Errorf("translator profile: %+v", gfNDK)
	}
}

func TestWriteFileReplace(t *testing.T) {
	dir := testutil.TempDir(t)
	path := filepath.Join(dir, ndkfix.CacheName)
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
