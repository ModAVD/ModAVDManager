package emu

// A new instance is set up before create returns (10-30): one headless boot that runs the per-boot setup, the host
// IME, the first-boot setup (firstboot.go) and the bridge install with its cold boots (bridge.go; skipped with
// --no-bridge), then a normal shutdown (sync first) and a wait until qemu is gone. Its first start afterwards is as
// fast as any other. Until the setup is done the instance is "creating": status / list / info say so; start, set,
// bridge, delete and create on its index are refused as busy. A setup that fails leaves it "create_failed" with the
// reason and that boot's log; `create --resume <i>` runs the setup again (shutting down normally an emulator the
// failed one left running), delete removes it. Instances created
// before 0.2.0 have no creation state and keep doing that setup on their first start.
//
// State: instances.<i>.creationState ("creating" / "failed"), creationError, creationLog in mas-avd.json (removed
// once the setup is done; "creating" is written together with the instance's other keys, before it is listed), and
// the lock .mas_<i>.create.lock, held for the whole setup. "creating" without anybody holding that lock is a create
// that was killed half way: it reads as failed. While it runs, the setup's percentage (createProgress, by the steps
// done) is in .mas_<i>.create.progress, which status / list read as creation_progress.
// An explicit stop cancellation uses .mas_<i>.create.cancel to notify the creator across processes (createcancel.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

const (
	creationCreating = "creating"
	creationFailed   = "failed"

	// DefaultCreateTimeout bounds create's whole setup (create --timeout): three boots, the bridge script, a shutdown.
	DefaultCreateTimeout = 20 * time.Minute
	// createStopTimeout bounds the shutdown at the end of the setup, also after the setup's own deadline passed.
	createStopTimeout = 3 * time.Minute

	interruptedCreation = "新建它的程序中途退出了"
)

// Hooks for the tests (a setup without an emulator).
var (
	createStart   = (*Manager).Start
	createStop    = (*Manager).stopAndWait
	createCheck   = (*Manager).checkCreated
	createRunning = func(m *Manager, i int) (bool, error) { _, ok, err := m.findQemu(i); return ok, err }
)

// creation is the creation state of instance i: "" (ready, or made before 0.2.0), "creating" or "failed", with the
// failure's message and log. A "creating" nobody works on (its lock is free) is a create that was killed: failed.
func (m *Manager) creation(i int) (state, message, logFile string) {
	meta := m.Root.InstanceMeta(i)
	if meta == nil {
		return "", "", ""
	}
	state = metaText(meta.Get("creationState"))
	message = metaText(meta.Get("creationError"))
	logFile = metaText(meta.Get("creationLog"))
	switch state {
	case creationCreating:
		if !m.lockHeld(i, "create") {
			return creationFailed, interruptedCreation, logFile
		}
		return state, "", ""
	case creationFailed:
		return state, message, logFile
	}
	return "", "", ""
}

// creationRecorded is instances.<i>.creationState as written, without asking who holds the lock.
func (m *Manager) creationRecorded(i int) string {
	if meta := m.Root.InstanceMeta(i); meta != nil {
		return metaText(meta.Get("creationState"))
	}
	return ""
}

func (m *Manager) setCreation(i int, state, message, logFile string) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		for _, k := range []string{"creationState", "creationError", "creationLog"} {
			meta.Delete(k)
		}
		if state == "" {
			return nil
		}
		meta.Set("creationState", jsonord.NewString(state))
		if message != "" {
			meta.Set("creationError", jsonord.NewString(message))
		}
		if logFile != "" {
			meta.Set("creationLog", jsonord.NewString(logFile))
		}
		return nil
	})
}

// notCreated is the error of an operation on an instance whose setup is running or failed ("" state: nil).
func (m *Manager) notCreated(i int, what string) error {
	state, message, logFile := m.creation(i)
	switch state {
	case creationCreating:
		return errs.WithDetails(errs.Newf(errs.Busy, "ModAVD 实例 %d 正在新建（后台开机做初始化），%s", i, what).
			WithAdvice("等它做完；进度用 modavd status 查看"), map[string]string{errs.Reason: "locked", errs.Lock: "create"})
	case creationFailed:
		advice := fmt.Sprintf("用 modavd create --resume %d 重新初始化，或者 modavd delete %d --yes 删掉", i, i)
		if logFile != "" {
			advice += "；日志见 " + logFile
		}
		if message == "" {
			message = "初始化没做完"
		}
		// the reason last: a setup error has its own brackets
		return errs.Newf(errs.NotReady, "ModAVD 实例 %d 新建时初始化失败，%s：%s", i, what, message).WithAdvice(advice)
	}
	return nil
}

