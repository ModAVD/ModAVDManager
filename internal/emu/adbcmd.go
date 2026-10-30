//go:build windows

package emu

// adb for callers: one guest shell command (MCP tool) and the `modavd adb <i> ...` passthrough. Both go to the
// private server only (AUTO-MAS AvdHostAdb), never 5037.

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

const passthroughServerWait = 20 * time.Second

// Shell runs one guest shell command on the private server (MCP tool).
func (m *Manager) Shell(ctx context.Context, i int, command string, timeout time.Duration) (*ShellResult, error) {
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	if strings.TrimSpace(command) == "" {
		return nil, errs.New(errs.InvalidArgument, "要执行的命令是空的")
	}
	if _, ok := m.qemuOf(i); !ok {
		return nil, errs.Newf(errs.Unsupported, "魔改 AVD 实例 %d 没有在运行", i)
	}
	res := m.shell(ctx, i, command, timeout)
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	return &ShellResult{ExitCode: res.Code, Output: res.Out}, nil
}

// AdbPassthrough runs `adb -P <private port> -s <serial> args...` with the given stdio and returns adb's exit code
// (`modavd adb <i> ...`).
func (m *Manager) AdbPassthrough(ctx context.Context, i int, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if _, err := m.instance(i); err != nil {
		return 1, err
	}
	// The server must not be spawned by the adb client below: it would inherit our stdio (see package adb).
	up, err := m.ADB.EnsureServer(ctx, passthroughServerWait)
	if err != nil {
		return 1, err
	}
	if !up {
		return 1, errs.Newf(errs.Internal, "私有 adb server（端口 %d）%.0f 秒内没起来", m.Root.AdbServerPort(), passthroughServerWait.Seconds())
	}
	serial := root.Serial(i)
	// What adb.Run does on "device not found": bring a running instance back onto a restarted private server.
	if _, running := m.qemuOf(i); running && m.ADB.GetState(ctx, serial, statusAdbTimeout) != "device" {
		m.ADB.Reregister(ctx, serial)
	}

	argv := append([]string{"-P", strconv.Itoa(m.Root.AdbServerPort()), "-s", serial}, args...)
	cmd := exec.CommandContext(ctx, m.Root.AdbExe(), argv...)
	cmd.Env = m.ADB.Env()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if !hasConsole() {
		// Without a console of ours a console child would get a new, visible one.
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	}
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if cerr := ctxErr(ctx); cerr != nil {
			return exitErr.ExitCode(), cerr
		}
		return exitErr.ExitCode(), nil
	}
	if cerr := ctxErr(ctx); cerr != nil {
		return 1, cerr
	}
	return 1, errs.Wrap(errs.Internal, err, "adb 运行失败: "+err.Error())
}

var procGetConsoleWindow = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow")

// hasConsole: this process is attached to a console (a child then shares it and opens no window of its own).
func hasConsole() bool {
	if procGetConsoleWindow.Find() != nil {
		return false
	}
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}
