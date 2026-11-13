//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"errors"
	"fmt"
	"time"

	"modavd/internal/win"
)

func changeManagerWindows(toggle bool) (bool, error) {
	return changeEdgeWindows(EdgeProfileDir(), toggle)
}

// changeEdgeWindows runs under server.toggleMu. Each action re-reads the actual profile windows: a launch PID or a
// cached HWND is not their owner. Wait briefly for the asynchronous changes so the next action sees the new state.
func changeEdgeWindows(profile string, toggle bool) (bool, error) {
	windows, err := win.EdgeProfileWindows(profile)
	if err != nil || len(windows) == 0 {
		return false, err
	}
	visible := true
	if toggle {
		for _, w := range windows {
			if w.Visible() {
				visible = false
				break
			}
		}
	}
	var failures []error
	for _, w := range windows {
		if err := w.SetVisible(visible); err != nil {
			failures = append(failures, err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		settled := true
		for _, w := range windows {
			if w.Visible() != visible || (visible && w.Minimized()) {
				settled = false
				break
			}
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			failures = append(failures, fmt.Errorf("Edge 窗口未在一秒内响应显示切换"))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if visible {
		windows[0].Foreground()
	}
	return true, errors.Join(failures...)
}
