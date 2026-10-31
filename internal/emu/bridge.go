//go:build windows

package emu

// The arm64 execution bridge by default (10-30). A new instance gets the bridge's boot marker at create, so its
// first boot is already writable (the emulator boots writable while the marker is there, qemu fixq36 0001). A start
// that finds the marker while the guest's native bridge is still the stock translator runs the package's own
// tools\modavd-bridge.ps1 install once its setup is done. The script stays the one implementation of the install:
// remount, push the three payload files, edit vendor build.prop, set the GL transport to asg, verify.
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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

func (m *Manager) bridgeWanted(i int) bool {
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
	script := filepath.Join(m.Root.Dir, "tools", bridgeScriptName)
	if !regularFile(script) {
		return bridgeRun{Code: -1}, errs.Newf(errs.NotFound, "找不到 %s：包不完整", script).
			WithAdvice("用完整的新包覆盖解压一次")
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	// the payload of the SDK the instance runs on (the script's default is <root>\sdk, the same in a package root)
	payload := filepath.Join(m.Root.RuntimeSDK(), "emulator", "resources", "modavd")
	call := []string{"&", quote(script), action, "-Serial", quote(root.Serial(i)),
		"-AdbPort", strconv.Itoa(m.Root.AdbServerPort()), "-Adb", quote(m.Root.AdbExe()), "-Payload", quote(payload)}
	if reboot {
		call = append(call, "-Reboot")
	}
	command := "[Console]::OutputEncoding = [Text.Encoding]::UTF8; " + strings.Join(call, " ") + "; exit $LASTEXITCODE"
	ps := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")

	cctx, cancel := context.WithTimeout(ctx, bridgeScriptTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, ps, "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", command)
	// the script's adb calls: same environment as ours (an adb that starts a server must not scan local ports either)
	cmd.Env = m.ADB.Env()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
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
// Returns the result of the last start.
func (m *Manager) finishBridge(ctx context.Context, i int, opt StartOptions, res *StartResult) (*StartResult, error) {
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

// bridgeRestart shuts instance i down and boots it cold, without the bridge step (the caller does that).
func (m *Manager) bridgeRestart(ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
	sr, err := m.Stop(ctx, i, StopOptions{})
	if err != nil {
		return nil, err
	}
	if err := m.waitQemuGone(ctx, i); err != nil {
		return nil, err
	}
	r, err := m.Start(ctx, i, StartOptions{Window: opt.Window, Timeout: opt.Timeout, Progress: opt.Progress, NoBridge: true,
		creating: opt.creating})
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

// Bridge installs, uninstalls or verifies the execution bridge on instance i. Restarts are cold boots (headless when
// the instance was off; a running one comes back headless too).
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
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	if err := m.notCreated(i, "不能装卸执行桥"); err != nil {
		return nil, err
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
	_, wasRunning := m.findQemu(i)

	switch action {
	case "verify":
		if !wasRunning {
			return nil, errs.Newf(errs.Unsupported, "实例 %d 没开机", i).WithAdvice(fmt.Sprintf("先 modavd start %d", i))
		}
		run, err := m.runBridgeScript(ctx, i, "verify", false)
		res.fill(run)
		res.Installed = m.guestBridge(ctx, i) == bridgeProp
		return res, err

	case "install":
		if err := m.setBridgeWanted(i, true); err != nil {
			return nil, err
		}
		if err := writeBridgeMarker(m.Root, i); err != nil {
			return nil, err
		}
		if wasRunning && m.guestBridge(ctx, i) != bridgeProp {
			// this boot may not be writable: boot again with the marker, the start installs
			if err := m.bridgeStop(ctx, i, res); err != nil {
				return nil, err
			}
			res.Restarted = true
		}
		if !wasRunning || res.Restarted {
			// this start installs (installBridge, under the lock held above)
			sr, err := m.Start(ctx, i, StartOptions{bridgeLocked: true})
			if err != nil {
				return nil, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
		}
		if m.guestBridge(ctx, i) == bridgeProp {
			// Installed (now or before): the script's install once more updates a payload that differs from this
			// package's and does nothing when it is the same ("什么都没改"); an update is loaded on a cold boot.
			run, err := m.runBridgeScript(ctx, i, "install", false)
			res.fill(run)
			if err != nil {
				return res, err
			}
			if run.Code == 0 && !strings.Contains(run.Last, "什么都没改") {
				sr, err := m.bridgeRestart(ctx, i, StartOptions{})
				if err != nil {
					return res, err
				}
				res.Warnings = append(res.Warnings, sr.Warnings...)
				res.Restarted = true
			}
		} else {
			res.Message = "开机后没装上执行桥，见警告和 logs 里的 modavd-bridge-*.log"
		}
		res.Installed = m.guestBridge(ctx, i) == bridgeProp
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
			sr, err := m.Start(ctx, i, StartOptions{NoBridge: true})
			if err != nil {
				return nil, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
		}
		installed := m.guestBridge(ctx, i) == bridgeProp
		run, err := m.runBridgeScript(ctx, i, "uninstall", false)
		res.fill(run)
		if err != nil {
			return res, err
		}
		if run.Code == 3 {
			// the first remount of this instance only turned verity off: boot cold and run it again
			sr, err := m.bridgeRestart(ctx, i, StartOptions{})
			if err != nil {
				return res, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
			res.Restarted = true
			if run, err = m.runBridgeScript(ctx, i, "uninstall", false); err != nil {
				res.fill(run)
				return res, err
			}
			res.fill(run)
		}
		if run.Code == 0 && installed {
			// the property line is read at boot: a cold boot (the marker is gone, so not writable) back on stock
			sr, err := m.bridgeRestart(ctx, i, StartOptions{})
			if err != nil {
				return res, err
			}
			res.Warnings = append(res.Warnings, sr.Warnings...)
			res.Restarted = true
		}
		res.Installed = m.guestBridge(ctx, i) == bridgeProp
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
