package emu

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/guest"
	"modavd/internal/guestpatch"
	"modavd/internal/host"
	"modavd/internal/root"
)

const (
	guestFixScriptTimeout = 60 * time.Second
	frameworkBackTimeout  = 120 * time.Second
)

// guestLibFixes uses the instance's configured ABI, never the host OS. The instance was read successfully by the
// start path, so use that snapshot instead of opening config.ini a second time; a missing abi.type is the legacy
// x86_64 default used by the manager's bridge support checks.
func (m *Manager) guestLibFixes(inst *avd.Instance) ([]guestpatch.Profile, error) {
	if inst == nil || inst.Config == nil {
		return nil, fmt.Errorf("读取实例客体架构: 实例配置为空")
	}
	abi := inst.Config.Value("abi.type")
	if abi == "" {
		abi = "x86_64"
	}
	profiles := guestpatch.ForABI(abi)
	if len(profiles) == 0 {
		if abi == "arm64-v8a" {
			return nil, fmt.Errorf("ARM64 客体直接跑 ARM 代码，没有 ARM 转译层；API35 的 Vulkan 补丁也尚未验收，保留镜像原库，" +
				"不套用 API34 补丁，也不挂载或重启 zygote")
		}
		return nil, fmt.Errorf("客体架构 %q 没有适用的客体库补丁", abi)
	}
	return profiles, nil
}

// ensureGuestFixes bind-mounts the patched guest libraries made on this host over the image's own and restarts
// zygote once, for verified API34 x86_64 guests (internal/guestpatch):
//   - the Vulkan encoder /vendor/lib64/libvulkan_enc.so (AUTO-MAS _ensure_vulkan_fix; launcher -VulkanFix; user
//     10-16): Star Rail's descriptor templates (khrfix) and Endfield's unlocked vkBindImageMemory lookups (bindlock);
//   - the ARM translation layer /system/lib64/libndk_translation.so (ModAVD/ModAVD#131): WFE, WFI, SEV and SEVL run as
//     NOP instead of SIGILL (Unity 6 spin locks, World of Airports). zygote loads it before it forks any app, with or
//     without the execution bridge (whose shim opens the same path and hands apps off the gate list back to it).
//
// Games that use none of these paths are unaffected. ARM64 guests run ARM code natively, and API35's Vulkan has not
// been validated: they keep their original libraries.
//
//   - libfix.sh first checks every library: whether this boot already has something mounted over it (nothing to do
//     then), and the image library's hash against the profile; the patched copies are cached in the guest's
//     /data/local/tmp, so the next boot mounts them without an upload. All of them are mounted in one run, followed
//     by the one zygote restart (a mount left without it would read as done while zygote runs the old file).
//   - Without a guest cache the host copy in <root>\guest-patches is pushed; without that, the guest's own library is
//     pulled and patched here (patchedGuestLibrary). The patched files are never shipped.
//   - An instance that was already running (setupAdopted) is only checked: the zygote restart would end the game. A
//     missing fix is a warning then (it reaches StartResult.Warnings), since that whole boot runs without it.
//   - A boot whose setup an earlier start left unfinished (setupOwed) may have been put to use since: AUTO-MAS
//     launched a game on it, an MCP client went on with it (install_apk among others), a user plays in the window.
//     Each libfix.sh run that may mount first passes owedMountGate (no install holds the guest lock, and
//     guestFixMountAllowed finds the guest unused: home screen, SystemUI or nothing in front); otherwise nothing is
//     mounted and, when a fix is still missing, pending=true: the setup marker stays and the result says
//     SetupPending, so a start after the app is closed or the install is done (before until, the marker's deadline)
//     mounts it, or the next boot does.
//
// Nothing is written to the image: the mounts end with the guest's shutdown (libfix.sh revert takes them away within
// a boot). Failures are only logged. Returns whether zygote was restarted, and whether a fix is still owed.
func (m *Manager) ensureGuestFixes(ctx context.Context, inst *avd.Instance, mode bootSetup, until time.Time) (restarted, pending bool) {
	i := inst.Index
	profiles, err := m.guestLibFixes(inst)
	if err != nil {
		m.Log.Warnf("实例 %d 客体库补丁未执行: %v", i, err)
		return false, false
	}
	if mode == setupAdopted {
		res := m.shell(ctx, i, guestFixCheckScript(profiles), 0)
		for _, r := range guestFixCheckReports(i, res.Out, profiles) {
			m.logReport(r)
		}
		return false, false
	}
	remote, ok := m.pushGuestScript(ctx, i, guest.LibFix)
	if !ok {
		m.Log.Warnf("实例 %d 客体库补丁没挂: 推送客体脚本失败", i)
		return false, false
	}
	g := m.guestIO(ctx, i)
	return m.mountGuestFixes(ctx, i, guestFixIO{
		guestIO: g,
		remote:  remote,
		library: func(p guestpatch.Profile) string { return m.patchedGuestLibrary(ctx, i, p) },
		waitBack: func(uiBefore string) bool {
			return m.waitFrameworkBack(ctx, g, uiBefore, frameworkBackTimeout)
		},
	}, profiles, mode == setupOwed, until)
}

