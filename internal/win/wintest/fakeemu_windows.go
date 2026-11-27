//go:build windows

// Package wintest is a stand-in for the emulator's windows, for tests of the boot screen (internal/win and internal/emu):
// a child process (the test binary again, started with Env set) that makes a main window and a side toolbar with the
// class names and titles of the emulator's, far off the screen (x = 20000), so nothing shows on the desktop and no
// real window is touched. Not imported by any program.
package wintest

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// Env marks the child: a test binary that sees it in TestMain runs ChildMain instead of the tests.
const Env = "MODAVD_WINTEST_FAKE_EMULATOR"

// X and Y are where the windows are: far off any screen.
const X, Y = 20000, 20000

var (
	user32                = syscall.NewLazyDLL("user32.dll")
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleHandleW  = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW  = user32.NewProc("RegisterClassExW")
	procCreateWindowExW   = user32.NewProc("CreateWindowExW")
	procDefWindowProcW    = user32.NewProc("DefWindowProcW")
	procGetMessageW       = user32.NewProc("GetMessageW")
	procTranslateMessage  = user32.NewProc("TranslateMessage")
	procDispatchMessageW  = user32.NewProc("DispatchMessageW")
	procPostMessageW      = user32.NewProc("PostMessageW")
	procPostQuitMessage   = user32.NewProc("PostQuitMessage")
	procShowWindow        = user32.NewProc("ShowWindow")
	procShowWindowAsync   = user32.NewProc("ShowWindowAsync")
	procSetWindowPos      = user32.NewProc("SetWindowPos")
	procGetWindowThreadID = user32.NewProc("GetWindowThreadProcessId")
)

// IsChild: this process is the stand-in.
func IsChild() bool { return os.Getenv(Env) != "" }

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	Private uint32
}

// ChildMain makes the windows, prints "READY <pid> <main> <toolbar>" and pumps messages until its stdin is closed.
func ChildMain() int {
	runtime.LockOSThread()
	inst, _, _ := procGetModuleHandleW.Call(0)
	var mainW uintptr
	proc := syscall.NewCallback(func(hwnd, m, wparam, lparam uintptr) uintptr {
		if uint32(m) == 0x0002 && hwnd == mainW { // WM_DESTROY
			procPostQuitMessage.Call(0)
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, m, wparam, lparam)
		return r
	})
	for _, name := range []string{"Qt653QWindowIcon", "Qt653QWindowToolSaveBits"} {
		cls, _ := syscall.UTF16PtrFromString(name)
		wc := wndClassEx{WndProc: proc, Instance: inst, ClassName: cls}
		wc.Size = uint32(unsafe.Sizeof(wc))
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	}
	mk := func(class, title string, exStyle, style uintptr, x, y, w, h int) uintptr {
		cls, _ := syscall.UTF16PtrFromString(class)
		ttl, _ := syscall.UTF16PtrFromString(title)
		hwnd, _, _ := procCreateWindowExW.Call(exStyle, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(ttl)), style,
			uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0, 0, inst, 0)
		return hwnd
	}
	const (
		overlapped = 0x00CF0000 // WS_OVERLAPPEDWINDOW
		popup      = 0x80000000
		toolWindow = 0x00000080
		noActivate = 0x08000000
		showNoAct  = 4
	)
	mainW = mk("Qt653QWindowIcon", "cover-test", 0, overlapped, X, Y, 640, 400)
	tool := mk("Qt653QWindowToolSaveBits", "Emulator", toolWindow|noActivate, popup, X+660, Y, 60, 400)
	procShowWindow.Call(mainW, showNoAct)
	procShowWindow.Call(tool, showNoAct)
	fmt.Printf("READY %d %d %d\n", os.Getpid(), mainW, tool)
	go func() {
		io.Copy(io.Discard, os.Stdin) // the parent closing our stdin (or exiting) ends us
		procPostMessageW.Call(mainW, 0x0010, 0, 0)
	}()
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return 0
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// Emulator is a started stand-in.
type Emulator struct {
	PID        uint32
	Main, Tool uintptr
}

// Start runs the stand-in (this test binary again) and waits for its windows; it ends with the test.
func Start(t testing.TB) *Emulator {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), Env+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		in.Close()
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	})
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatalf("fake emulator: %v", err)
	}
	e := &Emulator{}
	if _, err := fmt.Sscanf(strings.TrimSpace(line), "READY %d %d %d", &e.PID, &e.Main, &e.Tool); err != nil {
		t.Fatalf("fake emulator said %q", line)
	}
	return e
}

// Move puts the main window at (x, y) with size w x h.
func (e *Emulator) Move(x, y, w, h int) {
	procSetWindowPos.Call(e.Main, 0, uintptr(x), uintptr(y), uintptr(w), uintptr(h), 0x0010|0x0004) // NOACTIVATE | NOZORDER
}

// Minimize and Restore the main window.
func (e *Emulator) Minimize() { procShowWindow.Call(e.Main, 6) }
func (e *Emulator) Restore()  { procShowWindow.Call(e.Main, 9) }

// Hide and Show the main window the way the boss key does (ShowWindowAsync).
func (e *Emulator) Hide() { procShowWindowAsync.Call(e.Main, 0) }
func (e *Emulator) Show() { procShowWindowAsync.Call(e.Main, 5) }
