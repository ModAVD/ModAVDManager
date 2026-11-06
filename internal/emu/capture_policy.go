// Copyright 2026 Ji Mao Cai
package emu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

// captureState names both process identities. A matching adb command line alone could be collect-info's -d call.
type captureState struct {
	HelperPID     uint32
	HelperCreated uint64
	ClientPID     uint32
	ClientCreated uint64
	QemuPID       uint32
	QemuCreated   uint64
	ServerPID     uint32
	ServerCreated uint64
	Enabled       bool
	Since         string
}

func (m *Manager) captureStatePath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".logcat-state.json")
}

func (m *Manager) readCaptureState(i int) (captureState, error) {
	var state captureState
	b, err := root.ReadFileShared(m.captureStatePath(i))
	if err == nil {
		err = json.Unmarshal(b, &state)
	}
	return state, err
}

func (m *Manager) writeCaptureState(i int, state captureState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	path := m.captureStatePath(i)
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) ownsCaptureHelper(i int, state captureState) bool {
	helper := win.Process{PID: state.HelperPID, Created: state.HelperCreated}
	if helper.PID == 0 || helper.Created == 0 || !helper.Alive() {
		return false
	}
	image, err := helper.ImagePath()
	if err != nil || !strings.EqualFold(filepath.Clean(image), filepath.Join(m.Root.Dir, "tools", "modavd.exe")) {
		return false
	}
	args, err := helper.CommandLine()
	if err != nil || (len(args) != 6 && len(args) != 7) || args[1] != LogcatCaptureArg ||
		!strings.EqualFold(filepath.Clean(args[2]), m.Root.Dir) || args[3] != strconv.Itoa(i) ||
		args[4] != strconv.FormatUint(uint64(state.QemuPID), 10) || args[5] != strconv.FormatUint(state.QemuCreated, 10) {
		return false
	}
	if len(args) == 7 {
		target, err := captureRuntimeFromToken(m.Root, args[6])
		if err != nil {
			return false
		}
		given, _ := target.RuntimeConfig()
		current, err := m.Root.SnapshotRuntime()
		if err != nil {
			return false
		}
		want, _ := current.RuntimeConfig()
		if given != want {
			return false
		}
	}
	return true
}

func (m *Manager) ownedCapture(i int, state captureState) (win.Process, bool) {
	helper := win.Process{PID: state.HelperPID, Created: state.HelperCreated}
	client := win.Process{PID: state.ClientPID, Created: state.ClientCreated}
	if !m.ownsCaptureHelper(i, state) || client.PID == 0 || client.Created == 0 || !client.Alive() {
		return win.Process{}, false
	}
	p, found := win.FindProcess(client.PID)
	if !found || p.Created != client.Created || !captureChildOf(p, helper) {
		return win.Process{}, false
	}
	image, err := client.ImagePath()
	if err != nil || !strings.EqualFold(filepath.Clean(image), filepath.Clean(m.Root.AdbExe())) {
		return win.Process{}, false
	}
	args, err := client.CommandLine()
	if err != nil || !logcatCmdlineMatches(args, strconv.Itoa(m.Root.AdbServerPort()), m.Root.Ports().Serial(i)) {
		return win.Process{}, false
	}
	for _, arg := range args {
		if arg == "-d" || arg == "--dump" {
			return win.Process{}, false
		}
	}
	return p, true
}

func captureChildOf(client, helper win.Process) bool {
	return client.PPID == helper.PID && client.Created != 0 && helper.Created != 0 && client.Created >= helper.Created
}

// A new owned adb child may appear just before its state file is published. Its validated helper parent proves
// this is a transition to wait for, not an external capture. This weaker readiness fact never permits killing it.
func (m *Manager) pendingOwnedCapture(i int, client win.Process) bool {
	image, err := client.ImagePath()
	if err != nil || !strings.EqualFold(filepath.Clean(image), filepath.Clean(m.Root.AdbExe())) {
		return false
	}
	helper, ok := win.FindProcess(client.PPID)
	if !ok || !captureChildOf(client, helper) {
		return false
	}
	args, err := helper.CommandLine()
	if err != nil || (len(args) != 6 && len(args) != 7) {
		return false
	}
	qpid, pidErr := strconv.ParseUint(args[4], 10, 32)
	qcreated, createdErr := strconv.ParseUint(args[5], 10, 64)
	if pidErr != nil || createdErr != nil || qpid == 0 || qcreated == 0 {
		return false
	}
	return m.ownsCaptureHelper(i, captureState{HelperPID: helper.PID, HelperCreated: helper.Created, QemuPID: uint32(qpid), QemuCreated: qcreated})
}

func (m *Manager) findOwnedLogcatProcesses(serial string) ([]win.Process, error) {
	port, ok := consolePortOfSerial(serial)
	if !ok {
		return nil, nil
	}
	i, ok := m.Root.Ports().IndexOfConsole(port)
	if !ok {
		return nil, nil
	}
	state, err := m.readCaptureState(i)
	if err != nil {
		return nil, nil
	}
	if p, ok := m.ownedCapture(i, state); ok {
		return []win.Process{p}, nil
	}
	return nil, nil
}