// guestFixIO is what mountGuestFixes works through: the instance's adb in Manager, a fake guest in tests.
type guestFixIO struct {
	guestIO
	remote   string                            // libfix.sh in the guest (pushed already)
	library  func(p guestpatch.Profile) string // the host's patched copy (patchedGuestLibrary); "" when it cannot be had
	waitBack func(uiBefore string) bool        // after the zygote restart (waitFrameworkBack)
}

// report is one log line: a warning or an info.
type report struct {
	warn bool
	msg  string
}

func (m *Manager) logReport(r report) {
	if r.warn {
		m.Log.Warnf("%s", r.msg)
	} else {
		m.Log.Infof("%s", r.msg)
	}
}

// mountBlocker is why an owed fix is not mounted now, for the messages: what is in the way ("有应用在前台"), what to
// do before the next start ("关掉应用后") and details ("" none).
type mountBlocker struct {
	what, then, detail string
}

// foregroundBlocker: guestFixMountAllowed refused (front as it returned it, "" when unreadable).
func foregroundBlocker(front string) mountBlocker {
	b := mountBlocker{what: "有应用在前台", then: "关掉应用后", detail: "前台: " + front}
	if front == "" {
		b.detail = "读不出前台是什么，按有应用在用处理"
	}
	return b
}

// installBlocker: the guest lock is held (err == nil) or cannot be taken for another reason, which counts the same
// (a needless deferral costs one more start, a wrong guess breaks an install).
func installBlocker(err error) mountBlocker {
	b := mountBlocker{what: "有安装正在进行", then: "等安装完"}
	if err != nil {
		b.detail = fmt.Sprintf("安装锁读不了（%v），按有安装正在进行处理", err)
	}
	return b
}

// owedMountGate is passed right before each libfix.sh run that may mount an owed fix (and so restart zygote, which
// ends every app and breaks an install that is running):
//   - the guest lock (guestLockPath), which InstallAPK holds while it installs: taken with TryLock, never waited
//     for (an install of a 3 GB game takes minutes, a start should not);
//   - then the foreground rule (guestFixMountAllowed), read again at every call: the patched library may have taken
//     tens of seconds to make and upload since the last one, and a game may have been launched meanwhile.
//
// ok: both allow it, and release frees the lock right after that run returns: the restart has been triggered by
// then, and an install started after it only meets a framework that is coming back (what InstallAPK's own boot check
// and adb's errors report). Otherwise b says what is in the way and nothing is held.
func (m *Manager) owedMountGate(i int, g guestIO) (release func(), b mountBlocker, ok bool) {
	l, free, err := host.TryLock(m.guestLockPath(i))
	if err != nil || !free {
		return nil, installBlocker(err), false
	}
	probe := g.shell(foregroundProbeScript, 0)
	allowed, front := guestFixMountAllowed(probe.Out)
	if !allowed {
		l.Unlock()
		m.Log.Debugf("实例 %d 前台检查: %s", i, orText(tail(joinNonEmptyLines(probe.Out, " | "), 300), "无输出"))
		return nil, foregroundBlocker(front), false
	}
	m.Log.Infof("实例 %d 这次开机的客体库补丁还没做，前台是 %s、没有应用在用，也没有安装在进行，接着挂（会重启 zygote）",
		i, front)
	return l.Unlock, mountBlocker{}, true
}

