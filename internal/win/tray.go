//go:build windows

package win

import (
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// Tray is the manager's hidden window (ModAVD/ModAVD#101): it owns the global hot keys of the boss keys
// (RegisterHotKey) and the notification-area icon. Everything runs on one locked OS thread that pumps the window's
// messages: a hot key belongs to the thread of the window it was registered for (RegisterHotKey fails with
// ERROR_WINDOW_OF_OTHER_THREAD from any other), so registering goes through that thread as well (call). One Tray per
// process.
//
// The window is an ordinary top-level window that is never shown, not a message-only one: only top-level windows
// get the TaskbarCreated broadcast, which says the icon must be added again after Explorer restarted.
type Tray struct {
	hwnd    uintptr
	ev      TrayEvents
	noIcon  bool
	tip     [128]uint16
	icon    uintptr
	ownIcon bool
	hotkeys map[int]bool

	// Only the window thread changes click state. KillTimer leaves queued WM_TIMER messages, so ids are not reused.
	clickTimer, nextTimer uintptr
	skipButtonUp          bool

	calls     chan func()
	done      chan struct{}
	closeOnce sync.Once
	eventMu   sync.Mutex
	events    []func()
	eventWake chan struct{}
}

// TrayEvents run in delivery order on a worker goroutine, never on the window thread. They may call back into
// Tray, including Close. Close drops queued events and does not wait for a callback already running.
type TrayEvents struct {
	HotKey      func(id int) // a registered hot key was pressed
	Click       func()       // a single left click, after the system double-click interval
	DoubleClick func()       // a double left click; neither button-up also fires Click
	Open        func()       // menu: 打开管理器
	Exit        func()       // menu: 退出
}

// TrayOptions of StartTray.
type TrayOptions struct {
	Tip    string // the icon's tooltip, at most 127 UTF-16 units (cut off beyond)
	Events TrayEvents
	// NoIcon: hot keys only, no notification-area icon (tests: nothing shows up on the desktop).
	NoIcon bool
}

// ErrHotKeyTaken is RegisterHotKey's error when the combination is registered already (by another program, or by
// this process for another id).
const ErrHotKeyTaken = syscall.Errno(1409) // ERROR_HOTKEY_ALREADY_REGISTERED

const (
	trayClassName = "ModAVDManagerTray"
	trayIconID    = 1
	// tryHotKeyID is the id of TryHotKey's register-and-release; ids of applications are 0x0000–0xBFFF.
	tryHotKeyID = 0xBFFF

	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmHotKey        = 0x0312
	wmTimer         = 0x0113
	wmLButtonDblClk = 0x0203
	csDblClks       = 0x0008
	wmLButtonUp     = 0x0202
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	wmTrayNotify    = wmApp + 1 // the icon's callback message
	wmTrayCall      = wmApp + 2 // run the queued calls

	nimAdd, nimDelete                 = 0, 2
	nifMessage, nifIcon, nifTip       = 0x1, 0x2, 0x4
	imageIcon, smCxSmIcon, smCySmIcon = 1, 49, 50
	idiApplication                    = 32512
	mfString                          = 0
	tpmRightButton, tpmNoNotify       = 0x2, 0x80
	tpmReturnCmd                      = 0x100
	menuOpen, menuExit                = 1, 2
	errorClassAlreadyExists           = syscall.Errno(1410)
)

// notifyIconData is NOTIFYICONDATAW (64-bit layout, 976 bytes).
type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        [16]byte
	BalloonIcon     uintptr
}

// wndClassEx is WNDCLASSEXW.
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

// winMsg is MSG.
type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	Private uint32
}

var (
	trayCur        atomic.Pointer[Tray] // the running Tray, for the window procedure
	trayStartMu    sync.Mutex
	trayProcCb     = sync.OnceValue(func() uintptr { return syscall.NewCallback(trayProc) })
	taskbarCreated uint32 // RegisterWindowMessage("TaskbarCreated"), set before the window exists
)

