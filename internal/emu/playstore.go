package emu

// The Google Play Store (ModAVD/ModAVD#125, package playstore). Off unless asked for: `modavd playstore <i> install`,
// the manager's 「补全 Google Play 商店」 on an instance; new instances never get it. Its five files
// (root.PlayStoreComponent, sdk\playstore) come from the root, put there by `components fetch --play-store` or
// `components import-playstore` (package components); they are pushed into the guest, where the guest script
// (guest/scripts/playstore.sh) checks them against the pinned SHA-256 and copies them into the instance's writable
// system overlay, hiding the image's own placeholder com.android.vending. That overlay exists only on instances with
// the execution bridge (its boot marker makes the emulator boot writable, bridge.go), so the store needs the bridge.
// The files take effect at the next boot: an install boots the instance cold once.
//
// Uninstalling takes two runs of the script with a cold boot between: the first disables the store (so it cannot
// update itself again), removes the updates Google Play installed for the store, Play services, GSF and Partner Setup
// and deletes the files from the overlay's upper layer; the second, after the boot, enables com.android.vending again,
// by then the image's placeholder.

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
	"modavd/internal/playstore"
	"modavd/internal/root"
)

const (
	// playStoreMetaKey is instances.<i>.playStore in mas-avd.json: true once playstore install put the store in, false
	// after playstore uninstall. Absent on every instance that never had it.
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
)

// Values of errs.Reason on the store's refusals, for callers that offer the way out (the manager: 补全组件 for the
// files, the bridge for the rest).
const (
	PlayStoreReasonFiles       = "playstore_files_missing" // errs.NotReady: the root has not got the five files (or they are damaged)
	PlayStoreReasonNoBridge    = "no_bridge"               // errs.NotReady: the instance does not boot with a writable system
	PlayStoreReasonUnsupported = "unsupported_guest"       // errs.Unsupported: an arm64 guest
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

// playStoreRun is one run of the guest script.
type playStoreRun struct {
	Result string   // the word after RESULT, "" when the script did not get there
	Lines  []string // its other output lines
}

// Hooks for the tests (PlayStore without an emulator).
var (
	playStoreFindQemu = (*Manager).findQemu
	playStoreStart    = (*Manager).Start
	playStoreStop     = func(m *Manager, ctx context.Context, i int) ([]string, error) {
		sr, err := m.Stop(ctx, i, StopOptions{})
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

// playStoreWanted: instances.<i>.playStore is true.
func (m *Manager) playStoreWanted(i int) bool {
	meta := m.Root.InstanceMeta(i)
	if meta == nil {
		return false
	}
	v := meta.Get(playStoreMetaKey)
	if v == nil {
		return false
	}
	b, ok := v.BoolVal()
	return ok && b
}

func (m *Manager) setPlayStoreWanted(i int, on bool) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(playStoreMetaKey, jsonord.NewBool(on))
		return nil
	})
}

