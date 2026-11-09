package emu

// The Google Play Store (ModAVD/ModAVD#125, package playstore). Off unless asked for: `modavd playstore <i> install`,
// the manager's 「补全 Google Play 商店」 on an instance; new instances never get it. Its five files
// (root.PlayStoreComponent, sdk\playstore) come from the root, put there by `components fetch --play-store` or
// `components import-playstore` (package components); they are pushed into the guest, where the guest script
// (guest/scripts/playstore.sh) checks them against the pinned SHA-256 and copies them into the instance's writable
// system overlay, hiding the image's own placeholder com.android.vending. Windows uses the execution bridge's
// writable boot marker. Native macOS ARM64 uses an independent persistent request and the standard -writable-system
// flag; it does not install an execution bridge. Cold boots enable the overlay and load the replacement packages.
//
// Uninstalling takes two runs of the script with a cold boot between: the first disables the store (so it cannot
// update itself again), removes the updates Google Play installed for the store, Play services, GSF and Partner Setup
// and deletes the files from the overlay's upper layer; the second, after the boot, enables com.android.vending again,
// by then the image's placeholder.
//
// An install that fails half way (the overlay out of space ...) is taken back by the script; what it cannot take back
// makes it partial, recorded as such (playStore: "partial"), so the manager offers 移除 too and bridge uninstall still
// refuses. An instance that was off is shut down again after a failure as well.
//
// `playstore <i> gsfid` reads the instance's GSF ID, which https://www.google.com/android/uncertified/ registers so
// Google's apps work on a device that is not Play Protect certified.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/guest"
	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/playstore"
	"modavd/internal/root"
)

const (
	// playStoreMetaKey is instances.<i>.playStore in mas-avd.json: true once playstore install put the store in, false
	// after playstore uninstall, "partial" after an install that failed and could not take back all it put in (or
	// stopped without saying). Absent on every instance that never had it.
	playStoreMetaKey = "playStore"
	// playStoreGuestDir is where the files and the script are pushed to; removed after each run.
	playStoreGuestDir = "/data/local/tmp/modavd-playstore"
	// playStoreScriptTimeout bounds one run of the guest script (the install copies about 230 MB).
	playStoreScriptTimeout = 5 * time.Minute
	// playStorePushTimeout bounds the push of one file (Play services is 144 MB).
	playStorePushTimeout = 5 * time.Minute
)

// The results of the guest script's runs (the word after RESULT).
const (
	playStoreInstalled   = "installed"
	playStorePartial     = "partial"
	playStoreAbsent      = "absent"
	playStoreReboot      = "reboot"
	playStoreGone        = "gone"
	playStoreNotWritable = "not-writable"
	playStoreFailure     = "failed" // install: what the run put in is taken back
	playStoreOK          = "ok"     // gsfid: the lines "decimal <id>" and "hex <id>" follow
	playStoreNoID        = "no-id"  // gsfid: GSF has not got an ID yet
)

// playStoreUndoneAtBoot is the line of an install's undo when some of it (a file that replaced one of the image's,
// the placeholder's whiteout) was taken back in the overlay's upper layer, which the guest sees from its next boot.
const playStoreUndoneAtBoot = "undone from the next boot"

// PlayStoreUncertifiedURL is where a GSF ID is registered (the documentation site's step 6).
const PlayStoreUncertifiedURL = "https://www.google.com/android/uncertified/"

// Values of errs.Reason on the store's refusals, for callers that offer the way out (the manager: 补全组件 for the
// files, the bridge for the rest).
const (
	PlayStoreReasonFiles       = "playstore_files_missing" // errs.NotReady: the root has not got the five files (or they are damaged)
	PlayStoreReasonNoBridge    = "no_bridge"               // errs.NotReady: the instance does not boot with a writable system
	PlayStoreReasonUnsupported = "unsupported_guest"       // errs.Unsupported: wrong API or ABI for this host
	PlayStoreReasonNoGSFID     = "no_gsf_id"               // errs.NotReady: GSF has no ID yet (gsfid; it registers once online)
)

// PlayStoreResult is the result of `modavd playstore`.
type PlayStoreResult struct {
	Index  int    `json:"index"`
	Action string `json:"action"`
	// State is what the guest has after the action: installed (all files in place, the placeholder hidden), absent
	// (none of them, the placeholder back) or partial; "" when the instance was off and is off again before a check.
	State     string   `json:"state"`
	Installed bool     `json:"installed"` // State is installed
	Files     []string `json:"files"`     // the guest script's lines about each file and the placeholder
	Restarted bool     `json:"restarted"` // it was shut down and booted cold
	Stopped   bool     `json:"stopped"`   // it was off before and is off again
	Warnings  []string `json:"warnings"`
}

