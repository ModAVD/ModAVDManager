//go:build windows

package emu

import (
	"testing"
	"time"

	"modavd/internal/win"
)

// TestClosingItself: a running, not booted instance reads as going down by itself only when adb no longer reaches
// the guest and no unfinished modavd boot of this qemu is recorded (setup marker).
func TestClosingItself(t *testing.T) {
	m := markerTestManager(t)
	now := time.Now()
	qemuAt := func(age time.Duration) win.Process {
		return win.Process{PID: 4000, PPID: 3000, Name: "qemu-system-x86_64.exe", Created: filetimeOf(now.Add(-age))}
	}
	old, young := qemuAt(10*time.Minute), qemuAt(20*time.Second)

	if !m.closingItself(3, old, false, now) {
		t.Error("old qemu, adb gone, no marker: want closing")
	}
	if m.closingItself(3, old, true, now) {
		t.Error("adb still answers (guest reboot, booting): want not closing")
	}
	if !m.closingItself(3, young, false, now) {
		t.Error("window closed right after the boot: want closing")
	}

	// an interrupted modavd boot of this qemu (marker before its deadline): still booting
	mk := setupMarker{QemuPID: old.PID, QemuCreated: old.Created, ValidUntil: now.Add(time.Hour).Unix()}
	m.writeSetupMarker(3, mk)
	if got, ok := m.readSetupMarker(3); !ok || got != mk {
		t.Fatalf("marker not written: %+v %v", got, ok)
	}
	if m.closingItself(3, old, false, now) {
		t.Error("unfinished modavd boot of this qemu: want not closing")
	}
	// the marker of another qemu (an earlier boot) does not count
	mk.QemuPID = 4001
	m.writeSetupMarker(3, mk)
	if !m.closingItself(3, old, false, now) {
		t.Error("marker of another qemu: want closing")
	}
	// a marker past its deadline: that boot has long been in use
	mk.QemuPID, mk.ValidUntil = old.PID, now.Add(-time.Minute).Unix()
	m.writeSetupMarker(3, mk)
	if !m.closingItself(3, old, false, now) {
		t.Error("stale marker: want closing")
	}
}
