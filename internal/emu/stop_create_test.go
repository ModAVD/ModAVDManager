//go:build windows

package emu

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

func TestStopCreationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, state       string
		locked, running   bool
		opt               StopOptions
		busy, interrupted bool
	}{
		{name: "creating", state: creationCreating, locked: true, running: true, busy: true},
		{name: "force does not cancel", state: creationCreating, locked: true, running: true, opt: StopOptions{Force: true}, busy: true},
		{name: "before qemu", state: creationCreating, locked: true, busy: true},
		{name: "explicit cancel", state: creationCreating, locked: true, running: true, opt: StopOptions{CancelCreate: true}, interrupted: true},
		{name: "explicit force cancel", state: creationCreating, locked: true, running: true, opt: StopOptions{CancelCreate: true, Force: true}, interrupted: true},
		{name: "own shutdown", state: creationCreating, locked: true, running: true, opt: StopOptions{creating: true}},
		{name: "ready", running: true},
		{name: "failed", state: creationFailed, running: true},
		{name: "abandoned create", state: creationCreating, running: true},
		{name: "offline"},
		{name: "cancel offline", state: creationCreating, locked: true, opt: StopOptions{CancelCreate: true}, interrupted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(managerTestRoot(t, 3), nil)
			if err := m.setCreation(3, tc.state, "", ""); err != nil {
				t.Fatal(err)
			}
			if tc.locked {
				l, ok, err := win.TryLock(m.lockPath(3, "create"))
				if err != nil || !ok {
					t.Fatalf("create lock: %v %v", ok, err)
				}
				t.Cleanup(l.Unlock)
			}
			// A fresh manager, as in another CLI / MCP / GUI process, must see the protection too.
			m = New(m.Root, nil)
			f := &stopFake{syncOK: true}
			savedScan, savedOps := qemuScan, stopOpsOf
			t.Cleanup(func() { qemuScan, stopOpsOf = savedScan, savedOps })
			scans := 0
			qemuScan = func([]int) map[int]win.Process {
				scans++
				if tc.running {
					return map[int]win.Process{3: {PID: 4242}}
				}
				return map[int]win.Process{}
			}
			stopOpsOf = func(*Manager, context.Context, int, win.Process) stopOps { return f.ops() }
			r, err := m.Stop(context.Background(), 3, tc.opt)
			if tc.busy {
				if r != nil || errs.CodeOf(err) != errs.Busy || scans != 0 || len(f.calls) != 0 {
					t.Fatalf("rejected stop: %+v %v, scans %d, calls %v", r, err, scans, f.calls)
				}
				for _, text := range []string{"等新建做完", "modavd status", "--cancel-create"} {
					if !strings.Contains(errs.As(err).Advice, text) {
						t.Errorf("advice %q lacks %q", errs.As(err).Advice, text)
					}
				}
			} else {
				if err != nil || r.InterruptedCreate != tc.interrupted {
					t.Fatalf("stop: %+v %v", r, err)
				}
				want := []string(nil)
				method := "already_offline"
				if tc.running {
					want, method = []string{"prepare", "home", "sync", "graceful"}, "graceful"
					if tc.opt.Force {
						want, method = []string{"prepare", "sync", "force_kill"}, "force"
					}
				}
				if !slices.Equal(f.calls, want) || r.Method != method {
					t.Fatalf("method %s, calls %v, want %s %v", r.Method, f.calls, method, want)
				}
			}
			if got := m.creationRecorded(3); got != tc.state {
				t.Fatalf("stop changed creation metadata: %q", got)
			}
		})
	}
}

// The real shutdown used by create must pass its own guard, without marking the boot as interrupted.
func TestCreateOwnStop(t *testing.T) {
	m, _ := opsTestManager(t)
	h := &createHooks{}
	h.install(t)
	running := true
	createRunning = func(*Manager, int) (bool, error) { return running, nil }
	savedOps := stopOpsOf
	t.Cleanup(func() { stopOpsOf = savedOps })
	qemuScan = func([]int) map[int]win.Process {
		if running {
			return map[int]win.Process{6: {PID: 4242}}
		}
		return map[int]win.Process{}
	}
	f := &stopFake{syncOK: true, onGraceful: func() { running = false }}
	stopOpsOf = func(*Manager, context.Context, int, win.Process) stopOps { return f.ops() }
	createStop = (*Manager).stopAndWait
	i := 6
	info, err := m.Create(context.Background(), CreateRequest{Index: &i, NoBridge: true})
	if err != nil || info.Status != StatusOffline || info.Creation != "" {
		t.Fatalf("create: %+v %v", info, err)
	}
	if !slices.Equal(f.calls, []string{"prepare", "home", "sync", "graceful"}) {
		t.Fatalf("shutdown calls %v", f.calls)
	}
}

func TestCancelCreateCanResume(t *testing.T) {
	m, _ := opsTestManager(t)
	h := &createHooks{}
	h.install(t)
	var stopped *StopResult
	savedOps := stopOpsOf
	t.Cleanup(func() { stopOpsOf = savedOps })
	f := &stopFake{syncOK: true}
	stopOpsOf = func(*Manager, context.Context, int, win.Process) stopOps { return f.ops() }
	h.start = func(m *Manager, ctx context.Context, i int, _ StartOptions) (*StartResult, error) {
		qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{i: {PID: 4242}} }
		var err error
		stopped, err = New(m.Root, nil).Stop(ctx, i, StopOptions{CancelCreate: true})
		if err != nil {
			return nil, err
		}
		qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{} }
		// The boot wait notices that its qemu exited during initialization.
		return nil, errs.New(errs.LaunchFailed, "模拟器在初始化时退出了")
	}
	i := 6
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, NoBridge: true}); errs.CodeOf(err) != errs.LaunchFailed {
		t.Fatalf("interrupted create: %v", err)
	}
	if stopped == nil || !stopped.InterruptedCreate || stopped.Method != "graceful" || !stopped.Synced {
		t.Fatalf("interruption result: %+v", stopped)
	}
	if info, err := m.Info(context.Background(), i); err != nil || info.Status != StatusCreateFailed {
		t.Fatalf("after interruption: %+v %v", info, err)
	}
	h.start = nil
	info, err := m.Create(context.Background(), CreateRequest{Index: &i, Resume: true})
	if err != nil || info.Status != StatusOffline || info.Creation != "" {
		t.Fatalf("resume: %+v %v", info, err)
	}
}