// createStage is one stretch of the setup: from the Progress step that starts it to the one that starts the next.
type createStage struct {
	step   string
	weight float64 // the seconds it usually takes; only the ratios matter
}

// What the stages of the setup usually take, in seconds. Measured on real creates (11-02: two with --no-bridge;
// 11-03: one with --no-bridge, one with the bridge): the first boot of a new instance 25–45 s, a cold boot 16–18 s,
// the tuning 1–2 s, the host IME and the steps after it 2–3 s, the first-boot setup 3 s, the guest Vulkan fix 13–14 s
// (most of it waiting for the framework after the zygote restart), the shutdown 5–8 s. The bridge on a new instance
// takes two rounds with a cold boot each, then a check: the first remount turns verity off (27 s with its
// shutdown), the install (12 s with its shutdown), the check that finds it installed (1 s, no boot).
const (
	stagePrepare     = 2.0  // the checks before the launch (and starting the private adb server)
	stageBoot        = 35.0 // from the launch until Android has booted, the first boot of a new instance
	stageColdBoot    = 17.0 // the same for a later boot (a bridge round's, a resume's)
	stageTune        = 1.5
	stageIme         = 2.5
	stageFirstBoot   = 3.0
	stageVulkanFix   = 13.5
	stageRemount     = 27.0 // the bridge's first round: the first remount and the shutdown
	stageBridge      = 12.0 // its second: the install and the shutdown
	stageBridgeCheck = 1.0  // its last: the check that finds it installed
	stageShutdown    = 6.0
)

// createPlan is the stages of a setup: one boot (boot is its wait; with the first-boot setup unless the instance is
// initialized), the bridge rounds of a new instance when the bridge is installed, the shutdown. An instance whose
// first remount is done (a resume) needs one round fewer: the shutdown passes over the rest.
func createPlan(boot float64, firstBoot, bridge bool) []createStage {
	plan := append([]createStage{{"create", stagePrepare}}, bootStages(boot, firstBoot)...)
	if bridge {
		plan = append(plan, bridgeRound(stageRemount)...)
		plan = append(plan, bridgeRound(stageBridge)...)
		plan = append(plan, createStage{"bridge", stageBridgeCheck})
	}
	return append(plan, createStage{"create_stop", stageShutdown})
}

// bootStages are the stages of one boot of the setup, by the steps Start reports; boot is the wait for Android.
func bootStages(boot float64, firstBoot bool) []createStage {
	s := []createStage{{"launch", boot}, {"tune", stageTune}, {"ime", stageIme}}
	if firstBoot {
		s = append(s, createStage{"first_boot", stageFirstBoot})
	}
	return append(s, createStage{"vulkan_fix", stageVulkanFix})
}

// bridgeRound is a round of the bridge install that changes the guest: the script (and the shutdown), then a cold
// boot.
func bridgeRound(script float64) []createStage {
	return append([]createStage{{"bridge", script}}, bootStages(stageColdBoot, false)...)
}

// bootMilestones are lines the emulator writes to its log while Android boots, in that order, with the share of the
// boot wait that has passed when each appears. Measured 11-03 on the first boot of a new instance and two cold boots:
// 0.24 / 0.11 / 0.12 for the first two, 0.43 / 0.54 / 0.56, 0.65 / 0.58 / 0.61. The emulator's "Initialized handle"
// only comes after Android has booted.
var bootMilestones = []struct {
	text  string
	share float64
}{
	{" is operational", 0.1},                       // the accelerator (WHPX ...): qemu's machine is up
	{"Advertising in:", 0.15},                      // the emulator's console and gRPC are up
	{"qemu_win32_discard_memory", 0.45},            // the guest kernel reports free pages to the balloon
	{"driver prefers a dedicated allocation", 0.6}, // the guest's first screen buffer (only some GPU drivers say so)
	{"Boot completed in", 0.95},                    // Android says it has booted
}

// bootLogShare is the share of the boot wait the emulator log at path shows done (0 when unreadable). Only the
// start of the log is read: the milestones come long before the log grows large.
func bootLogShare(path string) float64 {
	if path == "" {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 512<<10))
	if err != nil {
		return 0
	}
	share := 0.0
	for _, ms := range bootMilestones {
		if bytes.Contains(b, []byte(ms.text)) {
			share = max(share, ms.share)
		}
	}
	return share
}

