package emu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
	"modavd/internal/win"
)

// bootAdbFakes makes instance 3's qemu q, the emulator report Android booted when booted says so, and counts the
// RegisterBooted tries (none succeeds); adb itself fails (the test root has no adb.exe).
func bootAdbFakes(t *testing.T, q win.Process, booted bool) *atomic.Int32 {
	t.Helper()
	savedFind, savedReported, savedRegister := waitBootFindQemu, emulatorReportedBoot, registerBooted
	savedWait, savedEvery := adbAfterBootWait, bootRegisterEvery
	t.Cleanup(func() {
		waitBootFindQemu, emulatorReportedBoot, registerBooted = savedFind, savedReported, savedRegister
		adbAfterBootWait, bootRegisterEvery = savedWait, savedEvery
	})
	waitBootFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
	emulatorReportedBoot = func(*root.Root, int, host.Process) bool { return booted }
	var tries atomic.Int32
	registerBooted = func(context.Context, *Manager, int) bool { tries.Add(1); return false }
	return &tries
}

// Android booted but adb never answers: the wait tries to register the instance, stops adbAfterBootWait after the
// emulator reported the boot (not at the far timeout), and says Android is up; at the timeout, it says the same.
func TestWaitBootAdbUnusableAfterBoot(t *testing.T) {
	m, _ := opsTestManager(t)
	q, _ := startSleeper(t)
	tries := bootAdbFakes(t, q, true)
	adbAfterBootWait, bootRegisterEvery = 1500*time.Millisecond, 500*time.Millisecond
	t0 := time.Now()
	_, err := m.waitBoot(context.Background(), 3, time.Hour, &q, nil, "", &progressor{}, nil, nil)
	if errs.CodeOf(err) != errs.Timeout || !strings.Contains(err.Error(), "魔改 AVD 实例 3 的安卓已经开好了（模拟器报告开机完成），但之后") ||
		!strings.Contains(err.Error(), "私有 adb（端口 20391）一直用不了") {
		t.Fatalf("err %v", err)
	}
	if d := time.Since(t0); d > time.Minute {
		t.Errorf("waited %v", d)
	}
	if n := tries.Load(); n < 2 {
		t.Errorf("%d register tries", n)
	}

	// the last line of the adb server's log goes into the message
	if err := os.MkdirAll(m.Root.LogsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.Root.LogsDir(), "adb-server.log"), []byte("--- adb starting (pid 9) ---\nfatal: boom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	adbAfterBootWait = time.Hour
	_, err = m.waitBoot(context.Background(), 3, 1500*time.Millisecond, &q, nil, "", &progressor{}, nil, nil)
	if errs.CodeOf(err) != errs.Timeout || !strings.Contains(err.Error(), "安卓已经开好了") ||
		!strings.Contains(err.Error(), "；adb 自己的日志最后一行：fatal: boom。日志见 ") {
		t.Fatalf("at the timeout: %v", err)
	}
}

// Without a boot report from the emulator nothing changes: no register tries, the plain timeout.
func TestWaitBootNoBootReport(t *testing.T) {
	m, _ := opsTestManager(t)
	q, _ := startSleeper(t)
	tries := bootAdbFakes(t, q, false)
	_, err := m.waitBoot(context.Background(), 3, 1500*time.Millisecond, &q, nil, "", &progressor{}, nil, nil)
	if errs.CodeOf(err) != errs.Timeout || !strings.Contains(err.Error(), "启动超时（2 秒内安卓没有启动完成）") {
		t.Fatalf("err %v", err)
	}
	if n := tries.Load(); n != 0 {
		t.Errorf("%d register tries", n)
	}
}

// bootcompleted.ini counts only when it is no older than qemu (the emulator deletes it when it launches).
func TestEmulatorReportedBoot(t *testing.T) {
	m, _ := opsTestManager(t)
	q, _ := startSleeper(t)
	ini := filepath.Join(avd.Dir(m.Root, 3), "bootcompleted.ini")
	if emulatorReportedBoot(m.Root, 3, q) {
		t.Fatal("no file: booted")
	}
	if err := os.MkdirAll(filepath.Dir(ini), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ini, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !emulatorReportedBoot(m.Root, 3, q) {
		t.Error("a file written after qemu started: not booted")
	}
	old := q.StartedAt().Add(-time.Minute)
	if err := os.Chtimes(ini, old, old); err != nil {
		t.Fatal(err)
	}
	if emulatorReportedBoot(m.Root, 3, q) {
		t.Error("a file older than qemu: booted")
	}
	if emulatorReportedBoot(m.Root, 3, win.Process{PID: q.PID}) {
		t.Error("qemu without a start time: booted")
	}
}