// libFixMountCommand runs `libfix.sh mount` as root for profiles.
func libFixMountCommand(remote string, profiles []guestpatch.Profile) string {
	var b strings.Builder
	b.WriteString("su 0 sh " + remote + " mount")
	for _, p := range profiles {
		for _, a := range []string{p.GuestPath, p.CachePath, p.UploadPath, p.OrigSHA256, p.FixSHA256} {
			b.WriteString(" " + a)
		}
	}
	return b.String()
}

// mountGuestFixes is the mounting part of ensureGuestFixes (a launched boot, or an owed one, owed=true: every run
// that may mount passes owedMountGate first; until is the marker's deadline for the messages). Returns whether zygote
// was restarted and whether an owed fix is still owed (deferred while the guest is in use).
func (m *Manager) mountGuestFixes(ctx context.Context, i int, fx guestFixIO, profiles []guestpatch.Profile, owed bool,
	until time.Time) (restarted, pending bool) {
	// gatedFix is one libfix.sh run over ps, for an owed fix only once owedMountGate allows it, with the guest lock
	// held over the run. ok=false: deferred (logged here) and nothing ran.
	gatedFix := func(ps []guestpatch.Profile, first bool) (out string, ok, pending bool) {
		var release func()
		var b mountBlocker
		allowed := false
		if owed {
			release, b, allowed = m.owedMountGate(i, fx.guestIO)
		} else {
			// A launched boot has no app of ours in front yet, but an install_apk of another modavd process may already
			// run (sys.boot_completed is 1 by now): the guest lock alone decides, the same way.
			l, free, err := host.TryLock(m.guestLockPath(i))
			if err == nil && free {
				release, allowed = l.Unlock, true
			} else {
				b = installBlocker(err)
			}
		}
		if allowed {
			defer release()
			return fx.shell(libFixMountCommand(fx.remote, ps), guestFixScriptTimeout).Out, true, false
		}
		if !first {
			// the first run said NEED_UPLOAD and mounted nothing: ps all want mounting. The uploads stay in the guest,
			// so the start that mounts them does not upload again (libfix.sh takes them into its cache).
			m.Log.Warnf("%s", guestFixDeferredMessage(i, ps, b, b.detail, until, time.Now()))
			return "", false, true
		}
		res := fx.shell(guestFixCheckScript(ps), 0)
		pending, reports := guestFixDeferredReport(i, res.Out, b, until, time.Now(), ps)
		for _, r := range reports {
			m.logReport(r)
		}
		return "", false, pending
	}

	uiBefore := strings.TrimSpace(fx.shell("pidof com.android.systemui", 0).Out)
	out, ok, pending := gatedFix(profiles, true)
	if !ok {
		return false, pending
	}
	m.Log.Infof("实例 %d 客体库补丁 第 1 步（客体检查）: %s", i, orText(joinNonEmptyLines(out, "；"), "无输出"))
	status := libFixStatus(out, profiles)
	uploadFailed := map[string]bool{}
	var next []guestpatch.Profile // a second run: the libraries ready and the ones uploaded now
	needUpload := false
	for _, p := range profiles {
		switch kind, _ := libFixKind(status[p.GuestPath]); kind {
		case "READY":
			next = append(next, p)
		case "NEED_UPLOAD":
			needUpload = true
			if m.uploadGuestFix(i, fx, p) {
				next = append(next, p)
			} else {
				uploadFailed[p.GuestPath] = true
			}
		}
	}
	if needUpload {
		if len(next) == 0 {
			return false, false
		}
		if out, ok, pending = gatedFix(next, false); !ok {
			return false, pending
		}
		m.Log.Infof("实例 %d 客体库补丁 第 3 步（挂载）: %s", i, orText(joinNonEmptyLines(out, "；"), "无输出"))
		second := libFixStatus(out, next)
		for _, p := range next {
			if s, ok := second[p.GuestPath]; ok {
				status[p.GuestPath] = s
			} else {
				delete(status, p.GuestPath) // no line this time: reported as such below
			}
		}
	}

	var mounted []string
	for _, p := range profiles {
		if uploadFailed[p.GuestPath] {
			continue // warned already
		}
		r, ok := guestFixResult(i, p, status[p.GuestPath], out)
		m.logReport(r)
		if ok {
			mounted = append(mounted, p.Name)
		}
	}
	if lastNonEmptyLine(out) != "RESTARTED" {
		return false, false
	}

	started := time.Now()
	back := fx.waitBack(uiBefore)
	if ctx.Err() != nil {
		return true, false // canceled while waiting: zygote did restart, but nothing is known about the framework
	}
	how := "超时未回来"
	if back {
		how = "已回来"
	}
	m.Log.Infof("实例 %d 客体库补丁已挂上（%s），zygote 重启后系统界面%s（%.0f 秒）", i, strings.Join(mounted, "、"), how,
		time.Since(started).Seconds())
	return true, false
}