// createProgress turns the Progress of the setup's boots into one bar by what has been done, never by the clock
// (its own steps besides Start's: create, setup at the end of each boot, bridge, create_stop, done):
// the setup is a list of stages (createPlan) weighted by what they usually take, and the step that starts a stage
// completes every stage before it. Within the boot wait the emulator's log tells how far the boot got
// (bootMilestones). A planned stage that does not happen counts as done once a later one starts; work the plan does
// not have (a bridge round more than planned) leaves the bar where it is. It never goes back, and it stays below
// 100 % until the setup is done. Done is per mille of progressTotal.
type createProgress struct {
	i       int
	fn      func(Progress)
	bootLog func() string // the emulator log of the boot under way ("" when unknown)
	plan    []createStage
	pos     int     // the stage under way; len(plan) when all are done
	part    float64 // the share of plan[pos] done (the boot wait only)
}

// next is the index of the first stage from pos on whose step is one of steps, -1 when there is none.
func (p *createProgress) next(steps ...string) int {
	for k := p.pos; k < len(p.plan); k++ {
		if slices.Contains(steps, p.plan[k].step) {
			return k
		}
	}
	return -1
}

// advance makes stage k the one under way (every stage before it done).
func (p *createProgress) advance(k int) {
	if k > p.pos {
		p.pos, p.part = k, 0
	}
}

// done is how much of the plan is done, per mille.
func (p *createProgress) done() int {
	var done, total float64
	for k, s := range p.plan {
		total += s.weight
		if k < p.pos {
			done += s.weight
		}
	}
	if p.pos < len(p.plan) {
		done += p.part * p.plan[p.pos].weight
	}
	if total == 0 {
		return 0
	}
	return int(float64(progressTotal) * done / total)
}

// stage reports that step of the setup itself started ("create", "create_stop", "done").
func (p *createProgress) stage(step, msg string) {
	if step == "done" {
		p.pos = len(p.plan)
	} else if k := p.next(step); k >= 0 {
		p.advance(k)
	}
	p.send(step, msg)
}

// boot is the Progress of the boots inside the setup.
func (p *createProgress) boot(ev Progress) {
	step, msg := ev.Step, ev.Message
	switch step {
	case "boot":
		// the boot wait (Start counts it by the clock) or its end (the tuning that follows starts the next stage)
		if ev.Done < progressBooted && p.pos < len(p.plan) && p.plan[p.pos].step == "launch" && p.bootLog != nil {
			p.part = max(p.part, bootLogShare(p.bootLog()))
		}
	case "done":
		// one boot's setup is over: so is everything before the next bridge round or the shutdown. Not "ready":
		// the instance is still being created.
		step, msg = "setup", fmt.Sprintf("实例 %d 这次开机后的设置做完了", p.i)
		if k := p.next("bridge", "create_stop"); k >= 0 {
			p.advance(k)
		}
	default:
		if k := p.next(step); k >= 0 {
			p.advance(k)
		}
	}
	p.send(step, msg)
}

// send reports the bar. It never goes back (stages only advance, the boot milestones only add up) and stays below
// 100 % until done: the shutdown is the last stage, and only done completes it.
func (p *createProgress) send(step, msg string) {
	done := p.done()
	if step == "done" {
		done = progressTotal
	}
	if p.fn != nil {
		p.fn(Progress{Step: step, Message: msg, Done: done, Total: progressTotal})
	}
}

