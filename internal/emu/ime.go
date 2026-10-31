//go:build windows

package emu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/apk"
	"modavd/internal/root"
)

// The host input method org.modavd.ime: the guest half of the emulator's modavd_ime channel; what the user types
// with a Windows IME in the emulator window reaches the app through it. It has no input view and leaves key events
// alone, so `adb shell input`, maatouch and MaaFW input reach the app as before. Same constants as AUTO-MAS HOST_IME*
// and the launcher's embedded setup script.
const (
	hostImePackage = "org.modavd.ime"
	hostImeID      = "org.modavd.ime/.HostImeService"
	// the emulator has the modavd_ime channel from mas-35 on
	hostImeMinBuild = 35
	// The guest marker: this /data has decided whether the host IME becomes the default; the default is not touched
	// again afterwards. Shared with the launcher and AUTO-MAS: another name and they would take the default back
	// and forth.
	hostImeMarker = "/data/local/tmp/modavd-ime.autoselected"
	// what the emulator logs when it has the channel (launcher's check)
	hostImeChannelLine = "modavd_ime: host input method pipe service registered"
	hostImeWaitTimeout = 20 * time.Second
	uninstallTimeout   = 60 * time.Second
)

// hostImeStockDefaults: the image's own keyboards (Gboard, AOSP LatinIME) and unset. Only while the default is one of
// these (and nothing was decided yet) does it become the host IME.
var hostImeStockDefaults = map[string]bool{
	"": true,
	"com.google.android.inputmethod.latin/com.android.inputmethod.latin.LatinIME": true,
	"com.android.inputmethod.latin/.LatinIME":                                     true,
}

// hostImeReadDefault prints "def=<input method id>" ("null", never set, printed empty), or def_err when settings
// fails.
const hostImeReadDefault = `if d=$(settings get secure default_input_method 2>/dev/null); then` +
	` [ "$d" = null ] && d=; echo "def=$d"; else echo def_err; fi`

// hostImeProbeScript gets four things at once: the installed version (pm failing prints pm_err, which must not
// count as "not installed"), the default input method, the marker (mark=0|1), and the SHA-256 of the installed
// base.apk ("sha=<hex>", "sha=" when not installed or it cannot be hashed). The shell user reads base.apk (it is
// world-readable under /data/app; adb pull of it works without root), so no su.
const hostImeProbeScript = `if p=$(pm list packages --show-versioncode ` + hostImePackage + ` 2>/dev/null);` +
	` then echo "$p"; echo pm_ok; else echo pm_err; fi; ` +
	hostImeReadDefault + `; ` +
	`if [ -e ` + hostImeMarker + ` ]; then echo mark=1; else echo mark=0; fi; ` +
	`s=; for a in $(pm path ` + hostImePackage + ` 2>/dev/null); do case $a in package:*/base.apk)` +
	` s=$(sha256sum "${a#package:}" 2>/dev/null) ;; esac; done; echo "sha=${s%% *}"`

// hostImeWaitScript: after an install the input method service learns of the package from a broadcast, until then
// `ime enable` says "Unknown input method". Waits up to 5 s, prints ok or timeout.
const hostImeWaitScript = `i=0; while [ $i -lt 20 ]; do` +
	` ime list -a -s | grep -qx ` + hostImeID + ` && { echo ok; exit 0; };` +
	` sleep 0.25; i=$((i+1)); done; echo timeout`

// pm list packages filters by substring (org.modavd.ime.testapp would match too): only the exact line counts.
var hostImePackageLine = regexp.MustCompile(`^package:` + regexp.QuoteMeta(hostImePackage) + ` versionCode:(\d+)$`)

var installFailureCode = regexp.MustCompile(`Failure \[([A-Z_]+)`)

var hostImeSHALine = regexp.MustCompile(`^sha=([0-9a-f]{64})$`)

type hostImeProbe struct {
	Installed int64  // installed versionCode, 0 = not installed
	Default   string // default input method, "" = unset
	Marked    bool
	SHA       string // SHA-256 (lower-case hex) of the installed base.apk, "" = unknown
}

// parseHostImeProbe reads hostImeProbeScript's output; ok=false when pm, settings or the marker test could not be
// read (the step is skipped then, nothing is taken as "not installed" / "unset"). A missing or bad sha line only
// leaves SHA unknown.
func parseHostImeProbe(out string) (hostImeProbe, bool) {
	var p hostImeProbe
	pmOK, markSeen := false, false
	for _, line := range outputLines(out) {
		line = strings.TrimSpace(line)
		switch line {
		case "pm_ok":
			pmOK = true
		case "mark=0":
			markSeen = true
		case "mark=1":
			markSeen, p.Marked = true, true
		}
		if mm := hostImePackageLine.FindStringSubmatch(line); mm != nil {
			if v, err := strconv.ParseInt(mm[1], 10, 64); err == nil {
				p.Installed = v
			}
		}
		if mm := hostImeSHALine.FindStringSubmatch(line); mm != nil {
			p.SHA = mm[1]
		}
	}
	def, defOK := defaultImeFrom(out)
	p.Default = def
	return p, pmOK && defOK && markSeen
}