// PlayStoreGSFIDResult is the result of `modavd playstore <i> gsfid`: the GSF ID (Google Services Framework's
// android_id) registered at PlayStoreUncertifiedURL. Decimal as GSF keeps it, Hex the same number (lower case; two's
// complement for a negative one, as printf %x writes it).
type PlayStoreGSFIDResult struct {
	Index   int    `json:"index"`
	Decimal string `json:"decimal"`
	Hex     string `json:"hex"`
}

// playStoreRun is one run of the guest script.
type playStoreRun struct {
	Result string   // the word after RESULT, "" when the script did not get there
	Lines  []string // its other output lines
	Ran    bool     // the script was started (a Result of "" then: it may have changed something before it stopped)
}

// Hooks for the tests (PlayStore without an emulator).
var (
	playStoreFindQemu = (*Manager).findQemu
	playStoreStart    = (*Manager).Start
	playStoreStop     = func(m *Manager, ctx context.Context, i int) ([]string, error) {
		sr, err := m.Stop(ctx, i, StopOptions{playStoreLocked: true})
		if err != nil {
			return nil, err
		}
		return sr.Warnings, m.waitQemuGone(ctx, i)
	}
	playStoreScript = (*Manager).runPlayStoreScript
	// playStoreFilesReady: the component in dir is complete (every file's pinned SHA-256).
	playStoreFilesReady = func(dir string) bool { return root.PlayStoreComponent().Complete(dir) }
)

// playStoreDir is the store's component directory in the SDK the instances run on.
func (m *Manager) playStoreDir() string {
	return filepath.Join(m.Root.RuntimeSDK(), root.PlayStoreDir)
}

// playStoreReady: the component is there and every file matches its pinned SHA-256; otherwise errs.NotReady
// (PlayStoreReasonFiles) with the ways to get them.
func (m *Manager) playStoreReady() error {
	if playStoreFilesReady(m.playStoreDir()) {
		return nil
	}
	e := errs.Newf(errs.NotReady, "这个根目录还没有 Google Play 商店的五个文件，或者文件不全、已损坏（%s）", m.playStoreDir()).
		WithAdvice("先在管理器的「补全组件」里勾选「补全 Google Play 商店服务」（或者用自己的文件导入）；" +
			"命令行是 modavd components fetch --play-store（下载约 1.5 GB，只留五个文件）或 modavd components import-playstore <文件>")
	return errs.WithDetail(e, errs.Reason, PlayStoreReasonFiles)
}

// playStoreRecord is what instances.<i>.playStore says: playStoreInstalled (true), playStorePartial ("partial") or ""
// (false, absent, anything else).
func playStoreRecord(meta *jsonord.Obj) string {
	if meta == nil {
		return ""
	}
	v := meta.Get(playStoreMetaKey)
	if b, ok := v.BoolVal(); ok && b {
		return playStoreInstalled
	}
	if s, ok := v.Str(); ok && s == playStorePartial {
		return playStorePartial
	}
	return ""
}

// playStoreWanted: instances.<i>.playStore says the store, or some of it, is in the overlay (bridge uninstall refuses).
func (m *Manager) playStoreWanted(i int) bool {
	return playStoreRecord(m.Root.InstanceMeta(i)) != ""
}

func (m *Manager) setPlayStoreWanted(i int, on bool) error {
	return m.setPlayStoreRecord(i, jsonord.NewBool(on))
}

func (m *Manager) setPlayStoreRecord(i int, v *jsonord.Value) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(playStoreMetaKey, v)
		return nil
	})
}

// playStoreBridgeError checks the matching native guest, or the Windows bridge's writable boot request.
func (m *Manager) playStoreBridgeError(i int, cfg *avd.Ini) error {
	if nativePlayStore() {
		return playStoreGuestError(cfg)
	}
	if !bridgeGuestSupported(cfg) {
		return errs.WithDetail(errs.New(errs.Unsupported, "arm64 客体没有 Google Play 商店这一项"),
			errs.Reason, PlayStoreReasonUnsupported)
	}
	if !m.bridgeWanted(i, cfg) || !regularFile(bridgeMarker(m.Root, i)) {
		e := errs.Newf(errs.NotReady, "实例 %d 没有 arm64 执行桥，不按可写系统镜像开机，放不进 Google Play 商店", i).
			WithAdvice(fmt.Sprintf("先运行 modavd bridge %d install 装上执行桥，再补全商店", i))
		return errs.WithDetail(e, errs.Reason, PlayStoreReasonNoBridge)
	}
	return nil
}