// setupNew is the setup of a new instance (or a retry of one that failed); the create lock is held by the caller and
// released here. cfg is the instance's config.ini as the caller read it.
func (m *Manager) setupNew(ctx context.Context, i int, cfg *avd.Ini, req CreateRequest, lock *host.FileLock) (_ *InstanceInfo, retErr error) {
	defer lock.Unlock()
	defer func() {
		if retErr != nil {
			retErr = m.withCreateErrorState(i, retErr, true)
		}
	}()
	if err := m.setCreation(i, creationCreating, "", ""); err != nil {
		return nil, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultCreateTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	canceled := m.watchCreateCancel(cctx, cancel, i)
	defer func() {
		cancel()
		<-canceled
		if err := os.Remove(m.createCancelPath(i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			m.Log.Warnf("实例 %d 没清理新建取消标记: %v", i, err)
		}
	}()
	// Publish the attempt before any qemu probe / leftover shutdown, so stop --all
	// can find a resume even with no qemu. Before this point the new attempt is
	// not published; target discovery is a snapshot, not a barrier to future creates.
	if req.Resume {
		if err := m.stopCreateLeftover(ctx, i); err != nil {
			m.recordCreationFailure(i, err, m.newestEmulatorLog(i))
			return nil, err
		}
	}
	bridge := m.bridgeWanted(i, cfg)
	// the progress is also kept on disk, for status / list in other processes (AUTO-MAS, the manager)
	defer m.removeCreateProgress(i)
	boot := stageBoot
	if req.Resume {
		boot = stageColdBoot // the instance has booted before
	}
	prog := &createProgress{i: i, plan: createPlan(boot, !m.initialized(i), bridge),
		bootLog: func() string {
			if mk, ok := m.readSetupMarker(i); ok {
				return mk.LogFile // written at the launch, before the boot wait
			}
			return ""
		},
		fn: func(ev Progress) {
			m.writeCreateProgress(i, ev)
			if req.Progress != nil {
				req.Progress(ev)
			}
		}}
	what := "开机设置、宿主输入法、首次初始化"
	if bridge {
		what += "、arm64 执行桥"
	}
	prog.stage("create", fmt.Sprintf("实例 %d：后台无头开机，做完%s后正常关机（一般几分钟）", i, what))
	m.Log.Infof("实例 %d 新建后的初始化：后台无头开机，做完%s后正常关机", i, what)

	sr, err := createStart(m, cctx, i, StartOptions{creating: true, NoBridge: !bridge, Progress: prog.boot})
	if err == nil {
		err = createCheck(m, cctx, i, bridge, sr)
	}
	// Shut down whatever happened (a failed or timed-out boot leaves qemu running by design): the instance must end
	// stopped, normally, sync first; on its own deadline, since the setup's may be over.
	if err == nil {
		prog.stage("create_stop", fmt.Sprintf("实例 %d 初始化做完，先 sync 再正常关机", i))
	} else {
		// what the failed setup did not get to was not done: the bar stays where it got to
		prog.send("create_stop", fmt.Sprintf("实例 %d 初始化没做完，先 sync 再正常关机", i))
	}
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), createStopTimeout)
	stopErr := createStop(m, sctx, i)
	scancel()
	if err == nil && stopErr != nil {
		err = errs.Wrap(errs.As(stopErr).Code, stopErr, fmt.Sprintf("实例 %d 初始化做完了，但没能正常关机: %s", i, errs.As(stopErr).Message))
	}
	if err == nil {
		err = m.finishCreation(cctx, i)
	}
	if err != nil {
		switch {
		case ctx.Err() == nil && cctx.Err() == context.DeadlineExceeded:
			err = errs.Wrap(errs.Timeout, err, fmt.Sprintf("实例 %d 新建后的初始化 %.0f 分钟没做完", i, timeout.Minutes()))
		case ctx.Err() != nil && errs.CodeOf(err) != errs.Canceled:
			err = errs.Wrap(errs.Canceled, err, "已取消")
		}
		// Start (including a bridge restart) can say it left qemu running. The
		// cleanup above has since stopped it; persist the final state instead.
		if errs.CodeOf(err) == errs.Canceled && stopErr == nil {
			err = errs.Wrap(errs.Canceled, err, "新建初始化已取消，已关机")
		}
		logFile := ""
		if sr != nil {
			logFile = sr.LogFile
		}
		if logFile == "" {
			logFile = m.newestEmulatorLog(i)
		}
		e := errs.As(err)
		m.recordCreationFailure(i, err, logFile)
		advice := fmt.Sprintf("用 modavd create --resume %d 重新初始化，或者 modavd delete %d --yes 删掉", i, i)
		if logFile != "" {
			advice += "；日志见 " + logFile
		}
		return nil, &errs.Error{Code: e.Code, Message: fmt.Sprintf("ModAVD 实例 %d 新建失败：%s", i, e.Message),
			Advice: advice, Cause: err}
	}
	prog.stage("done", fmt.Sprintf("实例 %d 新建完成，已关机；第一次开机不用再做初始化", i))
	m.Log.Infof("实例 %d 新建完成", i)
	return m.Info(ctx, i)
}

// stopCreateLeftover ends the failed attempt's qemu before a resume boots again.
// The create watcher must not cancel this shutdown or trigger a second cleanup
// try. Use the caller's context, as before, rather than the watched setup context.
func (m *Manager) stopCreateLeftover(ctx context.Context, i int) error {
	running, err := createRunning(m, i)
	if err != nil {
		e := errs.As(err)
		return errs.Wrap(e.Code, err, e.Message).WithAdvice(e.Advice)
	}
	if !running {
		return nil
	}
	m.Log.Infof("实例 %d 上次新建时开的模拟器还开着，先正常关机再重新初始化", i)
	sctx, cancel := context.WithTimeout(ctx, createStopTimeout)
	defer cancel()
	if err := createStop(m, sctx, i); err != nil {
		e := errs.As(err)
		return errs.Wrap(e.Code, err, fmt.Sprintf("ModAVD 实例 %d 上次新建时开的模拟器没能正常关掉: %s", i, e.Message)).
			WithAdvice(fmt.Sprintf("用 modavd stop %d 关掉它，再重试", i))
	}
	return nil
}