// playStoreBridgeError is nil when instance i boots with a writable system (it has the bridge and its marker).
func (m *Manager) playStoreBridgeError(i int, cfg *avd.Ini) error {
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

// PlayStoreCheck is what PlayStore checks before it touches anything, without a lock or a boot: the host, the
// action, the instance (there, not being created) and, for install, a writable system (the bridge) and the root's
// five files (their pinned SHA-256). The manager calls it before it starts the job, so the button hears at once.
func (m *Manager) PlayStoreCheck(i int, action string) error {
	_, err := m.playStoreCheck(i, action)
	return err
}

func (m *Manager) playStoreCheck(i int, action string) (*avd.Instance, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	switch action {
	case "install", "uninstall", "status":
	default:
		return nil, errs.Newf(errs.Usage, "只能是 install、uninstall 或 status，收到「%s」", action)
	}
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	if err := m.notCreated(i, "不能装卸 Google Play 商店"); err != nil {
		return nil, err
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
	if err := os.WriteFile(list, []byte(playstore.Manifest()), 0o644); err != nil {
		return playStoreRun{}, errs.Wrap(errs.Internal, err, "写不了文件清单")
	}
	script, err := guest.LocalFile(filepath.Join(os.TempDir(), "modavd-guest"), guest.PlayStore)
	if err != nil {
		return playStoreRun{}, errs.Wrap(errs.Internal, err, "写不了客体脚本 "+guest.PlayStore)
	}
	pushes := []string{script, list}
	if action == "install" {
		for _, f := range playstore.Files {
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
	res := sh(fmt.Sprintf("su 0 sh %s/%s %s %s", playStoreGuestDir, guest.PlayStore, action, playStoreGuestDir), playStoreScriptTimeout)
	run := parsePlayStoreRun(res.Out)
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

// fileLines are the lines of a status about the files and the placeholder.
func fileLines(lines []string) []string {
	out := []string{}
	for _, l := range lines {
		for _, p := range []string{"play ", "image ", "absent ", "stub ", "package "} {
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
	if len(run.Lines) > 0 {
		msg += "：" + run.Lines[len(run.Lines)-1]
	}
	if run.Result == playStoreNotWritable {
		e := errs.New(errs.NotReady, msg).WithAdvice(fmt.Sprintf("实例的系统镜像不可写：先运行 modavd bridge %d install，再试一次", i))
		return errs.WithDetail(e, errs.Reason, PlayStoreReasonNoBridge)
	}
	return errs.New(errs.LaunchFailed, msg)
}

// PlayStore installs, uninstalls or checks (status) the Google Play Store on instance i. install and uninstall boot an
// instance that is off and stop it again; their cold boots bring a running instance back the way it ran (window or
// headless). status needs it running. Everything PlayStoreCheck refuses is refused before the instance is touched.
func (m *Manager) PlayStore(ctx context.Context, i int, action string) (*PlayStoreResult, error) {
	if _, err := m.playStoreCheck(i, action); err != nil {
		return nil, err
	}
	// the bridge lock: the store lives in the bridge's overlay, and holding it makes status say busy (bridging), keeps
	// delete, compact and bridge actions out meanwhile; the starts below run the bridge step under it (bridgeLocked)
	lock, ok, err := m.tryOperationLock(i, "bridge")
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errs.WithDetails(errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正在装卸执行桥或 Google Play 商店，稍后再试", i),
			map[string]string{errs.Reason: "locked", errs.Lock: "bridge"})
	}
	defer lock.Unlock()

	res := &PlayStoreResult{Index: i, Action: action, Files: []string{}, Warnings: []string{}}
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
			return nil, errs.Newf(errs.Unsupported, "实例 %d 没开机", i).WithAdvice(fmt.Sprintf("先 modavd start %d", i))
		}
		return res, m.playStoreStatus(ctx, i, res)
	}

	boot := func() error {
		sr, err := playStoreStart(m, ctx, i, StartOptions{Window: windowed, bridgeLocked: true})
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
	if !wasRunning {
		if err := boot(); err != nil {
			return res, err
		}
	}

	switch action {
	case "install":
		run, err := playStoreScript(m, ctx, i, "install")
		if err != nil {
			return res, err
		}
		if run.Result != playStoreInstalled {
			return res, playStoreFailed(i, "install", run)
		}
		// recorded once it is in (bridge uninstall refuses from then on)
		if err := m.setPlayStoreWanted(i, true); err != nil {
			return res, err
		}
		if err := restart(); err != nil {
			return res, err
		}
	case "uninstall":
		run, err := playStoreScript(m, ctx, i, "uninstall")
		if err != nil {
			return res, err
		}
		if run.Result == playStoreReboot {
			if err := restart(); err != nil {
				return res, err
			}
			if run, err = playStoreScript(m, ctx, i, "uninstall"); err != nil {
				return res, err
			}
		}
		if run.Result != playStoreGone {
			return res, playStoreFailed(i, "uninstall", run)
		}
		if err := m.setPlayStoreWanted(i, false); err != nil {
			return res, err
		}
	}
	if err := m.playStoreStatus(ctx, i, res); err != nil {
		return res, err
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
