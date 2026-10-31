//go:build windows

package emu

// A new instance is set up before create returns (10-30): one headless boot that runs the per-boot setup, the host
// IME, the first-boot setup (firstboot.go) and the bridge install with its cold boots (bridge.go; skipped with
// --no-bridge), then a normal shutdown (sync first) and a wait until qemu is gone. Its first start afterwards is as
// fast as any other. Until the setup is done the instance is "creating": status / list / info say so; start, set,
// bridge, delete and create on its index are refused as busy. A setup that fails leaves it "create_failed" with the
// reason and that boot's log; `create --resume <i>` runs the setup again, delete removes it. Instances created
// before 0.2.0 have no creation state and keep doing that setup on their first start.
//
// State: instances.<i>.creationState ("creating" / "failed"), creationError, creationLog in mas-avd.json (removed
// once the setup is done), and the lock .mas_<i>.create.lock, held for the whole setup. "creating" without anybody
// holding that lock is a create that was killed half way: it reads as failed.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/win"
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
	createStart = (*Manager).Start
	createStop  = (*Manager).stopAndWait
	createCheck = (*Manager).checkCreated
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
		return errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正在新建（后台开机做初始化），%s", i, what).
			WithAdvice("等它做完；进度用 modavd status 查看")
	case creationFailed:
		advice := fmt.Sprintf("用 modavd create --resume %d 重新初始化，或者 modavd delete %d --yes 删掉", i, i)
		if logFile != "" {
			advice += "；日志见 " + logFile
		}
		if message == "" {
			message = "初始化没做完"
		}
		// the reason last: a setup error has its own brackets
		return errs.Newf(errs.NotReady, "魔改 AVD 实例 %d 新建时初始化失败，%s：%s", i, what, message).WithAdvice(advice)
	}
	return nil
}

// createProgress maps the steps of the setup's boots onto one 0–1000 bar: the first boot to 20–700, each bridge
// round (a cold boot) to the next slice up to 900, the shutdown 930, done 1000. Never goes back.
type createProgress struct {
	fn    func(Progress)
	phase int // 0 before the first boot, 1 the first boot, 2+ the bridge rounds
	last  int
}

func (p *createProgress) emit(step, msg string, done int) {
	if p.fn == nil {
		return
	}
	if done <= p.last && step != "done" {
		done = p.last + 1
	}
	if done > progressTotal {
		done = progressTotal
	}
	p.last = done
	p.fn(Progress{Step: step, Message: msg, Done: done, Total: progressTotal})
}

// boot is the Progress of the boots inside the setup.
func (p *createProgress) boot(ev Progress) {
	switch {
	case ev.Step == "bridge": // a bridge round: its cold boot follows
		p.phase = max(p.phase, 1) + 1
	case p.phase == 0:
		p.phase = 1
	}
	lo, hi := 20, 700
	if p.phase >= 2 {
		lo = min(700+(p.phase-2)*70, 860)
		hi = min(lo+70, 900)
	}
	step := ev.Step
	if step == "done" {
		step = "setup"
	}
	p.emit(step, ev.Message, lo+(hi-lo)*max(0, min(ev.Done, progressTotal))/progressTotal)
}

// setupNew is the setup of a new instance (or a retry of one that failed); the create lock is held by the caller and
// released here.
func (m *Manager) setupNew(ctx context.Context, i int, req CreateRequest, lock *win.FileLock) (*InstanceInfo, error) {
	defer lock.Unlock()
	if err := m.setCreation(i, creationCreating, "", ""); err != nil {
		return nil, err
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = DefaultCreateTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	prog := &createProgress{fn: req.Progress}
	bridge := m.bridgeWanted(i)
	what := "开机设置、宿主输入法、首次初始化"
	if bridge {
		what += "、arm64 执行桥"
	}
	prog.emit("create", fmt.Sprintf("实例 %d：后台无头开机，做完%s后正常关机（一般几分钟）", i, what), 20)
	m.Log.Infof("实例 %d 新建后的初始化：后台无头开机，做完%s后正常关机", i, what)

	sr, err := createStart(m, cctx, i, StartOptions{creating: true, NoBridge: !bridge, Progress: prog.boot})
	if err == nil {
		err = createCheck(m, cctx, i, bridge, sr)
	}
	// Shut down whatever happened (a failed or timed-out boot leaves qemu running by design): the instance must end
	// stopped, normally, sync first; on its own deadline, since the setup's may be over.
	prog.emit("create_stop", fmt.Sprintf("实例 %d 初始化%s，先 sync 再正常关机", i, map[bool]string{true: "做完", false: "没做完"}[err == nil]), 930)
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), createStopTimeout)
	stopErr := createStop(m, sctx, i)
	scancel()
	if err == nil && stopErr != nil {
		err = errs.Wrap(errs.As(stopErr).Code, stopErr, fmt.Sprintf("实例 %d 初始化做完了，但没能正常关机: %s", i, errs.As(stopErr).Message))
	}
	if err != nil {
		switch {
		case ctx.Err() == nil && cctx.Err() == context.DeadlineExceeded:
			err = errs.Wrap(errs.Timeout, err, fmt.Sprintf("实例 %d 新建后的初始化 %.0f 分钟没做完", i, timeout.Minutes()))
		case ctx.Err() != nil && errs.CodeOf(err) != errs.Canceled:
			err = errs.Wrap(errs.Canceled, err, "已取消")
		}
		logFile := ""
		if sr != nil {
			logFile = sr.LogFile
		}
		if logFile == "" {
			logFile = m.newestEmulatorLog(i)
		}
		e := errs.As(err)
		if serr := m.setCreation(i, creationFailed, e.Message, logFile); serr != nil {
			m.Log.Warnf("实例 %d 没记下「新建失败」: %v", i, serr)
		}
		advice := fmt.Sprintf("用 modavd create --resume %d 重新初始化，或者 modavd delete %d --yes 删掉", i, i)
		if logFile != "" {
			advice += "；日志见 " + logFile
		}
		return nil, &errs.Error{Code: e.Code, Message: fmt.Sprintf("魔改 AVD 实例 %d 新建失败：%s", i, e.Message),
			Advice: advice, Cause: err}
	}
	if err := m.setCreation(i, "", "", ""); err != nil {
		return nil, err
	}
	prog.emit("done", fmt.Sprintf("实例 %d 新建完成，已关机；第一次开机不用再做初始化", i), progressTotal)
	m.Log.Infof("实例 %d 新建完成", i)
	return m.Info(ctx, i)
}

// checkCreated: what the setup's boot was for is all there, read from the running guest before it shuts down.
func (m *Manager) checkCreated(ctx context.Context, i int, bridge bool, sr *StartResult) error {
	var missing []string
	if meta := m.Root.InstanceMeta(i); meta == nil || !metaTruthy(meta.Get("initialized")) {
		missing = append(missing, "首次初始化")
	}
	if !sr.HostIME {
		missing = append(missing, "宿主输入法")
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
	if _, running := m.findQemu(i); !running {
		return nil
	}
	if _, err := m.Stop(ctx, i, StopOptions{}); err != nil {
		return err
	}
	return m.waitQemuGone(ctx, i)
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