func (m *Manager) recordCreationFailure(i int, err error, logFile string) {
	if serr := m.setCreation(i, creationFailed, errs.As(err).Message, logFile); serr != nil {
		m.Log.Warnf("实例 %d 没记下「新建失败」: %v", i, serr)
	}
}

// checkCreated: what the setup's boot was for is all there, read from the running guest before it shuts down.
func (m *Manager) checkCreated(ctx context.Context, i int, bridge bool, sr *StartResult) error {
	var missing []string
	if meta := m.Root.InstanceMeta(i); meta == nil || !metaTruthy(meta.Get("initialized")) {
		missing = append(missing, "首次初始化")
	}
	if !sr.HostIME {
		channel, _ := m.hostImeChannel(true, sr.LogFile)
		if root.CurrentPlatform().Windows() || channel {
			missing = append(missing, "宿主输入法")
		}
	}
	if bridge && m.guestBridgeWithin(ctx, i, statusAdbTimeout) != bridgeProp {
		missing = append(missing, fmt.Sprintf("arm64 执行桥（日志见 logs 里的 modavd-bridge-%d-*.log）", i))
	}
	if len(missing) > 0 {
		msg := "没做完：" + strings.Join(missing, "、")
		if len(sr.Warnings) > 0 {
			msg += "；" + strings.Join(sr.Warnings, "；")
		}
		return errs.New(errs.NotReady, msg)
	}
	return nil
}

// stopAndWait shuts instance i down normally (sync first; Stop's own fallbacks) and waits for its qemu to exit.
func (m *Manager) stopAndWait(ctx context.Context, i int) error {
	if running, err := createRunning(m, i); err != nil {
		return err
	} else if !running {
		return nil
	}
	if _, err := m.Stop(ctx, i, StopOptions{creating: true}); err != nil {
		return err
	}
	return m.waitQemuGone(ctx, i)
}

// createProgressFile is the content of createProgressPath.
type createProgressFile struct {
	Percent int    `json:"percent"`
	Step    string `json:"step"`
	Message string `json:"message"`
}

// createProgressPath is <root>\avd\.mas_<i>.create.progress: where the setup under way keeps its progress, for status
// / list in other processes. Written by the process holding the create lock only, removed when the setup ends.
func (m *Manager) createProgressPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".create.progress")
}

// writeCreateProgress replaces the progress file (temp file + rename, so a reader never sees half of it). A failure
// only costs other processes the percentage.
func (m *Manager) writeCreateProgress(i int, ev Progress) {
	b, err := json.Marshal(createProgressFile{Percent: ev.Percent(), Step: ev.Step, Message: ev.Message})
	if err == nil {
		err = writeFileReplace(m.createProgressPath(i), b, strconv.Itoa(os.Getpid()))
	}
	if err != nil {
		m.Log.Debugf("实例 %d 记不下新建进度: %v", i, err)
	}
}

func (m *Manager) removeCreateProgress(i int) {
	if err := os.Remove(m.createProgressPath(i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.Log.Debugf("实例 %d 删不掉新建进度文件: %v", i, err)
	}
}

// createPercent is the percentage the setup under way last wrote (0 when there is none or it cannot be read).
func (m *Manager) createPercent(i int) int {
	var f createProgressFile
	b, err := os.ReadFile(m.createProgressPath(i))
	if err != nil || json.Unmarshal(b, &f) != nil {
		return 0
	}
	return max(0, min(f.Percent, 100))
}

// newestEmulatorLog is the newest logs\mas_<i>-*.log ("" when none).
func (m *Manager) newestEmulatorLog(i int) string {
	files, err := managedLogFiles(m.Root.LogsDir())
	if err != nil {
		return ""
	}
	prefix := fmt.Sprintf("mas_%d-", i)
	best := logFile{}
	for _, f := range files {
		if strings.HasPrefix(f.name, prefix) && strings.HasSuffix(f.name, ".log") && f.mtime.After(best.mtime) {
			best = f
		}
	}
	if best.path == "" {
		return ""
	}
	return filepath.Clean(best.path)
}
