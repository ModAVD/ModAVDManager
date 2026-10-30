//go:build windows

package emu

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/guest"
	"modavd/internal/root"
	"modavd/internal/vkfix"
	"modavd/internal/win"
)

const (
	vulkanFixScriptTimeout = 60 * time.Second
	frameworkBackTimeout   = 120 * time.Second
)

// ensureVulkanFix bind-mounts the patched guest Vulkan encoder made on this host over /vendor/lib64/libvulkan_enc.so
// and restarts zygote (AUTO-MAS _ensure_vulkan_fix; launcher -VulkanFix), on every instance for every game (user
// 10-16). The patch (internal/vkfix) fixes Star Rail's descriptor templates (khrfix) and Endfield's unlocked
// vkBindImageMemory lookups (bindlock); games that use neither path are unaffected.
//
//   - vulkanfix.sh first checks whether this boot already has it mounted (nothing to do then), then the image
//     library's hash (anything but 898c0bbc... is left alone); the patched copy is cached in the guest's
//     /data/local/tmp, so the next boot mounts it without an upload.
//   - Without a guest cache the host copy in <root>\guest-patches is pushed; without that, the guest's own library
//     is pulled and patched here (patchedVulkanLibrary). The patched file is never shipped.
//   - An instance that was already running (setupAdopted) is only checked: the zygote restart would end the game. A
//     missing fix is a warning then (it reaches StartResult.Warnings), since that whole boot runs without it.
//   - A boot whose setup an earlier start left unfinished (setupOwed) may have been put to use since: AUTO-MAS
//     launched a game on it, an MCP client went on with it (install_apk among others), a user plays in the window.
//     Each vulkanfix.sh run that may mount it first passes owedMountGate (no install holds the guest lock, and
//     vulkanFixMountAllowed finds the guest unused: home screen, SystemUI or nothing in front); otherwise it is not
//     mounted and, when the fix is still missing, pending=true: the setup marker stays and the result says
//     SetupPending, so a start after the app is closed or the install is done (before until, the marker's deadline)
//     mounts it, or the next boot does.
//
// Failures are only logged. Returns whether zygote was restarted, and whether the fix is still owed.
func (m *Manager) ensureVulkanFix(ctx context.Context, i int, mode bootSetup, until time.Time) (restarted, pending bool) {
	if mode == setupAdopted {
		res := m.shell(ctx, i, vulkanFixCheckScript, 0)
		if warn, msg := vulkanFixCheckReport(i, res.Out); warn {
			m.Log.Warnf("%s", msg)
		} else {
			m.Log.Infof("%s", msg)
		}
		return false, false
	}
	remote, ok := m.pushGuestScript(ctx, i, guest.VulkanFix)
	if !ok {
		m.Log.Warnf("实例 %d 客体 Vulkan 驱动补丁没挂: 推送客体脚本失败", i)
		return false, false
	}
	g := m.guestIO(ctx, i)
	return m.mountVulkanFix(ctx, i, vulkanFixIO{
		guestIO: g,
		remote:  remote,
		library: func() string { return m.patchedVulkanLibrary(ctx, i) },
		waitBack: func(uiBefore string) bool {
			return m.waitFrameworkBack(ctx, g, uiBefore, frameworkBackTimeout)
		},
	}, mode == setupOwed, until)
}

// vulkanFixIO is what mountVulkanFix works through: the instance's adb in Manager, a fake guest in tests.
type vulkanFixIO struct {
	guestIO
	remote   string                     // vulkanfix.sh in the guest (pushed already)
	library  func() string              // the host's patched library (patchedVulkanLibrary); "" when it cannot be had
	waitBack func(uiBefore string) bool // after the zygote restart (waitFrameworkBack)
}

// mountBlocker is why an owed Vulkan fix is not mounted now, for the messages: what is in the way ("有应用在前台"),
// what to do before the next start ("关掉应用后") and details ("" none).
type mountBlocker struct {
	what, then, detail string
}