// PlayStoreCheck is what PlayStore (and PlayStoreGSFID, action gsfid) checks before it touches anything, without a
// lock or a boot: the host, the action, the instance (there, not being created) and, for install, a writable system
// (the Windows bridge, or the supported native profile) and the root's five files (their pinned SHA-256). The manager calls it before it starts the job, so
// the button hears at once.
func (m *Manager) PlayStoreCheck(i int, action string) error {
	_, err := m.playStoreCheck(i, action)
	return err
}

func (m *Manager) playStoreCheck(i int, action string) (*avd.Instance, error) {
	if err := requireLifecycle(); err != nil {
		return nil, err
	}
	switch action {
	case "install", "uninstall", "status", "gsfid":
	default:
		return nil, errs.Newf(errs.Usage, "只能是 install、uninstall、status 或 gsfid，收到「%s」", action)
	}
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	if err := m.notCreated(i, "不能装卸 Google Play 商店"); err != nil {
		return nil, err
	}
	if nativePlayStore() {
		if err := playStoreGuestError(in.Config); err != nil {
			return nil, err
		}
	}
	if action == "install" {
		// the bridge first: without it the files would not help
		if err := m.playStoreBridgeError(i, in.Config); err != nil {
			return nil, err
		}
		if err := m.playStoreReady(); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// runPlayStoreScript runs the guest script's action on instance i (running) as root: the script and the file list
// are pushed first, and for install the component's files too; the push directory is removed afterwards.
func (m *Manager) runPlayStoreScript(ctx context.Context, i int, action string) (playStoreRun, error) {
	serial := m.Root.Ports().Serial(i)
	sh := func(cmd string, timeout time.Duration) adb.Result { return m.shell(ctx, i, cmd, timeout) }
	if res := sh("su 0 rm -rf "+playStoreGuestDir+" && mkdir -p "+playStoreGuestDir, 0); res.Code != 0 {
		return playStoreRun{}, errs.Newf(errs.LaunchFailed, "实例 %d 里建不了 %s：%s", i, playStoreGuestDir, tail(res.Out, 300))
	}
	defer sh("su 0 rm -rf "+playStoreGuestDir, 0)

	tmp, err := os.MkdirTemp("", "modavd-playstore-")
	if err != nil {
		return playStoreRun{}, errs.Wrap(errs.Internal, err, "建不了临时目录")
	}
	defer os.RemoveAll(tmp)
	list := filepath.Join(tmp, "files.txt")
	if err := os.WriteFile(list, []byte(playstore.CurrentProfile().Manifest()), 0o644); err != nil {
		return playStoreRun{}, errs.Wrap(errs.Internal, err, "写不了文件清单")
	}
	script, err := guest.LocalFile(filepath.Join(os.TempDir(), "modavd-guest"), guest.PlayStore)
	if err != nil {
		return playStoreRun{}, errs.Wrap(errs.Internal, err, "写不了客体脚本 "+guest.PlayStore)
	}
	pushes := []string{script, list}
	if action == "install" {
		for _, f := range playstore.CurrentProfile().Files {
			pushes = append(pushes, filepath.Join(m.playStoreDir(), f.Name))
		}
	}
	for _, local := range pushes {
		res := m.ADB.Run(ctx, adb.RunOptions{Serial: serial, Timeout: playStorePushTimeout, Reregister: true},
			"push", local, playStoreGuestDir+"/"+filepath.Base(local))
		if res.Code != 0 {
			return playStoreRun{}, errs.Newf(errs.LaunchFailed, "实例 %d 推送 %s 失败：%s", i, filepath.Base(local), tail(res.Out, 300))
		}
	}
	mode := ""
	if nativePlayStore() {
		mode = " native"
	}
	res := sh(fmt.Sprintf("su 0 sh %s/%s %s %s%s", playStoreGuestDir, guest.PlayStore, action, playStoreGuestDir, mode), playStoreScriptTimeout)
	run := parsePlayStoreRun(res.Out)
	run.Ran = true
	if run.Result == "" {
		return run, errs.Newf(errs.LaunchFailed, "实例 %d 的 Google Play 商店脚本（%s）没有跑完：%s", i, action, tail(res.Out, 300))
	}
	return run, nil
}

// parsePlayStoreRun splits the script's output into its lines and the RESULT word (the last one wins).
func parsePlayStoreRun(out string) playStoreRun {
	run := playStoreRun{Lines: []string{}}
	for _, line := range outputLines(out) {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "RESULT "):
			run.Result = strings.TrimSpace(strings.TrimPrefix(line, "RESULT "))
		default:
			run.Lines = append(run.Lines, line)
		}
	}
	return run
}

