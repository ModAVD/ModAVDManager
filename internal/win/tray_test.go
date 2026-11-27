//go:build windows

package win

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"
)

// A combination nothing on a desktop uses, so the tests do not collide with the user's programs: Ctrl+Alt+Shift+F23.
const testMods, testVK = 0x2 | 0x1 | 0x4 | 0x4000, 0x86

// holdHotKey registers modifiers+vk for the calling thread (no window) on a locked thread of its own, the way
// another program holds it; the returned function lets go.
func holdHotKey(t *testing.T, modifiers, vk uint32) func() {
	t.Helper()
	got := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(done)
		if r, err := call(procRegisterHotKey, 0, 0x100, uintptr(modifiers), uintptr(vk)); r == 0 {
			got <- err
			return
		}
		got <- nil
		<-release
		procUnregisterHotKey.Call(0, 0x100)
	}()
	if err := <-got; err != nil {
		t.Skipf("cannot hold Ctrl+Alt+Shift+F23 for the test (some program has it?): %v", err)
	}
	return func() { close(release); <-done }
}

func TestTrayHotKeys(t *testing.T) {
	pressed := make(chan int, 4)
	tray, err := StartTray(TrayOptions{NoIcon: true, Events: TrayEvents{HotKey: func(id int) { pressed <- id }}})
	if err != nil {
		t.Skipf("no tray window: %v", err)
	}
	defer tray.Close()
	if _, err := StartTray(TrayOptions{NoIcon: true}); err == nil {
		t.Fatal("a second Tray started")
	}

	if err := tray.TryHotKey(testMods, testVK); err != nil {
		t.Skipf("Ctrl+Alt+Shift+F23 is not free on this machine: %v", err)
	}
	// the try let go of it: we can register it for real, and another program could have taken it
	if err := tray.RegisterHotKey(7, testMods, testVK); err != nil {
		t.Fatalf("register: %v", err)
	}
	// registered by us under another id: taken
	if err := tray.TryHotKey(testMods, testVK); !errors.Is(err, ErrHotKeyTaken) {
		t.Fatalf("try while registered: %v", err)
	}
	// the hot key arrives as WM_HOTKEY to the window; a posted one stands for the key press
	procPostMessageW.Call(tray.HWND(), wmHotKey, 7, 0)
	select {
	case id := <-pressed:
		if id != 7 {
			t.Fatalf("hot key id %d", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hot key not delivered")
	}
	tray.UnregisterHotKey(7)
	tray.UnregisterHotKey(7) // twice is harmless

	// another program holds it: register and try both say taken
	release := holdHotKey(t, testMods, testVK)
	if err := tray.TryHotKey(testMods, testVK); !errors.Is(err, ErrHotKeyTaken) {
		t.Errorf("try while another holds it: %v", err)
	}
	if err := tray.RegisterHotKey(8, testMods, testVK); !errors.Is(err, ErrHotKeyTaken) {
		t.Errorf("register while another holds it: %v", err)
	}
	release()
	if err := tray.RegisterHotKey(8, testMods, testVK); err != nil {
		t.Fatalf("register after release: %v", err)
	}

	// Close lets go of every hot key, so another program can have it right away
	tray.Close()
	tray.Close()
	holdHotKey(t, testMods, testVK)()
	if err := tray.RegisterHotKey(9, testMods, testVK); err == nil {
		t.Error("register on a closed tray")
	}
	// and a new Tray can start
	again, err := StartTray(TrayOptions{NoIcon: true})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	again.Close()
}

func TestEmulatorWindowsShown(t *testing.T) {
	self := uint32(os.Getpid())
	if shown, n, err := EmulatorWindowsShown(0); shown || n != 0 || err != nil {
		t.Fatalf("pid 0: %v %d %v", shown, n, err)
	}
	suffix := fmt.Sprint("ModavdShown", self)
	main, tool := "Qt5QWindowIcon"+suffix, "Qt5QWindowToolSaveBits"+suffix
	testWindows(t, []testWindow{
		{class: main, title: "方舟"},
		{class: tool, title: "Emulator", owned: true},
		{class: main, title: "Extended Controls - mas_7:20070"}, // not the instance's window: never counted
	})
	check := func(want bool) {
		t.Helper()
		if !waitFor(3*time.Second, func() bool { shown, _, _ := EmulatorWindowsShown(self); return shown == want }) {
			shown, n, err := EmulatorWindowsShown(self)
			t.Fatalf("shown = %v (%d windows, %v), want %v", shown, n, err, want)
		}
		if _, n, err := EmulatorWindowsShown(self); n != 2 || err != nil {
			t.Fatalf("windows %d %v", n, err)
		}
	}
	check(true)
	if _, err := SetEmulatorWindowsVisible(self, false); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := SetEmulatorWindowsVisible(self, true); err != nil {
		t.Fatal(err)
	}
	check(true)
}
