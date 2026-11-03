package emu

// The arm64 execution bridge by default (10-30). A new instance gets the bridge's boot marker at create, so its
// first boot is already writable (the emulator boots writable while the marker is there, qemu fixq36 0001). A start
// that finds the marker while the guest's native bridge is still the stock translator runs the package's own
// tools\modavd-bridge.ps1 install once its setup is done. The script stays the one implementation of the install:
// remount, push the three payload files, edit vendor build.prop, set the GL transport to asg, verify.
//
// The package carries the payload (sdk\emulator\resources\modavd), so an update of the package brings new files the
// instances that have the bridge do not have yet (a fix in the companion library, say). A start that launched such an
// instance and finds the guest's files differing from the payload runs the same install, which replaces only the files
// that differ, and boots cold once so the guest loads them (bridgeOutdated, updateBridge).
//
// It runs without -Reboot: where the script would reboot the guest (the first remount only turns verity off; the new
// build.prop line is read at boot) modavd shuts the instance down and boots it cold instead. A guest reboot under
// WHPX can leave one vCPU deaf to IPIs, and the guest then spins in smp_call_function forever (10-30 bridge-default
// regression: soft lockup in toggle_allocation_gate right after the install's `adb reboot`; a cold boot does not
// reset vCPUs in place.
//
// Instances AUTO-MAS creates and boots itself never come through here; `modavd bridge <i> install` does them (and
// any older instance).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

const (
	// bridgeMarkerName is the boot marker modavd-bridge.ps1 places (Set-WritableMarker) and removes on uninstall.
	bridgeMarkerName = "modavd-writable-system"
	// bridgeProp is ro.dalvik.vm.native.bridge once the bridge is installed.
	bridgeProp = "libmodavd_bridge.so"
	// bridgeScriptName is the script in <root>\tools.
	bridgeScriptName = "modavd-bridge.ps1"
)

// bridgeMarkerText is what modavd-bridge.ps1 writes into the marker (UTF-8 with BOM, CRLF): the script appends to it
// later (gltransport=asg), so both must agree on the file.
const bridgeMarkerText = "ModAVD arm64 执行桥：这台实例装了桥（或正要装）。\r\n" +
	"0.36.0 起的模拟器看到这个文件，就按可写系统镜像开机，桥装在实例自己的 system / vendor 覆盖层里，只有这样才挂得上。\r\n" +
	"不要手动删；要卸桥请跑 tools\\modavd-bridge.ps1 uninstall，它会连同这个文件一起删掉。\r\n"

// bridgeScriptTimeout bounds one script run (no reboots in it: remount, push, edit, verify).
var bridgeScriptTimeout = 5 * time.Minute

// bridgeInstallRounds: script runs and cold boots finishBridge tries. A new instance needs at most three: the first
// remount only turns verity off, the second installs, the third boot reads the new property.
const bridgeInstallRounds = 3

// bridgeQemuGoneWait is how long Bridge waits for the qemu to exit after its stop before starting again.
var bridgeQemuGoneWait = 30 * time.Second

// bridgeMetaKey is instances.<i>.bridge in mas-avd.json: false when the user took the bridge off (`modavd bridge <i>
// uninstall`) or created the instance without it, so no start puts it back (before a downgrade, say). Absent or
// anything else: every start that launches the instance makes sure it has the bridge (10-30: users are not meant to
// install it themselves).
const bridgeMetaKey = "bridge"

// bridgeWanted: instance i, whose config.ini the caller already read (cfg, avd.Instance.Config), gets the bridge on
// its starts. Never for an arm64 guest (bridgeGuestSupported); otherwise unless instances.<i>.bridge is false.
func (m *Manager) bridgeWanted(i int, cfg *avd.Ini) bool {
	if !bridgeGuestSupported(cfg) {
		return false
	}
	meta := m.Root.InstanceMeta(i)
	if meta == nil {
		return true
	}
	v := meta.Get(bridgeMetaKey)
	if v == nil {
		return true
	}
	b, isBool := v.BoolVal()
	return !isBool || b
}

func (m *Manager) setBridgeWanted(i int, on bool) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(bridgeMetaKey, jsonord.NewBool(on))
		return nil
	})
}

