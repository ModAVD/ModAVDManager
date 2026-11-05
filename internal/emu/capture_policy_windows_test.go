//go:build windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/win"
)

func liveDiagnosticCapture(t *testing.T) (*Manager, win.Process, string) {
	t.Helper()
	r, log := portPlanRoot(t)
	m := New(r, nil)
	six := 6
	if _, err := m.Create(context.Background(), CreateRequest{Index: &six, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	copyTestExe(t, filepath.Join(r.Dir, "tools", "modavd.exe"))
	q := fakeQemu(t, filepath.Join(r.RuntimeSDK(), "emulator", "qemu", "windows-x86_64"), 6, 21060, false, filepath.Join(r.AvdHome(), "mas_6.avd"))
	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: q} }
	t.Cleanup(func() { qemuScan = saved })
	gate := filepath.Join(r.Dir, "stream-gate")
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeLogcatStreamEnv, gate)
	if path := m.ensureLogcat(context.Background(), 6); path == "" {
		t.Fatal("capture did not start")
	}
	t.Cleanup(func() {
		q.Kill()
		q.Wait(time.Second)
		waitLogcatTest(t, func() bool { return !m.lockHeld(6, "logcat") })
	})
	return m, q, log
}

func TestDiagnosticCaptureSwitchAndReregisterPreserveOtherClients(t *testing.T) {
	m, _, _ := liveDiagnosticCapture(t)
	ctx := context.Background()
	serial := m.Root.Ports().Serial(6)
	port := strconv.Itoa(m.Root.AdbServerPort())
	for _, enabled := range []bool{true, false} {
		if _, err := m.SetDiagnostics(ctx, 6, enabled); err != nil {
			t.Fatal(err)
		}
		state, err := m.readCaptureState(6)
		if err != nil || state.Enabled != enabled {
			t.Fatalf("capture state = %+v/%v", state, err)
		}
		client, ok := m.ownedCapture(6, state)
		if !ok {
			t.Fatal("owned client identity missing")
		}
		args, err := client.CommandLine()
		if err != nil {
			t.Fatal(err)
		}
		all := strings.Join(args, " ")
		if enabled != strings.Contains(all, "main,system,crash,events,kernel") {
			t.Fatalf("wrong buffers: %s", all)
		}
		if enabled && !strings.Contains(all, "shell su 0 logcat") {
			t.Fatalf("kernel capture is not root: %s", all)
		}
	}
	// Both have the same root adb and serial. Neither is owned by the capture helper: one is a single dump,
	// deliberately held open by the fake, and the other is a user-started stream.
	var foreign []win.Process
	for _, args := range [][]string{
		{"-P", port, "-s", serial, "logcat", "-d", "-b", "all"},
		{"-P", port, "-s", serial, "logcat", "-b", "main"},
	} {
		p, err := win.StartDetached(m.Root.AdbExe(), args, win.StartOptions{Env: m.ADB.Env()})
		if err != nil {
			t.Fatal(err)
		}
		foreign = append(foreign, p)
	}
	t.Cleanup(func() { endProcesses(foreign, time.Second) })
	server := m.privateServerProcess()
	if server.PID == 0 || server.Kill() != nil || !server.Wait(time.Second) {
		t.Fatal("fake private server did not stop")
	}
	startFake(t, m.Root.AdbExe(), "server", m.Root.AdbServerPort())
	var wg sync.WaitGroup
	errors := make(chan error, 2)
	for _, enabled := range []bool{true, false} {
		wg.Add(1)
		go func(enabled bool) { defer wg.Done(); _, err := m.SetDiagnostics(ctx, 6, enabled); errors <- err }(enabled)
	}
	wg.Add(1)
	go func() { defer wg.Done(); m.logcatAfterReregister(serial) }()
	wg.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	waitLogcatTest(t, func() bool {
		owned, _ := m.findOwnedLogcatProcesses(serial)
		return len(owned) == 0 && !m.lockHeld(6, "logcat")
	})
	for _, p := range foreign {
		if !p.Alive() {
			t.Fatal("diagnostic switch ended an independent logcat", p.PID)
		}
	}
}

func TestDelayedGuestPolicyReadsLatestMetadata(t *testing.T) {
	m, _, log := liveDiagnosticCapture(t)
	if _, err := m.persistDiagnostics(context.Background(), 6, true); err != nil {
		t.Fatal(err)
	}
	lock, err := m.Root.LockInstanceConfig(6)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := m.applyDiagnosticGuest(context.Background(), 6); done <- err }()
	// This stands in for an external metadata writer which does not take the instance config lock.
	if err := diagnostics.Set(m.Root, 6, false, time.Now()); err != nil {
		lock.Unlock()
		t.Fatal(err)
	}
	lock.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := m.waitDiagnosticCapture(context.Background(), 6, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var args []string
		if json.Unmarshal([]byte(line), &args) != nil {
			continue
		}
		text := strings.Join(args, " ")
		if strings.Contains(text, "persist.logd.size 16M") {
			last = text
		}
	}
	if last == "" || strings.Contains(last, "32M") || !strings.Contains(last, "logcat -b all -G 16M") {
		t.Fatalf("stale on policy applied: %s", last)
	}
}
