//go:build windows

package emu

import (
	"context"
	"fmt"

	"modavd/internal/root"
	"modavd/internal/win"
)

// SetWindowVisible shows / hides the window of an instance booted with one (AUTO-MAS setVisible): the main window
// and the side toolbar get SW_SHOW / SW_HIDE. Hiding does not stop rendering or the screenshot channel (10-04: the
// shared-memory frame counter kept going at ~51 fps while hidden). A headless instance has no window and cannot get
// one after boot: that is Windows 0, not an error. Window failures are only logged, never returned: scripts call this
// every round in silent mode and an error would fail the whole run.
func (m *Manager) SetWindowVisible(ctx context.Context, i int, visible bool) (*WindowResult, error) {
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	res := &WindowResult{Index: i, Visible: visible}
	p, ok := m.qemuOf(i)
	if !ok {
		return res, nil
	}
	verb := "隐藏"
	if visible {
		verb = "显示"
	}
	n, err := win.SetEmulatorWindowsVisible(p.PID, fmt.Sprintf("%s:%d", in.Name, root.ConsolePort(i)), visible)
	if err != nil {
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