func bridgeMarker(r *root.Root, i int) string { return filepath.Join(avd.Dir(r, i), bridgeMarkerName) }

// bridgeGuestSupported limits the Windows PowerShell bridge to x86_64 guests: false when the instance's config.ini
// (cfg, as avd parsed it) says abi.type or hw.cpu.arch is arm64 / arm64-v8a / aarch64, any case. Existing and
// externally created arm64 AVDs are left alone. Keys that are missing, an unreadable config (avd.List gives it an
// empty Ini) and nil all keep the historical x86_64 behaviour.
//
// It only looks at values already read: the read behind them (avd.ReadIni) shares delete, and status / info / list
// call this on every poll, so a file opened here would stand in the way of AUTO-MAS deleting the instance (see
// root.OpenShared).
func bridgeGuestSupported(cfg *avd.Ini) bool {
	for _, key := range []string{"abi.type", "hw.cpu.arch"} {
		switch strings.ToLower(cfg.Value(key)) {
		case "arm64", "arm64-v8a", "aarch64":
			return false
		}
	}
	return true
}

// writeBridgeMarker places the boot marker unless it is there already.
func writeBridgeMarker(r *root.Root, i int) error {
	p := bridgeMarker(r, i)
	if regularFile(p) {
		return nil
	}
	data := append([]byte{0xEF, 0xBB, 0xBF}, bridgeMarkerText...)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return errs.Wrap(errs.Internal, err, "写不了执行桥的开机标记 "+p+": "+err.Error())
	}
	return nil
}

// guestBridge is the booted guest's ro.dalvik.vm.native.bridge ("" when adb did not answer).
func (m *Manager) guestBridge(ctx context.Context, i int) string {
	return m.guestBridgeWithin(ctx, i, 0)
}

// guestBridgeWithin is guestBridge with an adb timeout (0: m.shell's default).
func (m *Manager) guestBridgeWithin(ctx context.Context, i int, timeout time.Duration) string {
	res := m.shell(ctx, i, "getprop ro.dalvik.vm.native.bridge", timeout)
	if res.Code != 0 {
		return ""
	}
	return strings.TrimSpace(res.Out)
}

// bridgePending: the marker is there and the booted guest still runs the stock translator.
func (m *Manager) bridgePending(ctx context.Context, i int) bool {
	if !regularFile(bridgeMarker(m.Root, i)) {
		return false
	}
	b := m.guestBridge(ctx, i)
	return b != "" && b != bridgeProp
}

// bridgePayload are the payload files (as named in the package's payload directory) and where the script installs
// them in the guest (modavd-bridge.ps1 $script:Items).
var bridgePayload = []struct{ name, target string }{
	{"libmodavd_bridge.so", "/system/lib64/libmodavd_bridge.so"},
	{"libmodavd_gate.so", "/system/lib64/libmodavd_gate.so"},
	{"modavd_bridge.rc", "/system/etc/init/modavd_bridge.rc"},
}

// bridgeGuestHashTimeout bounds the guest's sha256sum of the three installed files (a few MB).
const bridgeGuestHashTimeout = 15 * time.Second

// bridgePayloadDir is the payload of the SDK the instance runs on (the script's default is <root>\sdk, the same in a
// package root).
func (m *Manager) bridgePayloadDir() string {
	return filepath.Join(m.Root.RuntimeSDK(), "emulator", "resources", "modavd")
}

// payloadHashes is the SHA-256 (lowercase hex) of each payload file in dir, by its guest target; ok=false when one
// is missing or unreadable (the script then refuses to install anyway).
func payloadHashes(dir string) (map[string]string, bool) {
	sums := map[string]string{}
	for _, f := range bridgePayload {
		b, err := os.ReadFile(filepath.Join(dir, f.name))
		if err != nil {
			return nil, false
		}
		sum := sha256.Sum256(b)
		sums[f.target] = hex.EncodeToString(sum[:])
	}
	return sums, true
}

// bridgePayloadTargets are the guest paths of the payload files.
func bridgePayloadTargets() []string {
	targets := make([]string, len(bridgePayload))
	for k, f := range bridgePayload {
		targets[k] = f.target
	}
	return targets
}