// StartTray creates the window and, unless opt.NoIcon, its icon. An icon Explorer does not take now (it is not
// running yet) is added when it starts. Errors: the window could not be made, or a Tray runs already.
func StartTray(opt TrayOptions) (*Tray, error) {
	trayStartMu.Lock()
	defer trayStartMu.Unlock()
	if trayCur.Load() != nil {
		return nil, errors.New("托盘窗口已经开着")
	}
	t := &Tray{ev: opt.Events, noIcon: opt.NoIcon, hotkeys: map[int]bool{}, calls: make(chan func(), 64),
		done: make(chan struct{}), eventWake: make(chan struct{}, 1)}
	tip, _ := syscall.UTF16FromString(opt.Tip)
	copy(t.tip[:len(t.tip)-1], tip)
	ready := make(chan error, 1)
	go t.loop(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	go t.eventLoop()
	return t, nil
}

func (t *Tray) loop(ready chan<- error) {
	defer close(t.done)
	// never unlocked: a thread that made a window is not given back to other goroutines, and it exits with this one
	runtime.LockOSThread()
	inst, _, _ := procGetModuleHandleW.Call(0)
	cls, _ := syscall.UTF16PtrFromString(trayClassName)
	wc := wndClassEx{Style: csDblClks, WndProc: trayProcCb(), Instance: inst, ClassName: cls}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, err := call(procRegisterClassExW, uintptr(unsafe.Pointer(&wc))); r == 0 && !errors.Is(err, errorClassAlreadyExists) {
		ready <- errors.New("注册托盘窗口类失败: " + err.Error())
		return
	}
	name, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := procRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(name)))
	taskbarCreated = uint32(r)
	trayCur.Store(t)
	title, _ := syscall.UTF16PtrFromString("ModAVD")
	hwnd, err := call(procCreateWindowExW, 0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)), 0, 0, 0, 0, 0,
		0, 0, inst, 0)
	if hwnd == 0 {
		trayCur.Store(nil)
		ready <- errors.New("建托盘窗口失败: " + err.Error())
		return
	}
	t.hwnd = hwnd
	if !t.noIcon {
		t.loadIcon(inst)
		t.addIcon()
	}
	ready <- nil

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // WM_QUIT, or an error (the window is gone)
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	t.runCalls() // calls queued while it ended answer through done
	trayCur.Store(nil)
}

func (t *Tray) loadIcon(inst uintptr) {
	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	// the executable's icon group 1 (cmd/modavd-gui/winres); a test binary has none
	if h, _, _ := procLoadImageW.Call(inst, 1, imageIcon, cx, cy, 0); h != 0 {
		t.icon, t.ownIcon = h, true
		return
	}
	t.icon, _, _ = procLoadIconW.Call(0, idiApplication)
}

func (t *Tray) iconData() notifyIconData {
	d := notifyIconData{Wnd: t.hwnd, ID: trayIconID, Flags: nifMessage | nifIcon | nifTip, CallbackMessage: wmTrayNotify,
		Icon: t.icon, Tip: t.tip}
	d.Size = uint32(unsafe.Sizeof(d))
	return d
}

func (t *Tray) addIcon() {
	d := t.iconData()
	procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&d)))
}

func (t *Tray) removeIcon() {
	d := t.iconData()
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&d)))
}

func trayProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	t := trayCur.Load()
	if t == nil || (t.hwnd != 0 && hwnd != t.hwnd) {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	}
	switch m := uint32(msg); {
	case m == wmHotKey:
		if f := t.ev.HotKey; f != nil {
			t.dispatch(func() { f(int(wparam)) })
		}
		return 0
	case m == wmTrayNotify:
		switch uint32(lparam) & 0xFFFF {
		case wmLButtonUp:
			if t.skipButtonUp {
				t.skipButtonUp = false
			} else {
				t.deferClick()
			}
		case wmLButtonDblClk:
			t.cancelClick()
			t.skipButtonUp = true // the final button-up belongs to this double click
			if f := t.ev.DoubleClick; f != nil {
				t.dispatch(f)
			}
		case wmRButtonUp:
			t.cancelClick()
			t.skipButtonUp = false
			var f func()
			switch t.menu() {
			case menuOpen:
				f = t.ev.Open
			case menuExit:
				f = t.ev.Exit
			}
			if f != nil {
				t.dispatch(f)
			}
		}
		return 0
	case m == wmTimer:
		if t.clickTimer != 0 && wparam == t.clickTimer {
			t.cancelClick()
			if f := t.ev.Click; f != nil {
				t.dispatch(f)
			}
		}
		return 0
	case m == wmTrayCall:
		t.runCalls()
		return 0
	case m == taskbarCreated && m != 0:
		if !t.noIcon {
			t.addIcon()
		}
		return 0
	case m == wmDestroy:
		t.cancelClick()
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

// dispatch never waits for a handler: it may itself be waiting for a call on the window thread.
func (t *Tray) dispatch(f func()) {
	t.eventMu.Lock()
	t.events = append(t.events, f)
	t.eventMu.Unlock()
	select {
	case t.eventWake <- struct{}{}:
	default:
	}
}

func (t *Tray) eventLoop() {
	defer func() { t.eventMu.Lock(); t.events = nil; t.eventMu.Unlock() }()
	for {
		select {
		case <-t.done:
			return
		default:
		}
		t.eventMu.Lock()
		var f func()
		if len(t.events) > 0 {
			f = t.events[0]
			t.events[0] = nil
			t.events = t.events[1:]
		}
		t.eventMu.Unlock()
		if f != nil {
			f()
			continue
		}
		select {
		case <-t.done:
			return
		case <-t.eventWake:
		}
	}
}

// deferClick keeps cancellation and delivery on the window thread. If SetTimer fails, drop this click rather than
// accidentally turn a double click into an instance restore.
func (t *Tray) deferClick() {
	t.cancelClick()
	if t.ev.Click == nil {
		return
	}
	t.nextTimer++
	ms, _, _ := procGetDoubleClickTime.Call()
	if r, _, _ := procSetTimer.Call(t.hwnd, t.nextTimer, ms+1, 0); r != 0 {
		t.clickTimer = t.nextTimer
	}
}

func (t *Tray) cancelClick() {
	if t.clickTimer != 0 {
		procKillTimer.Call(t.hwnd, t.clickTimer)
		t.clickTimer = 0
	}
}

// menu shows the right-click menu at the cursor and returns the chosen item (0: none). The window comes to the
// foreground first, or the menu would not close when the user clicks elsewhere (TrackPopupMenu remarks).
func (t *Tray) menu() int {
	m, _, _ := procCreatePopupMenu.Call()
	if m == 0 {
		return 0
	}
	defer procDestroyMenu.Call(m)
	for _, it := range []struct {
		id   uintptr
		text string
	}{{menuOpen, "打开管理器"}, {menuExit, "退出"}} {
		p, _ := syscall.UTF16PtrFromString(it.text)
		procAppendMenuW.Call(m, mfString, it.id, uintptr(unsafe.Pointer(p)))
	}
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(t.hwnd)
	r, _, _ := procTrackPopupMenu.Call(m, tpmReturnCmd|tpmRightButton|tpmNoNotify, uintptr(int(pt.X)), uintptr(int(pt.Y)), 0,
		t.hwnd, 0)
	procPostMessageW.Call(t.hwnd, wmNull, 0, 0)
	return int(r)
}

func (t *Tray) runCalls() {
	for {
		select {
		case f := <-t.calls:
			f()
		default:
			return
		}
	}
}

// call runs f on the window's thread and waits for it; false when the Tray has ended (f did not run).
func (t *Tray) call(f func()) bool {
	ran := make(chan struct{})
	select {
	case <-t.done:
		return false
	case t.calls <- func() { f(); close(ran) }:
	}
	procPostMessageW.Call(t.hwnd, wmTrayCall, 0, 0)
	select {
	case <-ran:
		return true
	case <-t.done:
		select {
		case <-ran:
			return true
		default:
			return false
		}
	}
}

var errTrayClosed = errors.New("托盘窗口已经关了")

// RegisterHotKey registers modifiers+vk (RegisterHotKey's fsModifiers and virtual-key code) as hot key id
// (0–0xBFFE). ErrHotKeyTaken when another program (or this Tray under another id) has the combination.
func (t *Tray) RegisterHotKey(id int, modifiers, vk uint32) error {
	var err error
	if !t.call(func() {
		var r uintptr
		if r, err = call(procRegisterHotKey, t.hwnd, uintptr(id), uintptr(modifiers), uintptr(vk)); r != 0 {
			err = nil
			t.hotkeys[id] = true
		}
	}) {
		return errTrayClosed
	}
	return err
}

// UnregisterHotKey releases hot key id (nothing when it is not registered).
func (t *Tray) UnregisterHotKey(id int) {
	t.call(func() {
		if t.hotkeys[id] {
			procUnregisterHotKey.Call(t.hwnd, uintptr(id))
			delete(t.hotkeys, id)
		}
	})
}

// TryHotKey registers the combination and releases it at once: nil when it could be registered now.
func (t *Tray) TryHotKey(modifiers, vk uint32) error {
	var err error
	if !t.call(func() {
		var r uintptr
		if r, err = call(procRegisterHotKey, t.hwnd, tryHotKeyID, uintptr(modifiers), uintptr(vk)); r != 0 {
			err = nil
			procUnregisterHotKey.Call(t.hwnd, tryHotKeyID)
		}
	}) {
		return errTrayClosed
	}
	return err
}

// Close releases the hot keys, removes the icon and ends the window's thread. Safe to call more than once.
func (t *Tray) Close() {
	t.closeOnce.Do(func() {
		t.call(func() {
			t.cancelClick()
			for id := range t.hotkeys {
				procUnregisterHotKey.Call(t.hwnd, uintptr(id))
			}
			clear(t.hotkeys)
			if !t.noIcon {
				t.removeIcon()
			}
			if t.ownIcon {
				procDestroyIcon.Call(t.icon)
			}
			procDestroyWindow.Call(t.hwnd)
		})
		<-t.done
	})
}

// HWND is the tray window (the real-machine checks post WM_HOTKEY to it).
func (t *Tray) HWND() uintptr { return t.hwnd }