// uploadGuestFix pushes the host's patched copy of p to p.UploadPath (patchedGuestLibrary makes it when needed).
// false: it could not be had or pushed (logged).
func (m *Manager) uploadGuestFix(i int, fx guestFixIO, p guestpatch.Profile) bool {
	library := fx.library(p)
	if library == "" {
		return false
	}
	res := fx.adb(60*time.Second, "push", library, p.UploadPath)
	if res.Code != 0 {
		m.Log.Warnf("实例 %d %s没挂: 推送补丁库失败: %s", i, p.Name, tail(res.Out, 200))
		return false
	}
	m.Log.Infof("实例 %d %s 第 2 步: 已推送 %s（sha256 %s）到 %s", i, p.Name, library, p.FixSHA256, p.UploadPath)
	return true
}

// guestFixResult judges p's final status line of libfix.sh (out is the whole output, for a missing line). ok: p was
// mounted in this run.
func guestFixResult(i int, p guestpatch.Profile, status, out string) (r report, ok bool) {
	kind, rest := libFixKind(status)
	switch kind {
	case "MOUNTED":
		return report{false, fmt.Sprintf("实例 %d %s已挂上（sha256 %s）", i, p.Name, rest)}, true
	case "ALREADY":
		if rest == p.FixSHA256 {
			return report{false, fmt.Sprintf("实例 %d %s本次开机已挂上（sha256 %s）", i, p.Name, rest)}, false
		}
		return report{true, fmt.Sprintf("实例 %d %s上已经挂着别的文件（sha256 %s），不是这次的补丁，没有动它", i, p.Lib, rest)}, false
	case "IMAGE_PATCHED":
		return report{false, fmt.Sprintf("实例 %d 镜像自带的%s已是补丁版，不用挂", i, p.Lib)}, false
	case "ABSENT":
		return report{false, fmt.Sprintf("实例 %d 镜像里没有%s（%s），不用挂", i, p.Lib, p.GuestPath)}, false
	case "":
		return report{true, fmt.Sprintf("实例 %d %s没挂: 客体脚本没有报这个库的结果: %s", i, p.Name,
			orText(tail(out, 200), "无输出"))}, false
	}
	// SKIPPED image: not the expected image; SKIPPED patched: the guest cache was bad, removed, re-uploaded next time;
	// FAILED: the mount did not take; READY / NEED_UPLOAD after the second run: another upload went wrong meanwhile
	return report{true, fmt.Sprintf("实例 %d %s没挂: %s", i, p.Name, status)}, false
}

// libFixStatus maps each profile's target to its status in libfix.sh's output ("<target> <STATUS> [details]"; the
// last line for a target wins). A target without a line is missing from the map.
func libFixStatus(out string, profiles []guestpatch.Profile) map[string]string {
	status := map[string]string{}
	for _, line := range outputLines(out) {
		line = strings.TrimSpace(line)
		for _, p := range profiles {
			if rest, ok := strings.CutPrefix(line, p.GuestPath+" "); ok {
				status[p.GuestPath] = strings.TrimSpace(rest)
			}
		}
	}
	return status
}

// libFixKind splits a status: its first word when that is one of libfix.sh's, and what follows the first space (the
// sha256 for MOUNTED / ALREADY). "" for anything else.
func libFixKind(status string) (kind, rest string) {
	word, rest, _ := strings.Cut(status, " ")
	switch word {
	case "NEED_UPLOAD", "READY", "ALREADY", "ABSENT", "IMAGE_PATCHED", "MOUNTED", "SKIPPED", "FAILED":
		return word, rest
	}
	return "", ""
}