// bridgeGuestHashScript prints, for each of targets, "<sha256> <target>", "- <target>" when it is not there and
// "? <target>" when it is there but cannot be hashed (POSIX sh: the guest's mksh, sh in the tests).
func bridgeGuestHashScript(targets []string) string {
	return "for f in " + strings.Join(targets, " ") + `; do if [ ! -e "$f" ]; then echo "- $f"; ` +
		`elif s=$(sha256sum "$f" 2>/dev/null); then echo "${s%% *} $f"; else echo "? $f"; fi; done`
}

// payloadStale compares the guest's answer to bridgeGuestHashScript (out) with the package's payload (want, by
// target): stale when a file is not there in the guest or differs. known=false when out does not give a hash or "-"
// for every file (adb failed or cut it short, a file could not be hashed): nothing is decided then.
func payloadStale(want map[string]string, out string) (stale, known bool) {
	got := map[string]string{}
	for _, line := range outputLines(out) {
		if f := strings.Fields(line); len(f) == 2 {
			got[f[1]] = strings.ToLower(f[0])
		}
	}
	for target, sum := range want {
		g := got[target]
		if g != "-" && !isSHA256Hex(g) {
			return false, false
		}
		if g != sum {
			stale = true
		}
	}
	return stale, true
}

// isSHA256Hex: s is 64 lowercase hex digits.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// bridgeOutdated: the booted guest runs the bridge, and its files are not those of the package's payload (missing or
// different). false when anything is unknown: the payload incomplete, adb not answering.
func (m *Manager) bridgeOutdated(ctx context.Context, i int) bool {
	if !regularFile(bridgeMarker(m.Root, i)) || m.guestBridge(ctx, i) != bridgeProp {
		return false
	}
	want, ok := payloadHashes(m.bridgePayloadDir())
	if !ok {
		m.Log.Debugf("实例 %d：执行桥载荷 %s 不全，不比对", i, m.bridgePayloadDir())
		return false
	}
	res := m.shell(ctx, i, bridgeGuestHashScript(bridgePayloadTargets()), bridgeGuestHashTimeout)
	if res.Code != 0 {
		return false
	}
	stale, known := payloadStale(want, res.Out)
	return known && stale
}

// bridgeRun is one run of modavd-bridge.ps1.
type bridgeRun struct {
	Code    int    // the script's exit code (-1: it did not run to the end)
	Last    string // its last non-empty output line
	LogFile string // its whole output
}

