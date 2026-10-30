//go:build windows

package win

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"modavd/internal/errs"
)

const (
	emulatorWindowPrefix = "Android Emulator - "
	emulatorToolbarTitle = "Emulator"
	swHide               = 0
	swShow               = 5
)

// windowSearch is the state of one EnumWindows pass. The callback is created once (syscall.NewCallback slots are
// never freed and limited in number), so the state is a package variable serialized by windowMu.
type windowSearch struct {
	pid       uint32
	mainTitle string
	found     []uintptr
}

var (
	windowMu     sync.Mutex
	windowTarget windowSearch
	windowEnumCb = sync.OnceValue(func() uintptr { return syscall.NewCallback(enumWindowsProc) })
)

// windowText reads a class name or title (GetClassNameW / GetWindowTextW share the signature).
func windowText(p *syscall.LazyProc, hwnd uintptr) string {
	var buf [512]uint16
	n, _, _ := p.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 || n > uintptr(len(buf)) {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

// enumWindowsProc collects the target's Qt windows with the main title or the toolbar title. The process has many
// other hidden windows (OpenGL, IME) that must not be touched. Windows destroyed meanwhile just read as empty.
func enumWindowsProc(hwnd, _ uintptr) uintptr {
	var owner uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
	if owner != windowTarget.pid || !strings.HasPrefix(windowText(procGetClassNameW, hwnd), "Qt") {
		return 1
	}
	if t := windowText(procGetWindowTextW, hwnd); t == windowTarget.mainTitle || t == emulatorToolbarTitle {
		windowTarget.found = append(windowTarget.found, hwnd)
	}
	return 1
}

// SetEmulatorWindowsVisible shows or hides (ShowWindowAsync SW_SHOW / SW_HIDE) the windows of process pid whose
// class name starts with "Qt" and whose title is "Android Emulator - <avd>:<console port>" (the main window) or
// "Emulator" (the side toolbar). Returns how many windows it touched; 0 for a headless instance.
//
// ShowWindowAsync only posts the change, so a busy emulator UI thread cannot block us (AUTO-MAS emulator_native).
func SetEmulatorWindowsVisible(pid uint32, avdAndPort string, visible bool) (int, error) {
	if pid == 0 {
		return 0, nil
	}
	for _, p := range []*syscall.LazyProc{procEnumWindows, procGetWindowThreadProcessId, procGetClassNameW,
		procGetWindowTextW, procShowWindowAsync} {
		if err := p.Find(); err != nil {
			return 0, errs.Wrap(errs.Unsupported, err, fmt.Sprintf("这台电脑上用不了窗口操作: %v", err))
		}
	}
	cb := windowEnumCb()
	windowMu.Lock()
	windowTarget = windowSearch{pid: pid, mainTitle: emulatorWindowPrefix + avdAndPort}
	r, _, e := procEnumWindows.Call(cb, 0)
	found := windowTarget.found
	windowTarget = windowSearch{}
	windowMu.Unlock()
	if r == 0 {
		if en, ok := e.(syscall.Errno); ok && en != 0 {
			return 0, errs.Wrap(errs.Internal, e, fmt.Sprintf("枚举窗口失败: %v", e))
		}
	}
	cmd := uintptr(swHide)
	if visible {
		cmd = swShow
	}
	for _, hwnd := range found {
		procShowWindowAsync.Call(hwnd, cmd)
	}
	return len(found), nil
}
