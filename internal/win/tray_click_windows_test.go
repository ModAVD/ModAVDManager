//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package win

import (
	"testing"
	"time"
)

func trayClickWait() time.Duration {
	ms, _, _ := moduser32.NewProc("GetDoubleClickTime").Call()
	return time.Duration(ms) * time.Millisecond
}

func TestTraySingleClickWaitsForDoubleClick(t *testing.T) {
	clicks := make(chan struct{}, 4)
	tray, err := StartTray(TrayOptions{NoIcon: true, Events: TrayEvents{Click: func() { clicks <- struct{}{} }}})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	started := time.Now()
	procPostMessageW.Call(tray.HWND(), wmTrayNotify, trayIconID, wmLButtonUp)
	select {
	case <-clicks:
		if elapsed := time.Since(started); elapsed < trayClickWait() {
			t.Fatalf("single click ran after %v, before double-click interval", elapsed)
		}
	case <-time.After(trayClickWait() + 2*time.Second):
		t.Fatal("single click never ran")
	}
	select {
	case <-clicks:
		t.Fatal("single click repeated")
	case <-time.After(trayClickWait() + 100*time.Millisecond):
	}
}

func TestTrayDoubleClickNeverLeaksSingleClick(t *testing.T) {
	clicks := make(chan struct{}, 4)
	tray, err := StartTray(TrayOptions{NoIcon: true, Events: TrayEvents{Click: func() { clicks <- struct{}{} }}})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	for _, msg := range []uintptr{wmLButtonUp, 0x0203, wmLButtonUp} {
		procPostMessageW.Call(tray.HWND(), wmTrayNotify, trayIconID, msg)
	}
	select {
	case <-clicks:
		t.Fatal("double click leaked a single click")
	case <-time.After(trayClickWait() + 200*time.Millisecond):
	}
}

func TestTrayCloseCancelsPendingClick(t *testing.T) {
	clicks := make(chan struct{}, 4)
	tray, err := StartTray(TrayOptions{NoIcon: true, Events: TrayEvents{Click: func() { clicks <- struct{}{} }}})
	if err != nil {
		t.Fatal(err)
	}
	procPostMessageW.Call(tray.HWND(), wmTrayNotify, trayIconID, wmLButtonUp)
	if !tray.call(func() {}) {
		t.Fatal("tray closed early")
	}
	tray.Close()
	select {
	case <-clicks:
		t.Fatal("pending click ran during or after close")
	case <-time.After(trayClickWait() + 200*time.Millisecond):
	}
}

// A canceled timer may still have a queued WM_TIMER. Its id must never deliver a newer pending click.
func TestTrayStaleTimerAndNextSingleClick(t *testing.T) {
	clicks, doubles := make(chan struct{}, 4), make(chan struct{}, 4)
	tray, err := StartTray(TrayOptions{NoIcon: true, Events: TrayEvents{Click: func() { clicks <- struct{}{} }, DoubleClick: func() { doubles <- struct{}{} }}})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	var old, current uintptr
	tray.call(func() {
		tray.deferClick()
		old = tray.clickTimer
		tray.cancelClick()
		tray.deferClick()
		current = tray.clickTimer
	})
	if old == 0 || current == 0 || old == current {
		t.Fatalf("timer ids %d %d", old, current)
	}
	procPostMessageW.Call(tray.HWND(), wmTimer, old, 0)
	procPostMessageW.Call(tray.HWND(), wmTrayNotify, trayIconID, wmLButtonDblClk)
	procPostMessageW.Call(tray.HWND(), wmTrayNotify, trayIconID, wmLButtonUp)
	select {
	case <-doubles:
	case <-time.After(2 * time.Second):
		t.Fatal("no double click")
	}
	select {
	case <-clicks:
		t.Fatal("stale timer or final button-up leaked click")
	case <-time.After(trayClickWait() + 100*time.Millisecond):
	}
	procPostMessageW.Call(tray.HWND(), wmTrayNotify, trayIconID, wmLButtonUp)
	select {
	case <-clicks:
	case <-time.After(trayClickWait() + 2*time.Second):
		t.Fatal("next single click was swallowed")
	}
}

func TestTrayCallbacksAreOrderedAndCanClose(t *testing.T) {
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	order := make(chan int, 4)
	var tray *Tray
	var err error
	tray, err = StartTray(TrayOptions{NoIcon: true, Events: TrayEvents{HotKey: func(id int) {
		if id == 1 {
			close(entered)
			<-release
		}
		order <- id
		if id == 2 {
			tray.Close()
			close(closed)
		}
	}}})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	procPostMessageW.Call(tray.HWND(), wmHotKey, 1, 0)
	<-entered
	procPostMessageW.Call(tray.HWND(), wmHotKey, 2, 0)
	procPostMessageW.Call(tray.HWND(), wmHotKey, 3, 0)
	if !tray.call(func() {}) {
		t.Fatal("callback blocked window thread")
	}
	select {
	case id := <-order:
		t.Fatalf("event %d overtook first callback", id)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("Close from callback deadlocked")
	}
	for _, want := range []int{1, 2} {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("got %d want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("missing callback")
		}
	}
	select {
	case id := <-order:
		t.Fatalf("callback %d survived close", id)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTrayWindowClassAllowsDoubleClicks(t *testing.T) {
	tray, err := StartTray(TrayOptions{NoIcon: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	// GCL_STYLE is -26, sign-extended for GetClassLongPtrW.
	index := -26
	style, _, _ := moduser32.NewProc("GetClassLongPtrW").Call(tray.HWND(), uintptr(index))
	if style&csDblClks == 0 {
		t.Fatalf("window class style %#x lacks CS_DBLCLKS", style)
	}
}