// runBridgeScript runs <root>\tools\modavd-bridge.ps1 <action> on instance i with Windows PowerShell 5.1 (what the
// release notes tell users to run), against the root's private adb server and the root's adb. Its output goes to
// logs\modavd-bridge-<i>-<time>.log; the console encoding is set to UTF-8 first, otherwise a process without a console
// of its own gets the Chinese as question marks.
func (m *Manager) runBridgeScript(ctx context.Context, i int, action string, reboot bool) (bridgeRun, error) {
	if err := requireWindows(); err != nil {
		return bridgeRun{Code: -1}, err
	}
	script := filepath.Join(m.Root.Dir, "tools", bridgeScriptName)
	if !regularFile(script) {
		return bridgeRun{Code: -1}, errs.Newf(errs.NotFound, "找不到 %s：包不完整", script).
			WithAdvice("用完整的新包覆盖解压一次")
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	call := []string{"&", quote(script), action, "-Serial", quote(root.Serial(i)),
		"-AdbPort", strconv.Itoa(m.Root.AdbServerPort()), "-Adb", quote(m.Root.AdbExe()),
		"-Payload", quote(m.bridgePayloadDir())}
	if reboot {
		call = append(call, "-Reboot")
	}
	command := "[Console]::OutputEncoding = [Text.Encoding]::UTF8; " + strings.Join(call, " ") + "; exit $LASTEXITCODE"
	psRoot := os.Getenv("SystemRoot")
	if psRoot == "" || !filepath.IsAbs(psRoot) {
		return bridgeRun{Code: -1}, errs.New(errs.Internal, "找不到 Windows 系统目录，不能运行执行桥脚本")
	}
	ps := filepath.Join(psRoot, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")

	cctx, cancel := context.WithTimeout(ctx, bridgeScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	// the script's adb calls: same environment as ours (an adb that starts a server must not scan local ports either)
	cmd.Env = m.ADB.Env()
	win.HideConsoleCmd(cmd)
	// a killed powershell may leave an adb child holding the output pipe: stop waiting for it (as adbcmd.go)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()

	run := bridgeRun{Code: -1}
	if cmd.ProcessState != nil {
		run.Code = cmd.ProcessState.ExitCode()
	}
	for _, line := range outputLines(string(out)) {
		if line = strings.TrimSpace(line); line != "" {
			run.Last = line
		}
	}
	if dir := m.Root.LogsDir(); os.MkdirAll(dir, 0o755) == nil {
		run.LogFile = filepath.Join(dir, fmt.Sprintf("modavd-bridge-%d-%s.log", i, time.Now().Format("20060102-150405")))
		if werr := os.WriteFile(run.LogFile, out, 0o644); werr != nil {
			run.LogFile = ""
		}
	}
	if ctx.Err() != nil {
		return run, errs.Wrap(errs.Canceled, ctx.Err(), "已取消（执行桥脚本没跑完）")
	}
	if cctx.Err() == context.DeadlineExceeded {
		return run, errs.Newf(errs.Timeout, "执行桥脚本 %.0f 分钟没跑完", bridgeScriptTimeout.Minutes())
	}
	if err != nil && cmd.ProcessState == nil {
		return run, errs.Wrap(errs.Internal, err, "运行不了 Windows PowerShell: "+err.Error())
	}
	return run, nil
}

// finishBridge is the last step of a start that launched the instance, when the marker is there and the bridge is not:
// the script's install, and a cold boot (same window and timeout, without this step) whenever the bridge has not taken
// effect yet. A script that fails is only a warning: the instance keeps running, arm64 apps on the stock translator,
// and the next start tries again. A cold boot that fails is the start's error (the instance is off or half up then).
// Returns the result of the last start. Start calls it only when bridgeWanted, which already leaves arm64 guests out.
func (m *Manager) finishBridge(ctx context.Context, i int, opt StartOptions, res *StartResult) (*StartResult, error) {
	if err := requireWindows(); err != nil {
		return res, err
	}
	// Two starts of one instance (CLI, MCP, the manager) must not install at the same time: the second one leaves it.
	lock, ok, err := win.TryLock(m.lockPath(i, "bridge"))
	if err != nil || !ok {
		res.Warnings = append(res.Warnings, fmt.Sprintf("实例 %d 的执行桥正由另一次 modavd 在装，这次不装", i))
		return res, nil
	}
	r, err := m.installBridge(ctx, i, opt, res)
	lock.Unlock()
	// the instance in the result was read under this lock: the install is over now
	if r != nil && r.Instance.Busy == "bridging" {
		r.Instance.Busy = busyOf(m.stopping(i), false, false)
	}
	return r, err
}

// installBridge is finishBridge with the bridge lock already held by the caller.
func (m *Manager) installBridge(ctx context.Context, i int, opt StartOptions, res *StartResult) (*StartResult, error) {
	// The script's notes go to this log; the warnings of the cold boots come with their results (m, not mm, starts them).
	col := &logx.Collector{}
	mm := m.withLog(col.Sink())
	mm.Log.Infof("魔改 AVD 实例 %d 有执行桥的开机标记，桥还没装：现在装（中间要关机再开机一到两次）", i)
	notWritable := 0
	for round := 1; round <= bridgeInstallRounds; round++ {
		if opt.Progress != nil {
			opt.Progress(Progress{Step: "bridge", Message: fmt.Sprintf("实例 %d 装 arm64 执行桥（第 %d 轮）", i, round),
				Done: 0, Total: progressTotal})
		}
		run, err := mm.runBridgeScript(ctx, i, "install", false)
		if err != nil && errs.As(err).Code == errs.Canceled {
			res.Warnings = append(res.Warnings, col.Messages()...)
			return nil, err
		}
		if err != nil {
			mm.Log.Warnf("实例 %d 没装上执行桥，下次开机再试: %v", i, err)
			break
		}
		// 0: installed (or already there); 3: this boot was not writable, or the first remount only turned verity off
		if run.Code != 0 && run.Code != 3 {
			mm.Log.Warnf("实例 %d 没装上执行桥（退出码 %d：%s），下次开机再试；日志 %s", i, run.Code, run.Last, run.LogFile)
			break
		}
		if mm.guestBridge(ctx, i) == bridgeProp {
			break
		}
		// a second "not writable" in a row: another cold boot will not change that (an emulator without the marker
		// patch, say)
		if run.Code == 3 {
			if notWritable++; notWritable >= 2 {
				mm.Log.Warnf("实例 %d 冷启动后还是不能写系统分区（%s），不再试；日志 %s", i, run.Last, run.LogFile)
				break
			}
		} else {
			notWritable = 0
		}
		r2, err := m.bridgeRestart(ctx, i, opt)
		if err != nil {
			res.Warnings = append(res.Warnings, col.Messages()...)
			return nil, errs.Wrap(errs.As(err).Code, err, fmt.Sprintf("实例 %d 装执行桥时关机再开机失败: %v", i, err))
		}
		r2.Warnings = append(res.Warnings, r2.Warnings...)
		res = r2
	}
	if mm.guestBridge(ctx, i) == bridgeProp {
		mm.Log.Infof("实例 %d 已装好执行桥", i)
	} else {
		mm.Log.Warnf("实例 %d 没装上执行桥，下次开机再试；日志见 logs 里的 modavd-bridge-%d-*.log", i, i)
	}
	res.Warnings = append(res.Warnings, col.Messages()...)
	return res, nil
}

// updateBridge is the last step of a start that launched an instance with the bridge (Start calls it when the install
// step had nothing to do). When the guest's files are not the package's payload (bridgeOutdated: the package was
// updated since), the script's install replaces the files that differ (each is pushed under a temporary name, checked
// against the payload's hash and renamed into place), and one cold boot (same window and timeout, without the bridge
// step) loads them. Decided under the bridge lock: another modavd holding it is installing or updating already.
//
// The files the guest has keep working, so the update waits for a later start while an install holds the guest lock
// or an app is in the foreground once the setup is done (someone began to use the instance meanwhile; the update
// restarts adbd and boots cold), and a script that fails is only a warning, as in finishBridge.
func (m *Manager) updateBridge(ctx context.Context, i int, opt StartOptions, res *StartResult) (*StartResult, error) {
	lock, ok, err := win.TryLock(m.lockPath(i, "bridge"))
	if err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("实例 %d 的执行桥锁文件打不开，这次不看执行桥要不要更新: %v", i, err))
		return res, nil
	}
	if !ok {
		m.Log.Debugf("实例 %d 的执行桥锁在别的 modavd 手里（它在装或在更新），这次不看执行桥要不要更新", i)
		return res, nil
	}
	r, err := m.updateBridgeLocked(ctx, i, opt, res)
	lock.Unlock()
	// the instance in the result was read under this lock: the update is over now
	if r != nil && r.Instance.Busy == "bridging" {
		r.Instance.Busy = busyOf(m.stopping(i), false, false)
	}
	return r, err
}

func (m *Manager) updateBridgeLocked(ctx context.Context, i int, opt StartOptions,
	res *StartResult) (*StartResult, error) {
	if !m.bridgeOutdated(ctx, i) {
		return res, nil
	}
	col := &logx.Collector{}
	mm := m.withLog(col.Sink())
	if b, blocked := mm.bridgeUpdateBlocked(ctx, i); blocked {
		detail := ""
		if b.detail != "" {
			detail = "（" + b.detail + "）"
		}
		mm.Log.Warnf("实例 %d 的执行桥文件和根目录里的载荷不一样，但%s%s；更新要冷启动一次，这次先不更新，"+
			"下次由 modavd 或管理器开机时再更新", i, b.what, detail)
		res.Warnings = append(res.Warnings, col.Messages()...)
		return res, nil
	}
	mm.Log.Infof("魔改 AVD 实例 %d 的执行桥文件和根目录里的载荷不一样（多半是更新过 ModAVD 包）：现在更新，之后冷启动一次", i)
	if opt.Progress != nil {
		opt.Progress(Progress{Step: "bridge", Message: fmt.Sprintf("实例 %d 更新 arm64 执行桥", i), Done: 0,
			Total: progressTotal})
	}
	run, err := mm.runBridgeScript(ctx, i, "install", false)
	switch {
	case err != nil && errs.As(err).Code == errs.Canceled:
		res.Warnings = append(res.Warnings, col.Messages()...)
		return nil, err
	case err != nil:
		mm.Log.Warnf("实例 %d 没更新上执行桥，下次开机再试: %v", i, err)
	case run.Code != 0:
		mm.Log.Warnf("实例 %d 没更新上执行桥（退出码 %d：%s），下次开机再试；日志 %s", i, run.Code, run.Last, run.LogFile)
	case strings.Contains(run.Last, "什么都没改"):
		// the script found the files current after all (mode and label included): nothing to load
	default:
		r2, err := m.bridgeRestart(ctx, i, opt)
		if err != nil {
			res.Warnings = append(res.Warnings, col.Messages()...)
			return nil, errs.Wrap(errs.As(err).Code, err, fmt.Sprintf("实例 %d 更新执行桥后关机再开机失败: %v", i, err)).
				WithAdvice(bridgeUpdateRecovery(i, bridgeMarker(m.Root, i), run.LogFile))
		}
		r2.Warnings = append(res.Warnings, r2.Warnings...)
		res = r2
		if mm.bridgeOutdated(ctx, i) {
			mm.Log.Warnf("实例 %d 更新执行桥后冷启动，客体里的文件还是旧的；日志 %s", i, run.LogFile)
		} else {
			mm.Log.Infof("实例 %d 的执行桥已更新", i)
		}
	}
	res.Warnings = append(res.Warnings, col.Messages()...)
	return res, nil
}

// bridgeUpdateRecovery is the advice of a cold boot that failed after an update, in case the new files keep the
// instance from booting. Without the marker the instance boots the stock system, where the bridge's files are not
// there; uninstall then records that the instance wants no bridge (bridgeMetaKey), so no later start writes the
// marker again (it still ends in one more cold boot, the script finding the system not writable). logFile is the
// script's output ("" when it could not be written).
func bridgeUpdateRecovery(i int, marker, logFile string) string {
	s := fmt.Sprintf("要是新的执行桥让实例开不了机：先 modavd stop %d，删掉 %s，再跑 modavd bridge %d uninstall，"+
		"以后就以不带执行桥的原版系统开机；等修好的 ModAVD 包出来，再 modavd bridge %d install", i, marker, i, i)
	if logFile != "" {
		s += "。执行桥脚本的日志：" + logFile
	}
	return s
}

// bridgeUpdateBlocked: an install holds the guest lock or an app is in the foreground (the rules of owedMountGate,
// checked once; nothing stays held), so the update, which restarts adbd and boots cold, waits.
func (m *Manager) bridgeUpdateBlocked(ctx context.Context, i int) (mountBlocker, bool) {
	l, free, err := win.TryLock(m.guestLockPath(i))
	if err != nil || !free {
		return installBlocker(err), true
	}
	l.Unlock()
	if allowed, front := vulkanFixMountAllowed(m.shell(ctx, i, foregroundProbeScript, 0).Out); !allowed {
		return foregroundBlocker(front), true
	}
	return mountBlocker{}, false
}

// bridgeRestart shuts instance i down and boots it cold, without the bridge step (the caller does that).
func (m *Manager) bridgeRestart(ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
	sr, err := m.Stop(ctx, i, StopOptions{})
	if err != nil {
		return nil, err
	}
	if err := m.waitQemuGone(ctx, i); err != nil {
		return nil, err
	}
	r, err := bridgeStart(m, ctx, i, StartOptions{Window: opt.Window, Timeout: opt.Timeout, Progress: opt.Progress,
		NoBridge: true, creating: opt.creating})
	if err != nil {
		return nil, err
	}
	r.Warnings = append(append([]string{}, sr.Warnings...), r.Warnings...)
	return r, nil
}

// BridgeResult is the result of `modavd bridge`.
type BridgeResult struct {
	Index     int      `json:"index"`
	Action    string   `json:"action"`
	ExitCode  int      `json:"exit_code"` // the script's
	Installed bool     `json:"installed"` // the guest's native bridge is ours afterwards
	Message   string   `json:"message"`   // the script's last line
	LogFile   string   `json:"log_file"`  // the script's whole output
	Restarted bool     `json:"restarted"` // Bridge stopped and started the instance
	Stopped   bool     `json:"stopped"`   // it was off before and is off again
	Warnings  []string `json:"warnings"`
}

// Hooks for the tests (Bridge without an emulator): how Bridge finds the instance's qemu at first, starts it (in
// bridgeRestart too), reads the guest's native bridge and runs the script.
var (
	bridgeFindQemu = (*Manager).findQemu
	bridgeStart    func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error)
	bridgeGuest    = (*Manager).guestBridge
	bridgeScript   = (*Manager).runBridgeScript
)