// guestFixCheckScript is the check of the libraries (no root, nothing changed), one line per target: "<target>
// mounted" when something is mounted over it this boot (the test libfix.sh starts with), "<target> image <sha256>"
// of the image's own, or "<target> absent".
func guestFixCheckScript(profiles []guestpatch.Profile) string {
	targets := make([]string, len(profiles))
	for k, p := range profiles {
		targets[k] = p.GuestPath
	}
	return `for t in ` + strings.Join(targets, " ") + `; do if grep -qF " $t " /proc/self/mountinfo; then echo "$t mounted"; ` +
		`elif [ -e $t ]; then echo "$t image $(sha256sum $t | cut -d' ' -f1)"; else echo "$t absent"; fi; done`
}

// guestFixCheckReport judges p's line of guestFixCheckScript (line: what follows the target; out: the whole output,
// for the message when it is unreadable). A warning only when the fix applies to this image and is missing: the
// instance runs a whole boot without it then, and the warning says how to get it. Otherwise this boot's state is only
// logged: an image whose library already is the patched one or has none, or an unexpected image (the start that
// booted it reported that already).
func guestFixCheckReport(i int, line, out string, p guestpatch.Profile) report {
	switch sha, isImage := strings.CutPrefix(line, "image "); {
	case line == "mounted":
		return report{false, fmt.Sprintf("实例 %d %s: 本次开机已挂上", i, p.Name)}
	case line == "absent":
		return report{false, fmt.Sprintf("实例 %d 镜像里没有%s（%s），不用挂", i, p.Lib, p.GuestPath)}
	case isImage && sha == p.FixSHA256:
		return report{false, fmt.Sprintf("实例 %d 镜像自带的%s已是补丁版，不用挂", i, p.Lib)}
	case isImage && sha == p.OrigSHA256:
		return report{true, fmt.Sprintf("实例 %d %s这次开机没挂上：实例已在运行，挂它要重启 zygote（会结束正在运行的应用），"+
			"这次不挂；要挂就先关掉这个实例再启动", i, p.Name)}
	case isImage && len(sha) == 64:
		return report{false, fmt.Sprintf("实例 %d %s不是预期的镜像版本（sha256 %s），不挂%s", i, p.Lib, sha, p.Name)}
	}
	return report{true, fmt.Sprintf("实例 %d 读不出%s这次开机挂没挂: %s", i, p.Name, orText(tail(out, 200), "无输出"))}
}

// guestFixCheckReports is guestFixCheckReport for every profile.
func guestFixCheckReports(i int, out string, profiles []guestpatch.Profile) []report {
	lines := libFixStatus(out, profiles)
	reports := make([]report, len(profiles))
	for k, p := range profiles {
		reports[k] = guestFixCheckReport(i, lines[p.GuestPath], out, p)
	}
	return reports
}

// foregroundProbeScript prints, without root and without changing anything, the activity in front ("top: " and the
// first topResumedActivity / mResumedActivity line of dumpsys) and the default HOME activity ("home: " and the last
// line of resolve-activity --brief). Tagged lines: a dumpsys cut short by grep -m1 may add a "Broken pipe" line, and
// adb errors come without the tags.
const foregroundProbeScript = `echo "top: $(dumpsys activity activities | grep -m1 -E 'topResumedActivity|mResumedActivity')"; ` +
	`echo "home: $(cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.HOME | tail -n1)"`

// systemUIPackage in front (lock screen, notification shade) is no app in use.
const systemUIPackage = "com.android.systemui"

// guestFixMountAllowed decides from foregroundProbeScript's output whether an owed fix may be mounted now, i.e.
// whether restarting zygote ends nothing anybody uses: the resumed activity belongs to the default HOME app, to
// SystemUI, or there is none (topResumedActivity=null). A user app in front, or output that cannot be read, defers
// it: a needless deferral costs one more start, a wrong guess ends a running game.
//
// A HOME that resolves to the "android" package is the resolver (no default home set); the chooser activities of that
// package can also sit over a game, so it does not count as home. front names what is in front for the messages
// ("无" for nothing, "" when unreadable).
func guestFixMountAllowed(out string) (allowed bool, front string) {
	top, home, topOK, homeOK := parseForegroundProbe(out)
	if !topOK {
		return false, ""
	}
	pkg, none, ok := resumedPackage(top)
	switch {
	case !ok:
		return false, ""
	case none:
		return true, "无"
	case pkg == systemUIPackage:
		return true, pkg
	}
	if homeOK {
		if h, ok := componentPackage(home); ok && h == pkg && h != "android" {
			return true, pkg
		}
	}
	return false, pkg
}