// foregroundBlocker: vulkanFixMountAllowed refused (front as it returned it, "" when unreadable).
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

// owedMountGate is passed right before each vulkanfix.sh run that may mount an owed fix (and so restart zygote, which
// ends every app and breaks an install that is running):
//   - the guest lock (guestLockPath), which InstallAPK holds while it installs: taken with TryLock, never waited
//     for (an install of a 3 GB game takes minutes, a start should not);
//   - then the foreground rule (vulkanFixMountAllowed), read again at every call: the patched library may have taken
//     tens of seconds to make and upload since the last one, and a game may have been launched meanwhile.
//
// ok: both allow it, and release frees the lock right after that run returns: the restart has been triggered by
// then, and an install started after it only meets a framework that is coming back (what InstallAPK's own boot check
// and adb's errors report). Otherwise b says what is in the way and nothing is held.
func (m *Manager) owedMountGate(i int, g guestIO) (release func(), b mountBlocker, ok bool) {
	l, free, err := win.TryLock(m.guestLockPath(i))
	if err != nil || !free {
		return nil, installBlocker(err), false
	}
	probe := g.shell(foregroundProbeScript, 0)
	allowed, front := vulkanFixMountAllowed(probe.Out)
	if !allowed {
		l.Unlock()
		m.Log.Debugf("实例 %d 前台检查: %s", i, orText(tail(joinNonEmptyLines(probe.Out, " | "), 300), "无输出"))
		return nil, foregroundBlocker(front), false
	}
	m.Log.Infof("实例 %d 这次开机的客体 Vulkan 驱动补丁还没做，前台是 %s、没有应用在用，也没有安装在进行，接着挂（会重启 zygote）",
		i, front)
	return l.Unlock, mountBlocker{}, true
}

