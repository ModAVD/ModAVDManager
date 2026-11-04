package emu

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/win"
)

// qemuWindowed: qemu p runs with the emulator window. known=false when that cannot be told (its command line is
// unreadable).
func qemuWindowed(p win.Process) (windowed, known bool) {
	cmdline, err := p.CommandLine()
	return windowedFrom(p.Name, cmdline, err)
}

// windowedFrom tells a windowed qemu from a headless one by its executable and command line: for -no-window the
// emulator starts the -headless build of qemu (main-emulator.cpp; on Linux and macOS both builds have the same name)
// and passes -no-window on to it.
func windowedFrom(name string, cmdline []string, cmdlineErr error) (windowed, known bool) {
	if strings.Contains(strings.ToLower(name), "-headless") {
		return false, true
	}
	if cmdlineErr != nil {
		return false, false
	}
	return !slices.Contains(cmdline, "-no-window"), true
}

// windowConflict is the error of a start that would adopt instance i running the other way than opt asks for
// (windowed: how it runs now). Switching would take a restart, which nobody asked for: the caller shuts it down first.
func windowConflict(i int, opt StartOptions, windowed bool) error {
	const asIs = "（不指定 --window / --headless 就照原样接管）"
	switch {
	case opt.Window && !windowed:
		return errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正以无头方式运行，运行中不能换成带窗口", i).
			WithAdvice(fmt.Sprintf("先关机再换：modavd stop %d，再 modavd start %d --window%s", i, i, asIs))
	case opt.Headless && windowed:
		return errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正带窗口运行，运行中不能换成无头", i).
			WithAdvice(fmt.Sprintf("先关机再换：modavd stop %d，再 modavd start %d --headless%s", i, i, asIs))
	}
	return nil
}

// setEmulatorWindowsVisible is win.SetEmulatorWindowsVisible; a test replaces it.
var setEmulatorWindowsVisible = win.SetEmulatorWindowsVisible

// SetWindowVisible shows / hides the window of an instance booted with one (AUTO-MAS setVisible): the main window
// and the side toolbar get SW_SHOW / SW_HIDE. Hiding does not stop rendering or the screenshot channel (10-04: the
// shared-memory frame counter kept going at ~51 fps while hidden). A headless instance has no window and cannot get
// one after boot: that is Windows 0, not an error. Unsupported hosts return errs.Unsupported before any query.
// On Windows, ordinary window failures are only logged, never returned: scripts call this
// every round in silent mode and an error would fail the whole run. The windows are those of the instance's qemu
// process, not looked up by title: the title is going to change (ModAVD/ModAVD#58).
func (m *Manager) SetWindowVisible(ctx context.Context, i int, visible bool) (*WindowResult, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	res := &WindowResult{Index: i, Visible: visible}
	p, ok, err := m.qemuOf(i)
	if err != nil {
		return nil, err
	}
	if !ok {
		return res, nil
	}
	verb := "隐藏"
	if visible {
		verb = "显示"
	}
	n, err := setEmulatorWindowsVisible(p.PID, visible)
	if err != nil {
		if errs.CodeOf(err) == errs.Unsupported {
			return nil, err
		}
		m.Log.Warnf("魔改 AVD 实例 %d %s窗口失败: %v", i, verb, err)
		return res, nil
	}
	res.Windows = n
	switch {
	case n > 0:
		m.Log.Infof("魔改 AVD 实例 %d 已%s窗口", i, verb)
	case visible:
		m.Log.Infof("魔改 AVD 实例 %d 是无头开机的，没有窗口可显示；要窗口请关机后带窗口重新启动", i)
	}
	return res, nil
}
