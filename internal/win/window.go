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
	emulatorToolbarTitle = "Emulator"
	swHide               = 0
	swShow               = 5
	gwOwner              = 4
)

// windowSearch is the state of one EnumWindows pass. The callback is created once (syscall.NewCallback slots are
// never freed and limited in number), so the state is a package variable serialized by windowMu.
type windowSearch struct {
	pid   uint32
	found []uintptr
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

// enumWindowsProc collects the target's emulator windows (emulatorWindow). The process has many other hidden windows
// (OpenGL, IME, Qt's own) that must not be touched. Windows destroyed meanwhile just read as empty.
func enumWindowsProc(hwnd, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid != windowTarget.pid {
		return 1
	}
	owner, _, _ := procGetWindow.Call(hwnd, gwOwner)
	if emulatorWindow(windowText(procGetClassNameW, hwnd), windowText(procGetWindowTextW, hwnd), owner != 0) {
		windowTarget.found = append(windowTarget.found, hwnd)
	}
	return 1
}

// emulatorWindow: a top-level window of the emulator process with this class, title and owner is its main window or
// its side toolbar.
//
// Qt names the window class of a QWindow "Qt<version>QWindow<kind>" (the emulator's main window is "Qt653QWindowIcon"),
// where the kind starts with Tool or Popup for those types; Qt's helper windows (the screen change observer) are named
// otherwise. The main window is the QWindow that is neither a tool nor a popup, has no owner and has a title, whatever
// the title says: the upstream "Android Emulator - <avd>:<console port>" may carry a mouse-capture prefix or the
// safe-shutdown suffix, and ModAVD/ModAVD#58 replaces it with the instance name. The caller already picked the qemu
// process, which has one main window. Two upstream windows look the same and are told apart by their fixed titles:
// Extended Controls and the "Display <n>" windows of extra displays. The side toolbar (full toolbar mode) is the tool
// window titled "Emulator".
func emulatorWindow(class, title string, owned bool) bool {
	_, kind, ok := strings.Cut(class, "QWindow")
	switch {
	case !ok || !strings.HasPrefix(class, "Qt") || title == "":
		return false
	case title == emulatorToolbarTitle:
		return true
	case owned || strings.HasPrefix(kind, "Tool") || strings.HasPrefix(kind, "Popup"):
		return false
	}
	return !strings.Contains(title, "Extended Controls") && !displayTitle(title)
}

// displayTitle: "Display <n>", the title of an extra display's window (emulator-qt-window.cpp).
func displayTitle(t string) bool {
	n, ok := strings.CutPrefix(t, "Display ")
	return ok && n != "" && strings.Trim(n, "0123456789") == ""
}

// SetEmulatorWindowsVisible shows or hides (ShowWindowAsync SW_SHOW / SW_HIDE) the main window and the side toolbar of
// emulator process pid (emulatorWindow). Returns how many windows it touched; 0 for a headless instance.
//
// ShowWindowAsync only posts the change, so a busy emulator UI thread cannot block us (AUTO-MAS emulator_native).
func SetEmulatorWindowsVisible(pid uint32, visible bool) (int, error) {
	if pid == 0 {
		return 0, nil
	}
	for _, p := range []*syscall.LazyProc{procEnumWindows, procGetWindowThreadProcessId, procGetClassNameW,
		procGetWindowTextW, procGetWindow, procShowWindowAsync} {
		if err := p.Find(); err != nil {
			return 0, errs.Wrap(errs.Unsupported, err, fmt.Sprintf("这台电脑上用不了窗口操作: %v", err))
		}
	}
	cb := windowEnumCb()
	windowMu.Lock()
	windowTarget = windowSearch{pid: pid}
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