func TestCreateCancellationBetweenBootsAndAtCompletion(t *testing.T) {
	for _, phase := range []string{"before next boot", "final shutdown"} {
		t.Run(phase, func(t *testing.T) {
			m, _ := opsTestManager(t)
			h := &createHooks{}
			h.install(t)
			var stopped *StopResult
			interrupt := func(m *Manager, i int) {
				var err error
				stopped, err = New(m.Root, nil).Stop(context.Background(), i, StopOptions{CancelCreate: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			if phase == "before next boot" {
				h.start = func(m *Manager, ctx context.Context, i int, _ StartOptions) (*StartResult, error) {
					interrupt(m, i)
					// No QEMU is running between bridge boots. A new internal boot must also honor cancellation.
					if _, err := m.Start(ctx, i, StartOptions{creating: true}); errs.CodeOf(err) != errs.Canceled {
						t.Fatalf("another boot after cancellation: %v", err)
					}
					select {
					case <-ctx.Done():
						return nil, ctxErr(ctx)
					case <-time.After(2 * time.Second):
						t.Fatal("creator did not receive cancellation")
					}
					return nil, nil
				}
			} else {
				// The setup / verification succeeded; only its final shutdown is left. It must not erase a late cancel.
				h.onStop = interrupt
			}
			i := 6
			if _, err := m.Create(context.Background(), CreateRequest{Index: &i, NoBridge: true}); errs.CodeOf(err) != errs.Canceled {
				t.Fatalf("create: %v", err)
			}
			if stopped == nil || !stopped.InterruptedCreate || stopped.Method != "already_offline" {
				t.Fatalf("cancel without qemu: %+v", stopped)
			}
			if info, err := m.Info(context.Background(), i); err != nil || info.Status != StatusCreateFailed {
				t.Fatalf("canceled status: %+v %v", info, err)
			}
			if m.createCanceled(i) {
				t.Fatal("cancellation file left after the creator exited")
			}
			// Also recover a file left by a creator that was killed before cleanup.
			if err := os.WriteFile(m.createCancelPath(i), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			h.start, h.onStop = nil, nil
			if info, err := m.Create(context.Background(), CreateRequest{Index: &i, Resume: true}); err != nil || info.Status != StatusOffline {
				t.Fatalf("resume after cancel: %+v %v", info, err)
			}
		})
	}
}

func TestCreationBridgeRestartPassesStopGuard(t *testing.T) {
	m := New(managerTestRoot(t, 3), nil)
	if err := m.setCreation(3, creationCreating, "", ""); err != nil {
		t.Fatal(err)
	}
	l, ok, err := win.TryLock(m.lockPath(3, "create"))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	t.Cleanup(l.Unlock)
	savedOps, savedScan := stopOpsOf, qemuScan
	t.Cleanup(func() { stopOpsOf, qemuScan = savedOps, savedScan })
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{3: {PID: 4242}} }
	f := &stopFake{syncOK: true}
	stopOpsOf = func(*Manager, context.Context, int, win.Process) stopOps { return f.ops() }
	// This minimal root has no SDK for the subsequent Start. The shutdown must pass first.
	_, err = m.bridgeRestart(context.Background(), 3, StartOptions{creating: true})
	if err == nil || errs.CodeOf(err) == errs.Busy || !slices.Equal(f.calls, []string{"prepare", "home", "sync", "graceful"}) {
		t.Fatalf("bridge restart: %v, calls %v", err, f.calls)
	}
	if m.createCanceled(3) {
		t.Fatal("internal bridge restart requested external cancellation")
	}
}

func TestFinishCreationWaitsForCancellation(t *testing.T) {
	m := New(managerTestRoot(t, 3), nil)
	if err := m.setCreation(3, creationCreating, "", ""); err != nil {
		t.Fatal(err)
	}
	stop, ok, err := win.TryLock(m.lockPath(3, "stop"))
	if err != nil || !ok {
		t.Fatalf("stop lock: %v %v", ok, err)
	}
	t.Cleanup(stop.Unlock)
	waiting := make(chan struct{}, 1)
	m.Log = logx.New(func(_ logx.Level, message string) {
		if strings.Contains(message, "等它做完再继续") {
			waiting <- struct{}{}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- m.finishCreation(ctx, 3) }()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("completion passed a running stop: %v", err)
	case <-ctx.Done():
		t.Fatal("completion never tried the stop lock")
	}
	if err := os.WriteFile(m.createCancelPath(3), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stop.Unlock()
	if err := <-done; errs.CodeOf(err) != errs.Canceled || m.creationRecorded(3) != creationCreating {
		t.Fatalf("late cancellation erased by completion: %v, state %q", err, m.creationRecorded(3))
	}
}

func TestDeleteClearsStaleCreateCancel(t *testing.T) {
	m := New(managerTestRoot(t, 3), nil)
	if err := os.WriteFile(m.createCancelPath(3), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Delete(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if m.createCanceled(3) {
		t.Fatal("cancellation file left after deletion")
	}
}