// defaultImeFrom is the default input method from hostImeReadDefault's output (the last "def=" line); ok=false
// when there is none (def_err, adb failed).
func defaultImeFrom(out string) (string, bool) {
	lines := outputLines(out)
	for k := len(lines) - 1; k >= 0; k-- {
		if line := strings.TrimSpace(lines[k]); strings.HasPrefix(line, "def=") {
			return line[len("def="):], true
		}
	}
	return "", false
}

type imeInstallAction int

const (
	imeUpToDate imeInstallAction = iota
	imeKeepNewer
	imeInstall
	imeReplace     // same versionCode, other bytes: installed again
	imeHashUnknown // same versionCode, a hash is unknown: taken as up to date
)

// hostImeInstallAction: install when missing or older than the APK; never downgrade. The same versionCode with
// other bytes (the installed base.apk's SHA-256 is not the APK's) is installed again: a dev build and the release
// build are both versionCode 1. When either hash is unknown ("") the same versionCode counts as up to date, so a
// guest that cannot hash does not get a reinstall on every boot.
func hostImeInstallAction(installed, apkVersion int64, installedSHA, apkSHA string) imeInstallAction {
	switch {
	case installed > apkVersion:
		return imeKeepNewer
	case installed < apkVersion:
		return imeInstall
	case installedSHA == "" || apkSHA == "":
		return imeHashUnknown
	case installedSHA != apkSHA:
		return imeReplace
	}
	return imeUpToDate
}

