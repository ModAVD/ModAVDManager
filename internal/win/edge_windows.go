//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package win

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// EdgeWindow is a browser/app window of one identified Edge process. Helpers such as its message and GPU windows
// are excluded. The process identity is checked again before touching the HWND, including its creation time.
type EdgeWindow struct {
	hwnd    uintptr
	process Process
}

func (w EdgeWindow) HWND() uintptr { return w.hwnd }

func (w EdgeWindow) valid() bool {
	var pid uint32
	procGetWindowThreadProcessId.Call(w.hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid == w.process.PID && w.process.Alive()
}

func (w EdgeWindow) Visible() bool {
	if !w.valid() {
		return false
	}
	r, _, _ := procIsWindowVisible.Call(w.hwnd)
	return r != 0
}

// Minimized distinguishes an iconic but still visible window from a restored one.
func (w EdgeWindow) Minimized() bool {
	if !w.valid() {
		return false
	}
	r, _, _ := procIsIconic.Call(w.hwnd)
	return r != 0
}

// SetVisible posts the change to Edge's thread rather than blocking on a hung browser. Showing a minimized window
// restores it, while a maximized window keeps its size.
func (w EdgeWindow) SetVisible(visible bool) error {
	if !w.valid() {
		return nil
	}
	cmd := uintptr(swHide)
	if visible {
		cmd = swShow
		if w.Minimized() {
			cmd = 9
		} // SW_RESTORE
	}
	if r, err := call(procShowWindowAsync, w.hwnd, cmd); r == 0 {
		return fmt.Errorf("切换 Edge 窗口失败: %w", err)
	}
	return nil
}

// Foreground reports whether Windows accepted the activation request. Activation of Edge's input queue may
// complete asynchronously. A background caller without foreground permission gets false; showing still works.
func (w EdgeWindow) Foreground() bool {
	if !w.valid() {
		return false
	}
	r, _, _ := procSetForegroundWindow.Call(w.hwnd)
	return r != 0
}

var (
	procIsIconic      = moduser32.NewProc("IsIconic")
	edgeWindowMu      sync.Mutex
	edgeWindowTargets map[uint32]Process
	edgeWindowsFound  []EdgeWindow
	edgeWindowEnumCb  = sync.OnceValue(func() uintptr { return syscall.NewCallback(enumEdgeWindows) })
)

func enumEdgeWindows(hwnd, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	p, ok := edgeWindowTargets[pid]
	if !ok {
		return 1
	}
	// Chrome_WidgetWin_0 and titleless windows are browser helpers. Include owned top-level dialogs too: they
	// belong to this dedicated profile and must not be left behind when the manager is hidden.
	if windowText(procGetClassNameW, hwnd) == "Chrome_WidgetWin_1" && windowText(procGetWindowTextW, hwnd) != "" {
		edgeWindowsFound = append(edgeWindowsFound, EdgeWindow{hwnd: hwnd, process: p})
	}
	return 1
}

// edgeProfileMatches compares parsed switches rather than command-line substrings, so a sibling profile, URL or
// renderer argument cannot make an unrelated Edge window belong to the manager.
func edgeProfileMatches(args []string, profile string) bool {
	value := ""
	for i := 1; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--type=") {
			return false
		}
		if v, ok := strings.CutPrefix(args[i], "--user-data-dir="); ok {
			value = v
		} else if args[i] == "--user-data-dir" && i+1 < len(args) {
			i++
			value = args[i]
		}
	}
	return value != "" && filepath.IsAbs(value) && strings.EqualFold(filepath.Clean(value), filepath.Clean(profile))
}

// EdgeProfileWindows finds all manager windows, including hidden ones and ones handed to an existing Edge. It
// never uses the short-lived process returned by LaunchWindow. Unreadable or exited processes are left alone.
func EdgeProfileWindows(profile string) ([]EdgeWindow, error) {
	procs, err := ListProcesses()
	if err != nil {
		return nil, err
	}
	targets := map[uint32]Process{}
	for _, p := range procs {
		if !strings.EqualFold(p.Name, "msedge.exe") || p.Created == 0 {
			continue
		}
		args, err := p.CommandLine()
		if err == nil && edgeProfileMatches(args, profile) {
			targets[p.PID] = p
		}
	}
	return edgeProcessWindows(targets)
}

func edgeProcessWindows(targets map[uint32]Process) ([]EdgeWindow, error) {
	edgeWindowMu.Lock()
	defer edgeWindowMu.Unlock()
	edgeWindowTargets, edgeWindowsFound = targets, nil
	defer func() { edgeWindowTargets, edgeWindowsFound = nil, nil }()
	if r, err := call(procEnumWindows, edgeWindowEnumCb(), 0); r == 0 {
		return nil, err
	}
	return edgeWindowsFound, nil
}