// fileLines are the lines of a status about the files (staged: a copy an install left half way) and the placeholder.
func fileLines(lines []string) []string {
	out := []string{}
	for _, l := range lines {
		for _, p := range []string{"play ", "image ", "absent ", "staged ", "stub ", "package "} {
			if strings.HasPrefix(l, p) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

func playStoreFailed(i int, action string, run playStoreRun) error {
	what := map[string]string{"install": "装", "uninstall": "卸", "status": "查"}[action]
	msg := fmt.Sprintf("实例 %d 没%s好 Google Play 商店（%s）", i, what, run.Result)
	if action == "gsfid" {
		msg = fmt.Sprintf("实例 %d 没取到 GSF ID（%s）", i, run.Result)
	}
	if len(run.Lines) > 0 {
		msg += "：" + run.Lines[len(run.Lines)-1]
	}
	switch {
	case run.Result == playStoreNotWritable && nativePlayStore():
		return errs.WithDetail(errs.New(errs.NotReady, msg).WithAdvice("实例仍不能写入系统覆盖层，请检查模拟器是否支持 -writable-system，以及 remount 的错误后重试"), errs.Reason, "playstore_not_writable")
	case run.Result == playStoreNotWritable:
		e := errs.New(errs.NotReady, msg).WithAdvice(fmt.Sprintf("实例的系统镜像不可写：先运行 modavd bridge %d install，再试一次", i))
		return errs.WithDetail(e, errs.Reason, PlayStoreReasonNoBridge)
	case action == "install" && run.Result == playStorePartial:
		return errs.New(errs.LaunchFailed, msg).WithAdvice(fmt.Sprintf("放进去的文件没能全部撤回：运行 modavd playstore %d uninstall 移除（管理器里是「移除 Google Play 商店」），"+
			"或者解决问题后再补全一次", i))
	case action == "install" && run.Result == playStoreFailure:
		return errs.New(errs.LaunchFailed, msg).WithAdvice("这次放进去的文件已经撤回，解决问题后可以再补全一次")
	}
	return errs.New(errs.LaunchFailed, msg)
}

// noGSFID: GSF has not registered the instance yet (it does once online).
func noGSFID(i int) error {
	e := errs.Newf(errs.NotReady, "实例 %d 还没有 GSF ID", i).
		WithAdvice("Google 服务框架联网登记后才有：开着网等一两分钟，再试一次")
	return errs.WithDetail(e, errs.Reason, PlayStoreReasonNoGSFID)
}

// parseGSFID reads a gsfid run: its decimal line (hex is worked out here, not taken from the guest's printf).
func parseGSFID(i int, run playStoreRun) (*PlayStoreGSFIDResult, error) {
	switch run.Result {
	case playStoreOK:
	case playStoreNoID:
		return nil, noGSFID(i)
	default:
		return nil, playStoreFailed(i, "gsfid", run)
	}
	for _, l := range run.Lines {
		d, ok := strings.CutPrefix(l, "decimal ")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(d), 10, 64)
		if err != nil {
			return nil, errs.Newf(errs.LaunchFailed, "实例 %d 的 GSF ID 不是数字：%s", i, d)
		}
		if n == 0 {
			return nil, noGSFID(i)
		}
		return &PlayStoreGSFIDResult{Index: i, Decimal: strconv.FormatInt(n, 10), Hex: strconv.FormatUint(uint64(n), 16)}, nil
	}
	return nil, noGSFID(i)
}

// playStoreLock takes instance i's bridge lock: the store lives in the bridge's overlay, and holding it makes status
// say busy (bridging), keeps delete, compact, bridge actions and the store's other actions (they share the guest's push
// directory) out meanwhile; the starts of PlayStore run the bridge step under it (bridgeLocked).
func (m *Manager) playStoreLock(i int) (*host.FileLock, error) {
	lock, ok, err := m.tryOperationLock(i, "bridge")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, playStoreBusy(i)
	}
	if nativePlayStore() {
		// Native Start/Stop check our bridge lock after taking their own lock.
		// Verify both are idle while holding it: an older lifecycle operation
		// finishes first, and a newer one sees the bridge lock and returns busy.
		for _, kind := range []string{"start", "stop"} {
			guard, idle, err := m.tryOperationLock(i, kind)
			if err != nil || !idle {
				lock.Unlock()
				if err != nil {
					return nil, err
				}
				return nil, errs.WithDetails(errs.Newf(errs.Busy, "实例 %d 正在开机或关机，稍后再装卸 Google Play 商店", i),
					map[string]string{errs.Reason: "locked", errs.Lock: kind})
			}
			guard.Unlock()
		}
	}
	return lock, nil
}

func playStoreBusy(i int) error {
	return errs.WithDetails(errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正在装卸执行桥或 Google Play 商店，稍后再试", i),
		map[string]string{errs.Reason: "locked", errs.Lock: "bridge"})
}

func playStoreNotRunning(i int) error {
	return errs.Newf(errs.Unsupported, "实例 %d 没开机", i).WithAdvice(fmt.Sprintf("先 modavd start %d", i))
}

// PlayStore installs, uninstalls or checks (status) the Google Play Store on instance i. install and uninstall boot an
// instance that is off and stop it again, also when they fail (best effort: what stands in the way is a warning);
// their cold boots bring a running instance back the way it ran (window or headless). status needs it running.
// Everything PlayStoreCheck refuses is refused before the instance is touched. An install that fails leaves the
// record as it was, unless the script could not take back all it put in (or stopped without saying): then it is
// partial.
func (m *Manager) PlayStore(ctx context.Context, i int, action string) (*PlayStoreResult, error) {
	if action == "gsfid" {
		// its own result: PlayStoreGSFID
		return nil, errs.New(errs.Usage, "gsfid 用 PlayStoreGSFID")
	}
	if _, err := m.playStoreCheck(i, action); err != nil {
		return nil, err
	}
	lock, err := m.playStoreLock(i)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()

	res := &PlayStoreResult{Index: i, Action: action, Files: []string{}, Warnings: []string{}}
	p, wasRunning, err := playStoreFindQemu(m, i)
	if err != nil {
		return nil, err
	}
	windowed := false
	if wasRunning {
		var known bool
		windowed, known = qemuWindowed(p)
		if nativePlayStore() && action != "status" && !known {
			return nil, errs.Newf(errs.PrecheckFailed, "无法确认实例 %d 原来的窗口模式，暂不冷启动", i)
		}
	}
	if action == "status" {
		if !wasRunning {
			return nil, playStoreNotRunning(i)
		}
		return res, m.playStoreStatus(ctx, i, res)
	}

	// Native ARM64 has no execution bridge. A separate persistent marker requests
	// -writable-system on every boot until removal is confirmed.
	if nativePlayStore() {
		if err := m.writePlayStoreMarker(i); err != nil {
			return nil, err
		}
		if playStoreRecord(m.Root.InstanceMeta(i)) == "" {
			if err := m.setPlayStoreRecord(i, jsonord.NewString(playStorePartial)); err != nil {
				return nil, err
			}
		}
	}

	warn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		m.Log.Warnf("%s", msg)
		res.Warnings = append(res.Warnings, msg)
	}
	// ours: the last start was this call's (one that is busy found another start under way, not to be stopped)
	ours := false
	active := wasRunning
	changed := false
	boot := func() error {
		sr, err := playStoreStart(m, ctx, i, StartOptions{Window: windowed, bridgeLocked: true})
		ours = errs.CodeOf(err) != errs.Busy
		changed = changed || ours
		active = err == nil
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
		active = false
		changed = true
		res.Restarted = true
		return boot()
	}
	// fail ends a failed install or uninstall: an instance that was off is shut down again, even after a cancel (best
	// effort; what stands in the way goes into the warnings and the log, the error stays the one that failed).
	fail := func(err error) (*PlayStoreResult, error) {
		if wasRunning {
			if nativePlayStore() && changed && !active {
				sr, restoreErr := playStoreStart(m, context.WithoutCancel(ctx), i, StartOptions{Window: windowed, bridgeLocked: true})
				if restoreErr != nil {
					warn("实例 %d 原来是开着的，没做成后也没能恢复开机：%s", i, errs.As(restoreErr).Error())
				} else {
					res.Warnings = append(res.Warnings, sr.Warnings...)
				}
			}
			return res, err
		}
		if !ours {
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
	} else if nativePlayStore() {
		// The existing boot may have no writable flag. Always cold boot through
		// our launch plan before changing the native system overlay.
		if err := restart(); err != nil {
			return fail(err)
		}
	}

	runChange := func(action string) (playStoreRun, error) {
		run, err := playStoreScript(m, ctx, i, action)
		// First remount can only disable verity. One cold boot makes that change
		// effective; a second failure is returned instead of a reboot loop.
		if nativePlayStore() && err == nil && run.Result == playStoreNotWritable {
			if err := restart(); err != nil {
				return run, err
			}
			return playStoreScript(m, ctx, i, action)
		}
		return run, err
	}
	switch action {
	case "install":
		run, err := runChange("install")
		if err == nil && run.Result != playStoreInstalled {
			err = playStoreFailed(i, "install", run)
		}
		if err != nil {
			if run.Result == playStorePartial || run.Ran && run.Result == "" {
				// some of it may be in the overlay: recorded, so 移除 is offered and bridge uninstall refuses
				res.State = playStorePartial
				if werr := m.setPlayStoreRecord(i, jsonord.NewString(playStorePartial)); werr != nil {
					warn("实例 %d 记不下 Google Play 商店只装了一部分：%s", i, errs.As(werr).Error())
				}
			}
			if wasRunning && slices.Contains(run.Lines, playStoreUndoneAtBoot) {
				warn("实例 %d 撤回的改动有一部分要关机再开机后才生效", i)
			}
			return fail(err)
		}
		// recorded once it is in (bridge uninstall refuses from then on)
		if err := m.setPlayStoreWanted(i, true); err != nil {
			return fail(err)
		}
		if err := restart(); err != nil {
			return fail(err)
		}
	case "uninstall":
		run, err := runChange("uninstall")
		if err != nil {
			return fail(err)
		}
		if run.Result == playStoreReboot {
			if err := restart(); err != nil {
				return fail(err)
			}
			if run, err = playStoreScript(m, ctx, i, "uninstall"); err != nil {
				return fail(err)
			}
		}
		if run.Result != playStoreGone {
			return fail(playStoreFailed(i, "uninstall", run))
		}
	}
	if err := m.playStoreStatus(ctx, i, res); err != nil {
		return fail(err)
	}
	if action == "install" && res.State != playStoreInstalled {
		if err := m.setPlayStoreRecord(i, jsonord.NewString(playStorePartial)); err != nil {
			warn("实例 %d 记不下 Google Play 商店的未完成状态：%s", i, errs.As(err).Error())
		}
		return fail(playStoreFailed(i, "install", playStoreRun{Result: res.State}))
	}
	if action == "uninstall" {
		if res.State != playStoreAbsent {
			return fail(playStoreFailed(i, "uninstall", playStoreRun{Result: res.State}))
		}
		if err := m.setPlayStoreWanted(i, false); err != nil {
			return fail(err)
		}
		if nativePlayStore() {
			if err := m.removePlayStoreMarker(i); err != nil {
				return fail(err)
			}
			if wasRunning {
				if err := restart(); err != nil {
					return fail(err)
				}
			}
		}
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

// PlayStoreGSFID reads instance i's GSF ID (`modavd playstore <i> gsfid`): the guest script's gsfid, the documentation
// site's query of GSF's database. The instance must be running; GSF without an ID yet is errs.NotReady with
// PlayStoreReasonNoGSFID (wait online a minute or two). Read only, but under the bridge lock like the store's other
// actions.
func (m *Manager) PlayStoreGSFID(ctx context.Context, i int) (*PlayStoreGSFIDResult, error) {
	if _, err := m.playStoreCheck(i, "gsfid"); err != nil {
		return nil, err
	}
	lock, err := m.playStoreLock(i)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	_, running, err := playStoreFindQemu(m, i)
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, playStoreNotRunning(i)
	}
	run, err := playStoreScript(m, ctx, i, "gsfid")
	if err != nil {
		return nil, err
	}
	return parseGSFID(i, run)
}

// playStoreStatus fills res from the guest script's status.
func (m *Manager) playStoreStatus(ctx context.Context, i int, res *PlayStoreResult) error {
	run, err := playStoreScript(m, ctx, i, "status")
	if err != nil {
		return err
	}
	switch run.Result {
	case playStoreInstalled, playStorePartial, playStoreAbsent:
	default:
		return playStoreFailed(i, "status", run)
	}
	res.State, res.Installed, res.Files = run.Result, run.Result == playStoreInstalled, fileLines(run.Lines)
	return nil
}