// fileSHA256 is the lower-case hex SHA-256 of a file, "" when it cannot be read.
func fileSHA256(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hostImePlan is what to do once the package is present.
type hostImePlan struct {
	SetDefault  bool // ime set (and touch the marker once the default reads back as ours)
	Enable      bool // ime enable
	TouchMarker bool // only record the decision
}

// planHostIme decides enable / default / marker (launcher rules, tested on 25 scenarios on 10-28):
//   - default: decided once per /data. Undecided (no marker) and the default is stock or unset -> make it ours; the
//     marker is written only after the default reads back as ours (a failure retries next boot). A default that is
//     another IME (or already ours) is left alone and only the marker is written. Exception: a signer-change
//     reinstall over our own default puts it back (the uninstall took it away; the marker was dropped first);
//   - enable: only with setting the default, or after a fresh install / reinstall: an upgrade (or a same-version
//     install of other bytes) under the same signer leaves a user's "disabled" alone.
func planHostIme(prior string, marked, fresh, reinstalled bool, installedBefore int64) hostImePlan {
	var p hostImePlan
	p.SetDefault = !marked && (hostImeStockDefaults[prior] || (reinstalled && prior == hostImeID))
	p.Enable = p.SetDefault || (fresh && (installedBefore == 0 || reinstalled))
	p.TouchMarker = !p.SetDefault && !marked
	return p
}

// commands are the guest commands of the plan (without the trailing sync and read-back).
func (p hostImePlan) commands() []string {
	var cmds []string
	if p.Enable {
		cmds = append(cmds, "ime enable "+hostImeID+" >/dev/null")
	}
	if p.SetDefault {
		cmds = append(cmds, "ime set "+hostImeID+" >/dev/null",
			`[ "$(settings get secure default_input_method)" = `+hostImeID+` ] && touch `+hostImeMarker)
	} else if p.TouchMarker {
		cmds = append(cmds, "touch "+hostImeMarker)
	}
	return cmds
}

// hostImeChannel: the emulator has the modavd_ime channel. AUTO-MAS checks the build (mas-35+); the launcher checks
// the emulator log of the boot it started. Either is enough (a dev SDK's build id may be anything). why says what
// is missing.
func (m *Manager) hostImeChannel(launched bool, logPath string) (ok bool, why string) {
	build, known := m.Root.ModBuild()
	if known && build >= hostImeMinBuild {
		return true, ""
	}
	if launched && fileContains(logPath, hostImeChannelLine) {
		return true, ""
	}
	if known {
		return false, fmt.Sprintf("内测包 mas-%d 低于 mas-%d", build, hostImeMinBuild)
	}
	return false, fmt.Sprintf("读不到内测包编号（要 mas-%d 起），模拟器日志里也没有 modavd_ime 通道", hostImeMinBuild)
}

// guestIO is how the IME step reaches the guest: `adb shell <command>` and other adb commands of one instance. The
// real one goes through Manager; tests use a fake guest.
type guestIO struct {
	shell func(command string, timeout time.Duration) adb.Result
	adb   func(timeout time.Duration, args ...string) adb.Result
}

func (m *Manager) guestIO(ctx context.Context, i int) guestIO {
	return guestIO{
		shell: func(command string, timeout time.Duration) adb.Result { return m.shell(ctx, i, command, timeout) },
		adb: func(timeout time.Duration, args ...string) adb.Result {
			return m.ADB.Run(ctx, adb.RunOptions{Serial: root.Serial(i), Timeout: timeout, Reregister: true}, args...)
		},
	}
}

// setupHostIme installs the host IME when missing, upgrades it when older, installs it again when the version is the
// same but the bytes differ, enables it and makes it the default by the rules (planHostIme), every boot (user 10-28:
// typing in the window should just work). Skipped quietly when the package has no APK or the emulator has no
// modavd_ime channel. Returns whether the default is the host IME at the end of the step (AUTO-MAS _setup_host_ime).
func (m *Manager) setupHostIme(ctx context.Context, i int, launched bool, logPath string, prog *progressor) bool {
	apkPath := m.Root.ImeAPK()
	if !regularFile(apkPath) {
		return false
	}
	if ok, why := m.hostImeChannel(launched, logPath); !ok {
		m.Log.Debugf("实例 %d 宿主输入法跳过: %s", i, why)
		return false
	}
	man, err := apk.ReadManifest(apkPath)
	if err != nil || man == nil || man.Package != hostImePackage || man.VersionCode == 0 {
		m.Log.Warnf("实例 %d 宿主输入法没装: 读不出 %s 的包名和版本号", i, apkPath)
		return false
	}
	prog.emit("ime", fmt.Sprintf("实例 %d 宿主输入法", i), progressIme)
	// hashed once per boot (about 20 KB); "" (unreadable) makes the same versionCode count as up to date
	return m.runHostIme(i, apkPath, man.VersionCode, fileSHA256(apkPath), m.guestIO(ctx, i))
}

// runHostIme is the guest part of setupHostIme; apkSHA is the APK's SHA-256 ("" = unknown). Skipped with a warning
// when pm / settings cannot be read (never taken as "not installed" / "unset"). A signer change (dev build vs
// release) is an uninstall + install: the IME keeps no data; when the uninstall fails the old one stays and the step
// goes on with it. Failures are warnings; the result is one log line.
func (m *Manager) runHostIme(i int, apkPath string, apkVersion int64, apkSHA string, g guestIO) bool {
	started := time.Now()
	out := g.shell(hostImeProbeScript, 0).Out
	probe, ok := parseHostImeProbe(out)
	if !ok {
		m.Log.Warnf("实例 %d 宿主输入法这次跳过，读不出客体现状: %s", i,
			orText(tail(joinNonEmptyLines(out, " | "), 300), "无输出"))
		return false
	}
	installed, marked, prior := probe.Installed, probe.Marked, probe.Default
	wasOurs := prior == hostImeID
	current, currentOK := prior, true
	failed := false
	present := installed > 0 // the package is in the guest after this step
	reinstalled := false     // uninstalled and installed again (signer change)
	fresh := false           // installed in this step (new, upgrade or reinstall)
	state := fmt.Sprintf("%d 已是最新", installed)

	switch action := hostImeInstallAction(installed, apkVersion, probe.SHA, apkSHA); action {
	case imeKeepNewer:
		state = fmt.Sprintf("保留已装的 %d（安装包是 %d）", installed, apkVersion)
	case imeHashUnknown:
		state = fmt.Sprintf("%d 已是最新（哈希读不出，没比内容）", installed)
	case imeInstall, imeReplace:
		res := g.adb(installLauncherTimeout, "install", "-r", apkPath)
		signerChanged := strings.Contains(res.Out, "INSTALL_FAILED_UPDATE_INCOMPATIBLE")
		if signerChanged {
			// the uninstall takes the default away from it: drop the marker first, so the next boot puts it back
			// even when the install below fails
			if wasOurs {
				g.shell("rm -f "+hostImeMarker, 0)
				marked = false
			}
			res = g.adb(uninstallTimeout, "uninstall", hostImePackage)
			// not uninstalled: the old one is still there, go on with it
			if res.Code == 0 && strings.Contains(res.Out, "Success") {
				res = g.adb(installLauncherTimeout, "install", apkPath)
				reinstalled = true
			}
		}
		if res.Code == 0 && strings.Contains(res.Out, "Success") {
			fresh, present = true, true
			switch {
			case reinstalled && action == imeReplace:
				state = fmt.Sprintf("%d 签名不同，卸了重装", apkVersion)
			case reinstalled:
				state = fmt.Sprintf("%d 换签名重装（原来是 %d）", apkVersion, installed)
			case action == imeReplace:
				// same signer: like an upgrade, a user's "disabled" stays (planHostIme: not reinstalled)
				state = fmt.Sprintf("%d 内容不同，重装", apkVersion)
			case installed > 0:
				state = fmt.Sprintf("%d 从 %d 升级", apkVersion, installed)
			default:
				state = fmt.Sprintf("%d 新装", apkVersion)
			}
		} else {
			failed = true
			if signerChanged && !reinstalled {
				state = fmt.Sprintf("签名不同，卸载旧版 %d 失败（%s）", installed, installFailure(res))
			} else {
				state = fmt.Sprintf("安装失败（%s）", installFailure(res))
			}
			if reinstalled {
				// the old signer's package is gone: not in the guest any more, and the system changed the default
				state += "，旧签名的已卸掉"
				present = false
				current, currentOK = defaultImeFrom(g.shell(hostImeReadDefault, 0).Out)
			}
		}
	}

	setDefault := false
	if present {
		if fresh {
			seen := lastNonEmptyLine(g.shell(hostImeWaitScript, hostImeWaitTimeout).Out)
			if seen != "ok" {
				failed = true
				state += fmt.Sprintf("，输入法服务没列出它（%s）", orText(seen, "无输出"))
			}
		}
		plan := planHostIme(prior, marked, fresh, reinstalled, installed)
		setDefault = plan.SetDefault
		if cmds := plan.commands(); len(cmds) > 0 {
			// sync: the marker is on disk before anything else happens, a hard kill does not lose it
			cmds = append(cmds, "sync", hostImeReadDefault)
			current, currentOK = defaultImeFrom(g.shell(strings.Join(cmds, "; "), 0).Out)
			if setDefault && (!currentOK || current != hostImeID) {
				failed = true
				state += "，没能设成默认（下次开机再试）"
			}
		}
	}

	shown := current
	switch {
	case !currentOK:
		failed = true
		shown = "读不出"
	case current == "":
		shown = "没设"
	}
	note := ""
	if !currentOK || current != prior {
		note = fmt.Sprintf("（原来是 %s）", orText(prior, "没设"))
	} else if present && !setDefault && current != hostImeID {
		note = "（保持用户选的）"
	}
	msg := fmt.Sprintf("实例 %d 宿主输入法: %s %s，默认 %s%s（%.1f 秒）", i, hostImePackage, state, shown, note,
		time.Since(started).Seconds())
	if failed {
		m.Log.Warnf("%s", msg)
	} else {
		m.Log.Infof("%s", msg)
	}
	return currentOK && current == hostImeID
}

// installFailure is the reason of a failed adb command: the Failure [CODE], else the last output line, else the exit
// code.
func installFailure(res adb.Result) string {
	if mm := installFailureCode.FindStringSubmatch(res.Out); mm != nil {
		return mm[1]
	}
	if lines := outputLines(strings.TrimSpace(res.Out)); len(lines) > 0 {
		return lines[len(lines)-1]
	}
	return fmt.Sprintf("退出码 %d", res.Code)
}

// recheckHostIme runs after the zygote restart of the Vulkan fix: the default was the host IME at the end of the IME
// step; when it is not any more, set it back (AUTO-MAS _recheck_host_ime; the launcher only reports it).
func (m *Manager) recheckHostIme(i int, g guestIO) {
	current, ok := defaultImeFrom(g.shell(hostImeReadDefault, 0).Out)
	if ok && current == hostImeID {
		return
	}
	if !ok {
		m.Log.Warnf("实例 %d 宿主输入法: 重启 zygote 后读不出默认输入法", i)
		return
	}
	restored, rok := defaultImeFrom(g.shell("ime set "+hostImeID+" >/dev/null; "+hostImeReadDefault, 0).Out)
	if rok && restored == hostImeID {
		m.Log.Infof("实例 %d 宿主输入法: 重启 zygote 后默认变成了 %s，已设回", i, orText(current, "没设"))
		return
	}
	now := "读不出"
	if rok {
		now = orText(restored, "没设")
	}
	m.Log.Warnf("实例 %d 宿主输入法: 重启 zygote 后默认变成了 %s，没能设回（现在 %s）", i, orText(current, "没设"), now)
}
