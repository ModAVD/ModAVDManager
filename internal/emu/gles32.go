package emu

// The reported OpenGL ES version (ModAVD/ModAVD#131, experimental). Some apps list GLES 3.2 in their manifest and the
// Play Store hides them from a device whose ro.opengles.version says less (ModAVD reports 3.1, 196609). The switch
// `modavd gles32 <i> on` makes the guest report 3.2 (196610); the emulator still renders 3.1, so an app that really
// needs 3.2 installs and then does not run. Off unless asked for, per instance; new instances never get it.
//
// How: init.ranchu.rc sets the property in early-init, after build.prop was read, so a line in /vendor/build.prop
// is taken first. /vendor is writable only on instances with the execution bridge (their boot marker makes the
// emulator boot with a writable system, bridge.go), so the switch needs the bridge; the guest script
// (guest/scripts/gles32.sh) edits the file in the overlay, and the change shows after a cold boot: on and off boot
// the instance cold once and then check what the guest reports.
//
// The record (instances.<i>.gles32 in mas-avd.json) stops `bridge uninstall` while the switch is on, as it does for
// the Play Store: without the bridge the line could not be taken out again.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/guest"
	"modavd/internal/jsonord"
)

const (
	// gles32MetaKey is instances.<i>.gles32 in mas-avd.json: true once `gles32 <i> on` wrote the line, false after off.
	// Absent on every instance that never had it.
	gles32MetaKey = "gles32"
	// gles32GuestDir is where the script is pushed to; removed after each run.
	gles32GuestDir = "/data/local/tmp/modavd-gles32"
	// gles32ScriptTimeout bounds one run of the guest script.
	gles32ScriptTimeout = 2 * time.Minute
	// gles32PushTimeout bounds the push of the script.
	gles32PushTimeout = time.Minute
)

// The results of the guest script's runs (the word after RESULT).
const (
	gles32ResultOn         = "on"
	gles32ResultOff        = "off"
	gles32ResultPendingOn  = "pending-on"
	gles32ResultPendingOff = "pending-off"
	gles32ResultReboot     = "reboot"
	gles32ResultAlready    = "already"
	gles32ResultNotWriting = "not-writable"
)

// GLES32ReasonNoBridge is the errs.Reason of the switch's refusal on an instance that does not boot with a writable
// system (errs.NotReady): the manager offers the bridge by it.
const GLES32ReasonNoBridge = "gles32_no_bridge"

// GLES32PlayStoreNote is the note on the result when the instance has the Play Store: the store keeps what it knew of
// the device.
const GLES32PlayStoreNote = "Play 商店会记着旧的设备信息；要它按新版本重新判断兼容，到 设置 → 应用 → Google Play 商店 → 存储，清除数据（账号不会退出）"

// GLES32Result is the result of `modavd gles32`.
type GLES32Result struct {
	Index  int    `json:"index"`
	Action string `json:"action"`
	// State is what the guest has after the action: on (the line is in and the guest reports 196610), off (neither),
	// pending-on / pending-off (the line and the reported version disagree: a cold boot is still to come).
	State     string   `json:"state"`
	Enabled   bool     `json:"enabled"`   // State is on
	Prop      string   `json:"prop"`      // ro.opengles.version as the guest reports it
	Restarted bool     `json:"restarted"` // it was shut down and booted cold
	Stopped   bool     `json:"stopped"`   // it was off before and is off again
	Notes     []string `json:"notes"`
	Warnings  []string `json:"warnings"`
}

// gles32Script is one run of the guest script (a hook for the tests).
var gles32Script = (*Manager).runGLES32Script

// gles32Record: instances.<i>.gles32 is true.
func gles32Record(meta *jsonord.Obj) bool {
	if meta == nil {
		return false
	}
	b, ok := meta.Get(gles32MetaKey).BoolVal()
	return ok && b
}

// gles32Wanted: the switch is on (bridge uninstall refuses).
func (m *Manager) gles32Wanted(i int) bool {
	return gles32Record(m.Root.InstanceMeta(i))
}

func (m *Manager) setGLES32Wanted(i int, on bool) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(gles32MetaKey, jsonord.NewBool(on))
		return nil
	})
}

