//go:build windows

package emu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/win"
)

// Installing on an instance whose qemu does not run is Unsupported, decided by the Manager's own process scan before
// any adb command (not even getprop). checkAPKPath / installResultLine are tested in status_test.go.
func TestInstallAPKNotRunning(t *testing.T) {
	m, i := opsTestInstance(t)
	apk := filepath.Join(opsTestTempDir(t), "game.apk")
	if err := os.WriteFile(apk, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	asked := qemuScanRecorder(t)
	_, err := m.InstallAPK(context.Background(), i, apk)
	if errs.CodeOf(err) != errs.Unsupported || !strings.Contains(err.Error(), "没有开机，请先开机再安装 APK") {
		t.Fatalf("install: %v", err)
	}
	if len(*asked) != 1 || !slices.Equal((*asked)[0], []int{i}) {
		t.Fatalf("scans = %v", *asked)
	}
	// The path is checked first: a bad one never gets to the scan.
	if _, err := m.InstallAPK(context.Background(), i, filepath.Join(filepath.Dir(apk), "missing.apk")); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("missing apk: %v", err)
	}
	if len(*asked) != 1 {
		t.Fatalf("a bad path scanned: %v", *asked)
	}
}

// installTestAPK is an instance and an .apk file to install on it (never really installed: no qemu runs).
func installTestAPK(t *testing.T) (*Manager, int, string) {
	t.Helper()
	m, i := opsTestInstance(t)
	apk := filepath.Join(opsTestTempDir(t), "game.apk")
	if err := os.WriteFile(apk, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	savedWait, savedPoll := installGuestLockWait, installGuestLockPoll
	t.Cleanup(func() { installGuestLockWait, installGuestLockPoll = savedWait, savedPoll })
	installGuestLockPoll = 10 * time.Millisecond
	return m, i, apk
}

// While a start holds the guest lock (mounting the Vulkan fix restarts zygote) an install waits up to
// installGuestLockWait for it, then is Busy without having looked at the instance at all.
func TestInstallAPKGuestLockBusy(t *testing.T) {
	m, i, apk := installTestAPK(t)
	installGuestLockWait = 150 * time.Millisecond
	l, ok, err := win.TryLock(m.guestLockPath(i))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	defer l.Unlock()
	asked := qemuScanRecorder(t)
	started := time.Now()
	_, err = m.InstallAPK(context.Background(), i, apk)
	if errs.CodeOf(err) != errs.Busy || !strings.HasPrefix(err.Error(), fmt.Sprintf("实例 %d 正在挂驱动补丁，稍后再装", i)) {
		t.Fatalf("install: %v (%s)", err, errs.CodeOf(err))
	}
	if d := time.Since(started); d < installGuestLockWait {
		t.Fatalf("gave up after %v", d)
	}
	if len(*asked) != 0 {
		t.Fatalf("looked at the instance without the guest lock: %v", *asked)
	}

	// A cancel ends the wait.
	installGuestLockWait = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := m.InstallAPK(ctx, i, apk); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("canceled: %v", err)
	}
}

// Once the start lets go of the guest lock the install goes on, holding the lock itself through the online check (and
// the adb install after it), and lets go when it returns.
func TestInstallAPKHoldsGuestLock(t *testing.T) {
	m, i, apk := installTestAPK(t)
	installGuestLockWait = 5 * time.Second
	l, ok, err := win.TryLock(m.guestLockPath(i))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	go func() { time.Sleep(100 * time.Millisecond); l.Unlock() }()
	var scans atomic.Int32
	var heldDuringScan atomic.Bool
	saved := qemuScan
	t.Cleanup(func() { qemuScan = saved })
	qemuScan = func([]int) map[int]win.Process {
		scans.Add(1)
		heldDuringScan.Store(m.lockHeld(i, "guest"))
		return map[int]win.Process{} // not running: Unsupported, no adb
	}
	started := time.Now()
	_, err = m.InstallAPK(context.Background(), i, apk)
	if errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("install: %v", err)
	}
	if d := time.Since(started); d < 100*time.Millisecond {
		t.Fatalf("did not wait for the start's guest lock (%v)", d)
	}
	if scans.Load() != 1 || !heldDuringScan.Load() {
		t.Fatalf("scans %d, guest lock held during the check: %v", scans.Load(), heldDuringScan.Load())
	}
	if m.guestLockPath(i) != m.lockPath(i, "guest") {
		t.Fatalf("guest lock path %s", m.guestLockPath(i))
	}
	if m.lockHeld(i, "guest") {
		t.Fatal("guest lock still held after the install returned")
	}
}