// bridgeStart is set here: Start reaches bridgeRestart (the bridge step of a boot), so an initializer would be an
// initialization cycle.
func init() { bridgeStart = (*Manager).Start }

// Bridge installs, uninstalls or verifies the execution bridge on instance i. Restarts are cold boots: a running
// instance comes back the way it ran, with its window or headless (a start never switches, StartOptions.Window;
// headless when that cannot be told), and one that was off boots headless.
//   - install: places the marker; a running instance without the bridge is restarted, an off one started, and the
//     start installs (finishBridge). An instance that already has the bridge gets the script's install anyway, which
//     only updates a payload that differs from this package's; then it is restarted so the new one is loaded.
//   - uninstall: the script's uninstall on the running instance (started first without the install step), which
//     removes the payload, the property line, the marker and the asg it added; then a cold boot on the stock
//     translator.
//   - verify: the script's read-only verify; the instance must be running.
//
// An instance that was off is stopped again.
func (m *Manager) Bridge(ctx context.Context, i int, action string) (*BridgeResult, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	if err := m.notCreated(i, "不能装卸执行桥"); err != nil {
		return nil, err
	}
	if !bridgeGuestSupported(in.Config) {
		if action == "uninstall" {
			if err := m.setBridgeWanted(i, false); err != nil {
				return nil, err
			}
			if p := bridgeMarker(m.Root, i); markerExists(p) {
				if err := os.Remove(p); err != nil {
					return nil, errs.Wrap(errs.Internal, err, "删不掉 arm64 客体残留的执行桥开机标记: "+p)
				}
			}
		}
		return &BridgeResult{Index: i, Action: action, ExitCode: 0, Message: "arm64 客体不需要执行桥", Warnings: []string{}}, nil
	}
	bridgeLock, ok, err := win.TryLock(m.lockPath(i, "bridge"))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "实例执行桥锁文件打不开")
	}
	if !ok {
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正在装或卸执行桥，稍后再试", i)
	}
	defer bridgeLock.Unlock()
	res := &BridgeResult{Index: i, Action: action, ExitCode: -1, Warnings: []string{}}
	p, wasRunning := bridgeFindQemu(m, i)
	windowed := false // how the cold boots below come back
	if wasRunning {
		windowed, _ = qemuWindowed(p)
	}

	switch action {
	case "verify":
		if !wasRunning {
			return nil, errs.Newf(errs.Unsupported, "实例 %d 没开机", i).WithAdvice(fmt.Sprintf("先 modavd start %d", i))
		}
		run, err := bridgeScript(m, ctx, i, "verify", false)
		res.fill(run)
		res.Installed = bridgeGuest(m, ctx, i) == bridgeProp
		return res, err

	case "install":
		if err := m.setBridgeWanted(i, true); err != nil {
			return nil, err
		}
		if err := writeBridgeMarker(m.Root, i); err != nil {
			return nil, err
		}
		if wasRunning && bridgeGuest(m, ctx, i) != bridgeProp {
			// this boot may not be writable: boot again with the marker, the start installs
			if err := m.bridgeStop(ctx, i, res); err != nil {
				return nil, err
			}
			res.Restarted = true
		}
		if !wasRunning || res.Restarted {
			// this start installs (installBridge, under the lock held above; its cold boots keep the window)
			sr, err := bridgeStart(m, ctx, i, StartOptions{Window: windowed, bridgeLocked: true})
			if err != nil {
				return nil, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
		}
		if bridgeGuest(m, ctx, i) == bridgeProp {
			// Installed (now or before): the script's install once more updates a payload that differs from this
			// package's and does nothing when it is the same ("什么都没改"); an update is loaded on a cold boot.
			run, err := bridgeScript(m, ctx, i, "install", false)
			res.fill(run)
			if err != nil {
				return res, err
			}
			if run.Code == 0 && !strings.Contains(run.Last, "什么都没改") {
				sr, err := m.bridgeRestart(ctx, i, StartOptions{Window: windowed})
				if err != nil {
					return res, err
				}
				res.Warnings = append(res.Warnings, sr.Warnings...)
				res.Restarted = true
			}
		} else {
			res.Message = "开机后没装上执行桥，见警告和 logs 里的 modavd-bridge-*.log"
		}
		res.Installed = bridgeGuest(m, ctx, i) == bridgeProp
		if !wasRunning {
			if err := m.bridgeStop(ctx, i, res); err != nil {
				return res, err
			}
			res.Stopped = true
		}
		return res, nil

	case "uninstall":
		// recorded first, so none of the starts below (nor any later one) puts it back
		if err := m.setBridgeWanted(i, false); err != nil {
			return nil, err
		}
		if !wasRunning {
			sr, err := bridgeStart(m, ctx, i, StartOptions{NoBridge: true})
			if err != nil {
				return nil, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
		}
		installed := bridgeGuest(m, ctx, i) == bridgeProp
		run, err := bridgeScript(m, ctx, i, "uninstall", false)
		res.fill(run)
		if err != nil {
			return res, err
		}
		if run.Code == 3 {
			// the first remount of this instance only turned verity off: boot cold and run it again
			sr, err := m.bridgeRestart(ctx, i, StartOptions{Window: windowed})
			if err != nil {
				return res, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
			res.Restarted = true
			if run, err = bridgeScript(m, ctx, i, "uninstall", false); err != nil {
				res.fill(run)
				return res, err
			}
			res.fill(run)
		}
		if run.Code == 0 && installed {
			// the property line is read at boot: a cold boot (the marker is gone, so not writable) back on stock
			sr, err := m.bridgeRestart(ctx, i, StartOptions{Window: windowed})
			if err != nil {
				return res, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
			res.Restarted = true
		}
		res.Installed = bridgeGuest(m, ctx, i) == bridgeProp
		// An instance whose bridge was never installed (created by modavd, never booted by it) keeps the marker when
		// the script stops early: drop it, or the next start installs after all.
		if !res.Installed && regularFile(bridgeMarker(m.Root, i)) {
			if err := os.Remove(bridgeMarker(m.Root, i)); err == nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("实例 %d 的桥没装过，已删掉开机标记", i))
			}
		}
		if !wasRunning {
			if err := m.bridgeStop(ctx, i, res); err != nil {
				return res, err
			}
			res.Stopped = true
		}
		return res, nil
	}
	return nil, errs.Newf(errs.Usage, "只能是 install、uninstall 或 verify，收到「%s」", action)
}

func markerExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (r *BridgeResult) fill(run bridgeRun) {
	r.ExitCode, r.Message, r.LogFile = run.Code, run.Last, run.LogFile
}

// bridgeStop stops instance i and waits for its qemu to be gone.
func (m *Manager) bridgeStop(ctx context.Context, i int, res *BridgeResult) error {
	sr, err := m.Stop(ctx, i, StopOptions{})
	if err != nil {
		return err
	}
	res.Warnings = append(res.Warnings, sr.Warnings...)
	return m.waitQemuGone(ctx, i)
}

// waitQemuGone waits up to bridgeQemuGoneWait for instance i's qemu to exit after a stop.
func (m *Manager) waitQemuGone(ctx context.Context, i int) error {
	deadline := time.Now().Add(bridgeQemuGoneWait)
	for {
		if _, running := m.findQemu(i); !running {
			return nil
		}
		if time.Now().After(deadline) {
			return errs.Newf(errs.Timeout, "实例 %d 关机后等了 %.0f 秒，qemu 进程仍在运行", i, bridgeQemuGoneWait.Seconds())
		}
		select {
		case <-ctx.Done():
			return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		case <-time.After(time.Second):
		}
	}
}