// gles32BridgeError is nil when instance i boots with a writable system (it has the bridge and its marker).
func (m *Manager) gles32BridgeError(i int, cfg *avd.Ini) error {
	if !bridgeGuestSupported(cfg) {
		return errs.WithDetail(errs.New(errs.Unsupported, "arm64 客体没有「报告 GLES 3.2」这一项"),
			errs.Reason, PlayStoreReasonUnsupported)
	}
	if !m.bridgeWanted(i, cfg) || !regularFile(bridgeMarker(m.Root, i)) {
		e := errs.Newf(errs.NotReady, "实例 %d 没有 arm64 执行桥，不按可写系统镜像开机，改不了它报的 GLES 版本", i).
			WithAdvice(fmt.Sprintf("先运行 modavd bridge %d install 装上执行桥，再开这个开关", i))
		return errs.WithDetail(e, errs.Reason, GLES32ReasonNoBridge)
	}
	return nil
}

// GLES32Check is what GLES32 checks before it touches anything, without a lock or a boot: the host, the action, the
// instance (there, not being created) and, for on and off, a writable system (the bridge). The manager calls it before
// it starts the job, so the button hears at once.
func (m *Manager) GLES32Check(i int, action string) error {
	_, err := m.gles32Check(i, action)
	return err
}

func (m *Manager) gles32Check(i int, action string) (*avd.Instance, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	switch action {
	case "on", "off", "status":
	default:
		return nil, errs.Newf(errs.Usage, "只能是 on、off 或 status，收到「%s」", action)
	}
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	if err := m.notCreated(i, "不能改它报的 GLES 版本"); err != nil {
		return nil, err
	}
	if action != "status" {
		if err := m.gles32BridgeError(i, in.Config); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// runGLES32Script runs the guest script's action on instance i (running) as root; the push directory is removed
// afterwards.
func (m *Manager) runGLES32Script(ctx context.Context, i int, action string) (playStoreRun, error) {
	serial := m.Root.Ports().Serial(i)
	sh := func(cmd string, timeout time.Duration) adb.Result { return m.shell(ctx, i, cmd, timeout) }
	if res := sh("su 0 rm -rf "+gles32GuestDir+" && mkdir -p "+gles32GuestDir, 0); res.Code != 0 {
		return playStoreRun{}, errs.Newf(errs.LaunchFailed, "实例 %d 里建不了 %s：%s", i, gles32GuestDir, tail(res.Out, 300))
	}
	defer sh("su 0 rm -rf "+gles32GuestDir, 0)
	script, err := guest.LocalFile(filepath.Join(os.TempDir(), "modavd-guest"), guest.GLES32)
	if err != nil {
		return playStoreRun{}, errs.Wrap(errs.Internal, err, "写不了客体脚本 "+guest.GLES32)
	}
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: serial, Timeout: gles32PushTimeout, Reregister: true},
		"push", script, gles32GuestDir+"/"+guest.GLES32)
	if res.Code != 0 {
		return playStoreRun{}, errs.Newf(errs.LaunchFailed, "实例 %d 推送 %s 失败：%s", i, guest.GLES32, tail(res.Out, 300))
	}
	res = sh(fmt.Sprintf("su 0 sh %s/%s %s", gles32GuestDir, guest.GLES32, action), gles32ScriptTimeout)
	run := parsePlayStoreRun(res.Out)
	run.Ran = true
	if run.Result == "" {
		return run, errs.Newf(errs.LaunchFailed, "实例 %d 的 GLES 3.2 脚本（%s）没有跑完：%s", i, action, tail(res.Out, 300))
	}
	return run, nil
}

func gles32Failed(i int, action string, run playStoreRun) error {
	msg := fmt.Sprintf("实例 %d 的 GLES 3.2 开关没做成（%s %s）", i, action, run.Result)
	if len(run.Lines) > 0 {
		msg += "：" + run.Lines[len(run.Lines)-1]
	}
	if run.Result == gles32ResultNotWriting {
		e := errs.New(errs.NotReady, msg).WithAdvice(fmt.Sprintf("实例的系统镜像不可写：先运行 modavd bridge %d install，再试一次", i))
		return errs.WithDetail(e, errs.Reason, GLES32ReasonNoBridge)
	}
	return errs.New(errs.LaunchFailed, msg)
}

// gles32Status runs the script's status and fills res from it (State, Enabled, Prop).
func (m *Manager) gles32Status(ctx context.Context, i int, res *GLES32Result) error {
	run, err := gles32Script(m, ctx, i, "status")
	if err != nil {
		return err
	}
	switch run.Result {
	case gles32ResultOn, gles32ResultOff, gles32ResultPendingOn, gles32ResultPendingOff:
	default:
		return gles32Failed(i, "status", run)
	}
	res.State, res.Enabled, res.Prop = run.Result, run.Result == gles32ResultOn, ""
	for _, l := range run.Lines {
		if p, ok := strings.CutPrefix(l, "prop "); ok {
			res.Prop = strings.TrimSpace(p)
		} else if l == "prop" {
			res.Prop = ""
		}
	}
	return nil
}