func (m *Manager) waitDiagnosticCapture(ctx context.Context, i int, timeout time.Duration) error {
	r, err := m.Root.SnapshotRuntime()
	if err != nil {
		m.Log.Warnf("实例 %d 无法确认 SDK/adb 目标，本次不切换宿主采集: %v", i, err)
		return nil
	}
	return m.withRuntime(r).waitDiagnosticCaptureRuntime(ctx, i, timeout)
}

func (m *Manager) waitDiagnosticCaptureRuntime(ctx context.Context, i int, timeout time.Duration) error {
	for deadline := time.Now().Add(timeout); ; {
		policy := m.diagnosticPolicy(i)
		state, err := m.readCaptureState(i)
		server := captureServerProcess(m)
		serverMatches := server.PID == 0 || server.Created == 0 || state.ServerPID == 0 || state.ServerCreated == 0 ||
			state.ServerPID == server.PID && state.ServerCreated == server.Created
		if err == nil && state.Enabled == policy.Enabled && state.Since == policy.Since && serverMatches {
			if _, ok := m.ownedCapture(i, state); ok {
				return nil
			}
		}
		clients, scanErr := m.findLogcatProcesses(m.Root.Ports().Serial(i))
		if scanErr == nil && len(clients) > 0 {
			// The helper can replace its client while the process snapshot is being read. Classify against its
			// latest published state, not the previous generation read before the snapshot.
			if latest, readErr := m.readCaptureState(i); readErr == nil {
				state = latest
			}
			owned, ours := m.ownedCapture(i, state)
			helperOurs := m.ownsCaptureHelper(i, state)
			for _, client := range clients {
				if !client.Alive() {
					continue
				}
				if helperOurs && captureChildOf(client, win.Process{PID: state.HelperPID, Created: state.HelperCreated}) {
					image, err := client.ImagePath()
					if err == nil && strings.EqualFold(filepath.Clean(image), filepath.Clean(m.Root.AdbExe())) {
						continue
					}
				}
				if m.pendingOwnedCapture(i, client) {
					continue
				}
				if !ours || client.PID != owned.PID || client.Created != owned.Created {
					m.warnExternalCapture(i, policy)
					return nil
				}
			}
		}
		if _, running, qerr := m.qemuOf(i); qerr != nil || !running {
			return qerr
		}
		if ctx.Err() != nil {
			return errs.Wrap(errs.Canceled, ctx.Err(), "诊断设置已保存，等待日志采集切换时取消")
		}
		if time.Now().After(deadline) {
			return errs.New(errs.Timeout, "诊断设置已保存，但宿主日志采集尚未完成切换").WithAdvice("重试相同的诊断开关命令")
		}
		if !m.lockHeld(i, "logcat") {
			m.ensureLogcat(ctx, i)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *Manager) warnExternalCapture(i int, policy diagnostics.State) {
	if policy.Enabled {
		m.Log.Warnf("实例 %d 已有其它程序采集 logcat，诊断模式未接管宿主采集；客体缓冲区设置已应用，外部采集保持不变", i)
	}
}

// A failed observation is unknown, not a server replacement. The first successful observation establishes the
// baseline when startup could not read it. Restart only between two known process identities.
func captureServerChanged(observed *win.Process, next win.Process) bool {
	if next.PID == 0 || next.Created == 0 {
		return false
	}
	changed := observed.PID != 0 && observed.Created != 0 && (next.PID != observed.PID || next.Created != observed.Created)
	*observed = next
	return changed
}

var captureServerProcess = (*Manager).privateServerProcess

func diagnosticLogcatArgs(server int, serial string, enabled bool) []string {
	if !enabled {
		return logcatArgs(server, serial)
	}
	return []string{"-P", strconv.Itoa(server), "-s", serial, "shell", "su", "0", "logcat", "-v", "threadtime", "-b", "main,system,crash,events,kernel"}
}

func (m *Manager) captureStarted(i int, q win.Process, policy diagnostics.State, server win.Process) func(int) error {
	return func(pid int) error {
		helper, ok := win.FindProcess(uint32(os.Getpid()))
		if !ok || helper.Created == 0 {
			return fmt.Errorf("读不到采集进程身份")
		}
		client, ok := win.FindProcess(uint32(pid))
		if !ok || client.Created == 0 {
			return fmt.Errorf("读不到 adb 采集进程身份")
		}
		return m.writeCaptureState(i, captureState{HelperPID: helper.PID, HelperCreated: helper.Created,
			ClientPID: client.PID, ClientCreated: client.Created, QemuPID: q.PID, QemuCreated: q.Created,
			ServerPID: server.PID, ServerCreated: server.Created,
			Enabled: policy.Enabled, Since: policy.Since})
	}
}