// parseForegroundProbe returns the values of foregroundProbeScript's tagged lines; ok=false for a tag that is missing.
func parseForegroundProbe(out string) (top, home string, topOK, homeOK bool) {
	for _, line := range outputLines(out) {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "top:"); ok {
			top, topOK = strings.TrimSpace(v), true
		} else if v, ok := strings.CutPrefix(line, "home:"); ok {
			home, homeOK = strings.TrimSpace(v), true
		}
	}
	return top, home, topOK, homeOK
}

// resumedPackage reads a dumpsys line "topResumedActivity=ActivityRecord{1d5b2a5 u0 <pkg>/<activity> t5}" or
// "mResumedActivity: ActivityRecord{...}". none: the record is null (nothing resumed). ok=false when the line is
// neither (empty: dumpsys failed or printed something else).
func resumedPackage(line string) (pkg string, none, ok bool) {
	for _, mark := range []string{"topResumedActivity=", "mResumedActivity:"} {
		k := strings.Index(line, mark)
		if k < 0 {
			continue
		}
		v := strings.TrimSpace(line[k+len(mark):])
		if v == "null" {
			return "", true, true
		}
		if p, ok := componentPackage(v); ok {
			return p, false, true
		}
		return "", false, false
	}
	return "", false, false
}

// componentPackage is the package of the first "<pkg>/<class>" field of s (braces around it stripped); ok=false when
// there is none or the part before the slash is no package name ("No activity found", an error text).
func componentPackage(s string) (string, bool) {
	for _, f := range strings.Fields(s) {
		f = strings.Trim(f, "{}")
		pkg, _, found := strings.Cut(f, "/")
		if !found {
			continue
		}
		return pkg, validPackageName(pkg)
	}
	return "", false
}