// GLES32 turns the reported GLES 3.2 on or off for instance i, or checks (status) what the guest reports. on and off
// boot an instance that is off and stop it again, also when they fail (best effort: what stands in the way is a
// warning); their cold boot brings a running instance back the way it ran (window or headless). status needs it
// running. Everything GLES32Check refuses is refused before the instance is touched. After the cold boot the script's
// status must say what was asked for; if it does not, that is an error (the record stays as the script left the file).
func (m *Manager) GLES32(ctx context.Context, i int, action string) (*GLES32Result, error) {
	if _, err := m.gles32Check(i, action); err != nil {
		return nil, err
	}
	lock, err := m.playStoreLock(i)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	res := &GLES32Result{Index: i, Action: action, Notes: []string{}, Warnings: []string{}}
	p, wasRunning, err := playStoreFindQemu(m, i)
	if err != nil {
		return nil, err
	}
	windowed := false
	if wasRunning {
		windowed, _ = qemuWindowed(p)
	}
	if action == "status" {
		if !wasRunning {
			return nil, playStoreNotRunning(i)
		}
		return res, m.gles32Status(ctx, i, res)
	}

	warn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		m.Log.Warnf("%s", msg)
		res.Warnings = append(res.Warnings, msg)
	}
	// ours: the last start was this call's (one that is busy found another start under way, not to be stopped)
	ours := false
	boot := func() error {
		sr, err := playStoreStart(m, ctx, i, StartOptions{Window: windowed, bridgeLocked: true})
		ours = errs.CodeOf(err) != errs.Busy
		if err != nil {
			return err
		}
		res.Warnings = append(res.Warnings, sr.Warnings...)
		return nil
	}
	restart := func() error {
		w, err := playStoreStop(m, ctx, i)
		res.Warnings = append(res.Warnings, w...)
		if err != nil {
			return err
		}
		res.Restarted = true
		return boot()
	}
	// fail ends a failed run: an instance that was off is shut down again, even after a cancel (best effort; what
	// stands in the way goes into the warnings and the log, the error stays the one that failed).
	fail := func(err error) (*GLES32Result, error) {
		if wasRunning || !ours {
			return res, err
		}
		w, serr := playStoreStop(m, context.WithoutCancel(ctx), i)
		res.Warnings = append(res.Warnings, w...)
		if serr != nil {
			warn("实例 %d 原来是关着的，没做成后也没能关机：%s", i, errs.As(serr).Error())
			return res, err
		}
		res.Stopped = true
		m.Log.Infof("实例 %d 原来是关着的，没做成后已经关机", i)
		return res, err
	}
	if !wasRunning {
		if err := boot(); err != nil {
			return fail(err)
		}
	}

	want, pending := gles32ResultOn, gles32ResultPendingOn
	if action == "off" {
		want, pending = gles32ResultOff, gles32ResultPendingOff
	}
	run, err := gles32Script(m, ctx, i, action)
	if err == nil && run.Result != gles32ResultReboot && run.Result != gles32ResultAlready {
		err = gles32Failed(i, action, run)
	}
	if err != nil {
		return fail(err)
	}
	// recorded once the file says so (bridge uninstall refuses from then on, while the line is there)
	if err := m.setGLES32Wanted(i, action == "on"); err != nil {
		return fail(err)
	}
	needBoot := run.Result == gles32ResultReboot
	if !needBoot {
		// the file already was as asked: a cold boot is due only when the guest has not been through one since
		if err := m.gles32Status(ctx, i, res); err != nil {
			return fail(err)
		}
		needBoot = res.State == pending
	}
	if needBoot {
		if err := restart(); err != nil {
			return fail(err)
		}
	}
	if err := m.gles32Status(ctx, i, res); err != nil {
		return fail(err)
	}
	if res.State != want {
		return fail(errs.Newf(errs.LaunchFailed, "实例 %d 冷启动后 ro.opengles.version 是「%s」（%s），和「%s」对不上", i, res.Prop, res.State, action).
			WithAdvice(fmt.Sprintf("运行 modavd gles32 %d status 再看一次；还不对就把这条 %s 的输出发给维护者", i, action)))
	}
	if m.playStoreWanted(i) {
		res.Notes = append(res.Notes, GLES32PlayStoreNote)
	}
	if !wasRunning {
		w, err := playStoreStop(m, ctx, i)
		res.Warnings = append(res.Warnings, w...)
		if err != nil {
			return res, err
		}
		res.Stopped = true
	}
	return res, nil
}