// mountVulkanFix is the mounting part of ensureVulkanFix (a launched boot, or an owed one, owed=true: every run that
// may mount passes owedMountGate first; until is the marker's deadline for the messages). Returns whether zygote was
// restarted and whether an owed fix is still owed (deferred while the guest is in use).
func (m *Manager) mountVulkanFix(ctx context.Context, i int, fx vulkanFixIO, owed bool, until time.Time) (restarted,
	pending bool) {
	runFix := func() string {
		res := fx.shell("su 0 sh "+fx.remote+" "+vkfix.OrigSHA256+" "+vkfix.FixSHA256, vulkanFixScriptTimeout)
		return lastNonEmptyLine(res.Out)
	}
	// gatedFix is one vulkanfix.sh run, for an owed fix only once owedMountGate allows it, with the guest lock held
	// over the run. ok=false: deferred (logged here) and nothing ran.
	gatedFix := func(first bool) (status string, ok, pending bool) {
		var release func()
		var b mountBlocker
		allowed := false
		if owed {
			release, b, allowed = m.owedMountGate(i, fx.guestIO)
		} else {
			// A launched boot has no app of ours in front yet, but an install_apk of another modavd process may already
			// run (sys.boot_completed is 1 by now): the guest lock alone decides, the same way.
			l, free, err := win.TryLock(m.guestLockPath(i))
			if err == nil && free {
				release, allowed = l.Unlock, true
			} else {
				b = installBlocker(err)
			}
		}
		if allowed {
			defer release()
			return runFix(), true, false
		}
		if !first {
			// the first run said NEED_UPLOAD: the image wants the fix and it is not mounted. The upload stays in the
			// guest, so the start that mounts it does not upload again (vulkanfix.sh takes it into its cache).
			m.Log.Warnf("%s", vulkanFixDeferredMessage(i, b, b.detail, until, time.Now()))
			return "", false, true
		}
		res := fx.shell(vulkanFixCheckScript, 0)
		pending, msg := vulkanFixDeferredReport(i, res.Out, b, until, time.Now())
		if pending {
			m.Log.Warnf("%s", msg)
		} else {
			m.Log.Infof("%s", msg)
		}
		return "", false, pending
	}

	uiBefore := strings.TrimSpace(fx.shell("pidof com.android.systemui", 0).Out)
	status, ok, pending := gatedFix(true)
	if !ok {
		return false, pending
	}
	m.Log.Infof("实例 %d 客体 Vulkan 驱动补丁 第 1 步（客体检查）: %s", i, orText(status, "无输出"))
	if kind, _ := vulkanFixStatus(status); kind == "NEED_UPLOAD" {
		library := fx.library()
		if library == "" {
			return false, false
		}
		res := fx.adb(60*time.Second, "push", library, vkfix.UploadPath)
		if res.Code != 0 {
			m.Log.Warnf("实例 %d 客体 Vulkan 驱动补丁没挂: 推送补丁库失败: %s", i, tail(res.Out, 200))
			return false, false
		}
		m.Log.Infof("实例 %d 客体 Vulkan 驱动补丁 第 2 步: 已推送 %s（sha256 %s）到 %s", i, library, vkfix.FixSHA256,
			vkfix.UploadPath)
		if status, ok, pending = gatedFix(false); !ok {
			return false, pending
		}
		m.Log.Infof("实例 %d 客体 Vulkan 驱动补丁 第 3 步（挂载）: %s", i, orText(status, "无输出"))
	}

	kind, sha := vulkanFixStatus(status)
	switch kind {
	case "ALREADY":
		if sha == vkfix.FixSHA256 {
			m.Log.Infof("实例 %d 客体 Vulkan 驱动补丁本次开机已挂上（sha256 %s）", i, sha)
		} else {
			m.Log.Warnf("实例 %d 客体驱动库上已经挂着别的文件（sha256 %s），不是这次的补丁，没有动它", i, sha)
		}
		return false, false
	case "VENDOR_PATCHED":
		m.Log.Infof("实例 %d 镜像自带的客体驱动库已是补丁版，不用挂", i)
		return false, false
	case "MOUNTED":
	default:
		// SKIPPED image: not the expected image; SKIPPED patched: the guest cache was bad, removed, re-uploaded
		// next time; FAILED: the mount did not take
		m.Log.Warnf("实例 %d 客体 Vulkan 驱动补丁没挂: %s", i, orText(status, "客体脚本无输出"))
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
	m.Log.Infof("实例 %d 客体 Vulkan 驱动补丁已挂上（sha256 %s），zygote 重启后系统界面%s（%.0f 秒）", i, sha, how,
		time.Since(started).Seconds())
	return true, false
}

// vulkanFixCheckScript is the check of an adopted instance (no root, nothing changed): "mounted" when something is
// mounted over the library this boot (the test vulkanfix.sh starts with), else "image <sha256>" of the image's own.
const vulkanFixCheckScript = `if grep -q ' ` + vkfix.GuestPath + ` ' /proc/self/mountinfo; then echo mounted;` +
	` else echo "image $(sha256sum ` + vkfix.GuestPath + ` | cut -d' ' -f1)"; fi`

// vulkanFixCheckReport judges vulkanFixCheckScript's output. A warning only when the fix applies to this image and is
// missing: the instance runs a whole boot without it then (Star Rail's descriptor templates, Endfield's
// vkBindImageMemory), and the warning says how to get it. Otherwise this boot's state is only logged: an image whose
// library already is the patched one, or an unexpected image (the start that booted it reported that already).
func vulkanFixCheckReport(i int, out string) (warn bool, msg string) {
	line := lastNonEmptyLine(out)
	switch sha, isImage := strings.CutPrefix(line, "image "); {
	case line == "mounted":
		return false, fmt.Sprintf("实例 %d 客体 Vulkan 驱动补丁: 本次开机已挂上", i)
	case isImage && sha == vkfix.FixSHA256:
		return false, fmt.Sprintf("实例 %d 镜像自带的客体驱动库已是补丁版，不用挂", i)
	case isImage && sha == vkfix.OrigSHA256:
		return true, fmt.Sprintf("实例 %d 客体 Vulkan 驱动补丁这次开机没挂上：实例已在运行，挂它要重启 zygote（会结束正在运行的应用），"+
			"这次不挂；要挂就先关掉这个实例再启动", i)
	case isImage && len(sha) == 64:
		return false, fmt.Sprintf("实例 %d 客体驱动库不是预期的镜像版本（sha256 %s），不挂 Vulkan 驱动补丁", i, sha)
	}
	return true, fmt.Sprintf("实例 %d 读不出客体 Vulkan 驱动补丁这次开机挂没挂: %s", i, orText(tail(out, 200), "无输出"))
}

// foregroundProbeScript prints, without root and without changing anything, the activity in front ("top: " and the
// first topResumedActivity / mResumedActivity line of dumpsys) and the default HOME activity ("home: " and the last
// line of resolve-activity --brief). Tagged lines: a dumpsys cut short by grep -m1 may add a "Broken pipe" line, and
// adb errors come without the tags.
const foregroundProbeScript = `echo "top: $(dumpsys activity activities | grep -m1 -E 'topResumedActivity|mResumedActivity')"; ` +
	`echo "home: $(cmd package resolve-activity --brief -a android.intent.action.MAIN -c android.intent.category.HOME | tail -n1)"`

// systemUIPackage in front (lock screen, notification shade) is no app in use.
const systemUIPackage = "com.android.systemui"

// vulkanFixMountAllowed decides from foregroundProbeScript's output whether an owed Vulkan fix may be mounted now,
// i.e. whether restarting zygote ends nothing anybody uses: the resumed activity belongs to the default HOME app, to
// SystemUI, or there is none (topResumedActivity=null). A user app in front, or output that cannot be read, defers
// it: a needless deferral costs one more start, a wrong guess ends a running game.
//
// A HOME that resolves to the "android" package is the resolver (no default home set); the chooser activities of that
// package can also sit over a game, so it does not count as home. front names what is in front for the messages
// ("无" for nothing, "" when unreadable).
func vulkanFixMountAllowed(out string) (allowed bool, front string) {
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

// vulkanFixDeferredReport judges vulkanFixCheckScript's output for an owed fix that was not mounted because the
// guest is in use (b from owedMountGate). pending: the fix applies to this image and is missing, or that cannot be
// read; the setup marker then stays and msg is vulkanFixDeferredMessage's. Otherwise nothing is owed (mounted
// already, patched image, unexpected image) and msg is vulkanFixCheckReport's.
func vulkanFixDeferredReport(i int, out string, b mountBlocker, until, now time.Time) (pending bool, msg string) {
	warn, checkMsg := vulkanFixCheckReport(i, out)
	if !warn {
		return false, checkMsg
	}
	detail := b.detail
	if sha, isImage := strings.CutPrefix(lastNonEmptyLine(out), "image "); !isImage || sha != vkfix.OrigSHA256 {
		unknown := "这次开机挂没挂也读不出: " + orText(tail(out, 200), "无输出")
		if detail == "" {
			detail = unknown
		} else {
			detail += "；" + unknown
		}
	}
	return true, vulkanFixDeferredMessage(i, b, detail, until, now)
}

// vulkanFixDeferredMessage is the warning of an owed fix left out (b: why), with detail in brackets when not "". It
// names the marker's deadline (until) as local time and that only the next boot mounts it after that: a start after
// the deadline adopts the boot as an ordinary running instance and only checks the fix.
func vulkanFixDeferredMessage(i int, b mountBlocker, detail string, until, now time.Time) string {
	var retry string
	switch {
	case until.IsZero():
		retry = b.then + "再 start 一次，或下次开机自动挂"
	case !now.Before(until):
		retry = "这次开机接着做设置的期限（" + clockText(until, now) + "）已过，等下次开机自动挂"
	default:
		retry = b.then + "在 " + clockText(until, now) + " 前再 start 一次，过了这个时间就等下次开机自动挂"
	}
	msg := fmt.Sprintf("实例 %d 的客体 Vulkan 驱动补丁还没挂：现在%s，挂上要重启 zygote；%s", i, b.what, retry)
	if detail != "" {
		msg += "（" + detail + "）"
	}
	return msg
}

// vulkanFixStatus splits vulkanfix.sh's status line: the first word, and what follows the first space (the sha256
// for MOUNTED / ALREADY). A NEED_UPLOAD... line counts as NEED_UPLOAD etc. (AUTO-MAS startswith); "" for no output.
func vulkanFixStatus(status string) (kind, rest string) {
	for _, k := range []string{"NEED_UPLOAD", "ALREADY", "VENDOR_PATCHED", "MOUNTED", "SKIPPED", "FAILED"} {
		if strings.HasPrefix(status, k) {
			_, rest, _ = strings.Cut(status, " ")
			return k, rest
		}
	}
	return "", ""
}

// patchedVulkanLibrary is the host cache <root>\guest-patches\libvulkan_enc.khrfix-bindlock2.so, made when missing or
// bad: pull the guest's own library, check its hash, patch, check the result's hash (vkfix.Patch), write it. ""
// when any step fails (logged, nothing mounted). Host I/O errors (disk full, a virus scanner holding the file) are
// warnings too.
//
// Two instances (separate modavd processes) may build it at once: each writes its own temp file and renames it
// over the cache; one that cannot rename takes the other's file when that has the right hash.
func (m *Manager) patchedVulkanLibrary(ctx context.Context, i int) string {
	dir := m.Root.GuestPatchDir()
	library := filepath.Join(dir, vkfix.CacheName)
	ioFail := func(err error) string {
		m.Log.Warnf("实例 %d 客体 Vulkan 驱动补丁没挂: 本机读写补丁库失败（%s）: %v", i, library, err)
		return ""
	}

	if data, err := os.ReadFile(library); err == nil {
		digest := vkfix.SHA256Hex(data)
		if digest == vkfix.FixSHA256 {
			m.Log.Infof("实例 %d 客体 Vulkan 驱动补丁: 用本机缓存 %s", i, library)
			return library
		}
		m.Log.Warnf("本机缓存的客体驱动补丁库哈希不对（%s），重新生成: %s", digest, library)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ioFail(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ioFail(err)
	}

	original := filepath.Join(dir, fmt.Sprintf("libvulkan_enc-%d.orig.so", i))
	defer os.Remove(original)
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: root.Serial(i), Timeout: 60 * time.Second, Reregister: true},
		"pull", vkfix.GuestPath, original)
	if res.Code != 0 || !regularFile(original) {
		m.Log.Warnf("实例 %d 客体 Vulkan 驱动补丁没挂: 从客体拉 %s 失败: %s", i, vkfix.GuestPath, tail(res.Out, 200))
		return ""
	}
	data, err := os.ReadFile(original)
	if err != nil {
		return ioFail(err)
	}
	m.Log.Infof("实例 %d 从客体拉到驱动原库（sha256 %s），本机生成补丁库", i, vkfix.SHA256Hex(data))
	patched, err := vkfix.Patch(data)
	if err != nil {
		m.Log.Warnf("实例 %d 客体 Vulkan 驱动补丁没挂: %v", i, err)
		return ""
	}
	if err := writeFileReplace(library, patched, fmt.Sprintf("%d-%d", os.Getpid(), i)); err != nil {
		if b, rerr := os.ReadFile(library); rerr == nil && vkfix.SHA256Hex(b) == vkfix.FixSHA256 {
			// another start wrote the same file meanwhile (and may be pushing it right now)
			return library
		}
		return ioFail(err)
	}
	m.Log.Infof("实例 %d 已在本机生成客体驱动补丁库（sha256 %s）: %s", i, vkfix.FixSHA256, library)
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