// validPackageName: letters, digits, '_' and '.', starting with a letter (Android package names).
func validPackageName(s string) bool {
	if s == "" {
		return false
	}
	for k, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case k > 0 && (r >= '0' && r <= '9' || r == '_' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// guestFixDeferredReport judges guestFixCheckScript's output for owed fixes that were not mounted because the guest
// is in use (b from owedMountGate). pending: one of them applies to this image and is missing, or that cannot be
// read; one warning (guestFixDeferredMessage) then names those, and the setup marker stays. The others (mounted
// already, patched image, no such library, unexpected image) are reported as guestFixCheckReport says.
func guestFixDeferredReport(i int, out string, b mountBlocker, until, now time.Time, profiles []guestpatch.Profile) (pending bool,
	reports []report) {
	lines := libFixStatus(out, profiles)
	var owed []guestpatch.Profile
	unknown := false
	for _, p := range profiles {
		r := guestFixCheckReport(i, lines[p.GuestPath], out, p)
		if !r.warn {
			reports = append(reports, r)
			continue
		}
		owed = append(owed, p)
		if lines[p.GuestPath] != "image "+p.OrigSHA256 {
			unknown = true
		}
	}
	if len(owed) == 0 {
		return false, reports
	}
	detail := b.detail
	if unknown {
		u := "这次开机挂没挂也读不出: " + orText(tail(out, 200), "无输出")
		if detail == "" {
			detail = u
		} else {
			detail += "；" + u
		}
	}
	return true, append(reports, report{true, guestFixDeferredMessage(i, owed, b, detail, until, now)})
}

// guestFixDeferredMessage is the warning of owed fixes left out (b: why), with detail in brackets when not "". It
// names the marker's deadline (until) as local time and that only the next boot mounts them after that: a start after
// the deadline adopts the boot as an ordinary running instance and only checks the fixes.
func guestFixDeferredMessage(i int, profiles []guestpatch.Profile, b mountBlocker, detail string, until, now time.Time) string {
	var retry string
	switch {
	case until.IsZero():
		retry = b.then + "再 start 一次，或下次开机自动挂"
	case !now.Before(until):
		retry = "这次开机接着做设置的期限（" + clockText(until, now) + "）已过，等下次开机自动挂"
	default:
		retry = b.then + "在 " + clockText(until, now) + " 前再 start 一次，过了这个时间就等下次开机自动挂"
	}
	names := make([]string, len(profiles))
	for k, p := range profiles {
		names[k] = p.Name
	}
	msg := fmt.Sprintf("实例 %d 的%s还没挂：现在%s，挂上要重启 zygote；%s", i, strings.Join(names, "、"), b.what, retry)
	if detail != "" {
		msg += "（" + detail + "）"
	}
	return msg
}

// patchedGuestLibrary is the host cache <root>\guest-patches\<p.CacheName>, made when missing or bad: pull the
// guest's own library, check its hash, patch, check the result's hash (p.Patch), write it. "" when any step fails
// (logged, nothing mounted). Host I/O errors (disk full, a virus scanner holding the file) are warnings too.
//
// Two instances (separate modavd processes) may build it at once: each writes its own temp file and renames it
// over the cache; one that cannot rename takes the other's file when that has the right hash.
func (m *Manager) patchedGuestLibrary(ctx context.Context, i int, p guestpatch.Profile) string {
	dir := m.Root.GuestPatchDir()
	library := filepath.Join(dir, p.CacheName)
	ioFail := func(err error) string {
		m.Log.Warnf("实例 %d %s没挂: 本机读写补丁库失败（%s）: %v", i, p.Name, library, err)
		return ""
	}

	if data, err := os.ReadFile(library); err == nil {
		digest := guestpatch.SHA256Hex(data)
		if digest == p.FixSHA256 {
			m.Log.Infof("实例 %d %s: 用本机缓存 %s", i, p.Name, library)
			return library
		}
		m.Log.Warnf("本机缓存的%s补丁版哈希不对（%s），重新生成: %s", p.Lib, digest, library)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ioFail(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ioFail(err)
	}

	original := filepath.Join(dir, fmt.Sprintf("%s-%d.orig.so", strings.TrimSuffix(path.Base(p.GuestPath), ".so"), i))
	defer os.Remove(original)
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: m.Root.Ports().Serial(i), Timeout: 60 * time.Second, Reregister: true},
		"pull", p.GuestPath, original)
	if res.Code != 0 || !regularFile(original) {
		m.Log.Warnf("实例 %d %s没挂: 从客体拉 %s 失败: %s", i, p.Name, p.GuestPath, tail(res.Out, 200))
		return ""
	}
	data, err := os.ReadFile(original)
	if err != nil {
		return ioFail(err)
	}
	m.Log.Infof("实例 %d 从客体拉到%s原版（sha256 %s），本机生成补丁版", i, p.Lib, guestpatch.SHA256Hex(data))
	patched, err := p.Patch(data)
	if err != nil {
		m.Log.Warnf("实例 %d %s没挂: %v", i, p.Name, err)
		return ""
	}
	if err := writeFileReplace(library, patched, fmt.Sprintf("%d-%d", os.Getpid(), i)); err != nil {
		if b, rerr := os.ReadFile(library); rerr == nil && guestpatch.SHA256Hex(b) == p.FixSHA256 {
			// another start wrote the same file meanwhile (and may be pushing it right now)
			return library
		}
		return ioFail(err)
	}
	m.Log.Infof("实例 %d 已在本机生成%s补丁版（sha256 %s）: %s", i, p.Lib, p.FixSHA256, library)
	return library
}

// writeFileReplace writes path through <path>.<tag>.tmp and a rename over it, retried for up to 3 s while another
// process has the target open (an adb push reading it). The temp file is removed on failure.
func writeFileReplace(path string, data []byte, tag string) error {
	tmp := path + "." + tag + ".tmp"
	err := os.WriteFile(tmp, data, 0o644)
	if err == nil {
		err = root.ReplaceWithRetry(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// waitFrameworkBack waits after the zygote restart until SystemUI runs as a new process, then 8 s more (launcher).
func (m *Manager) waitFrameworkBack(ctx context.Context, g guestIO, uiBefore string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	if !sleepOrDone(ctx, 2*time.Second) {
		return false
	}
	for time.Now().Before(deadline) {
		res := g.shell("pidof com.android.systemui", 10*time.Second)
		// pidof exits 1 with no output while SystemUI is down; an adb error text is no new pid either
		if ui := strings.TrimSpace(res.Out); res.Code == 0 && ui != "" && ui != uiBefore {
			return sleepOrDone(ctx, 8*time.Second)
		}
		if !sleepOrDone(ctx, 2*time.Second) {
			return false
		}
	}
	return false
}
