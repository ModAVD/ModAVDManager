//go:build windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

func reviewCaptureFixture(t *testing.T) (*Manager, win.Process, string) {
	t.Helper()
	r, adbLog := portPlanRoot(t)
	m := New(r, nil)
	i := 6
	if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	q := fakeQemu(t, filepath.Join(r.RuntimeSDK(), "emulator", "qemu", "windows-x86_64"), i, 21060, false, filepath.Join(r.AvdHome(), "mas_6.avd"))
	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{6: q} }
	t.Cleanup(func() { qemuScan = saved })
	gate := filepath.Join(r.Dir, "stream-gate")
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeLogcatStreamEnv, gate)
	return m, q, adbLog
}

func externalReviewCapture(t *testing.T, m *Manager) win.Process {
	t.Helper()
	p, err := win.StartDetached(m.Root.AdbExe(), logcatArgs(m.Root.AdbServerPort(), m.Root.Ports().Serial(6)), win.StartOptions{Env: m.ADB.Env()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Kill(); p.Wait(time.Second) })
	return p
}

func runReviewCapture(t *testing.T, m *Manager, q win.Process, stderr *bytes.Buffer) chan int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		done <- runLogcatCapture([]string{m.Root.Dir, "6", strconv.FormatUint(uint64(q.PID), 10), strconv.FormatUint(q.Created, 10)}, stderr, logcatSegmentBytes)
	}()
	t.Cleanup(func() {
		q.Kill()
		q.Wait(time.Second)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("capture did not stop")
		}
	})
	return done
}

func TestExternalCaptureBlocksEnsureAndDiagnosticUpdateImmediately(t *testing.T) {
	m, _, _ := reviewCaptureFixture(t)
	foreign := externalReviewCapture(t, m)
	if got := m.ensureLogcat(context.Background(), 6); got != "" {
		t.Fatal("adopted unknown external output", got)
	}
	on := true
	started := time.Now()
	res, err := m.Update(context.Background(), 6, UpdateRequest{Diagnostics: &on})
	if err != nil || !res.Instance.Diagnostics || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "未接管") {
		t.Fatalf("update=%+v/%v", res, err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("waited for an external capture to switch")
	}
	if !foreign.Alive() || m.lockHeld(6, "logcat") {
		t.Fatal("external capture killed or duplicate helper started")
	}
	clients, err := m.findLogcatProcesses(m.Root.Ports().Serial(6))
	if err != nil || len(clients) != 1 || clients[0].PID != foreign.PID {
		t.Fatalf("clients=%v/%v", clients, err)
	}
}

func TestOwnedReplacementDuringProcessSnapshotIsNotExternal(t *testing.T) {
	m, _, _ := liveDiagnosticCapture(t)
	previous, err := m.readCaptureState(6)
	if err != nil {
		t.Fatal(err)
	}
	saved := logcatListProcesses
	var delayed atomic.Bool
	logcatListProcesses = func() ([]win.Process, error) {
		if delayed.CompareAndSwap(false, true) {
			deadline := time.Now().Add(4 * time.Second)
			for {
				current, err := m.readCaptureState(6)
				if err == nil && current.Enabled && current.ClientPID != previous.ClientPID {
					break
				}
				if time.Now().After(deadline) {
					return nil, errors.New("owned helper did not publish its replacement")
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
		return saved()
	}
	t.Cleanup(func() { logcatListProcesses = saved })
	enabled := true
	result, err := m.Update(context.Background(), 6, UpdateRequest{Diagnostics: &enabled})
	if err != nil || len(result.Warnings) != 0 {
		t.Fatalf("owned replacement reported as external: %+v/%v", result, err)
	}
	if !delayed.Load() {
		t.Fatal("replacement barrier was not exercised")
	}
}

func TestLogcatChecksAgainImmediatelyBeforeAdbStart(t *testing.T) {
	m, _, _ := reviewCaptureFixture(t)
	if err := m.logcatStartGuard(6); err != nil {
		t.Fatal(err)
	}
	var external win.Process
	var output bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := captureLogcatStarted(ctx, m.Root.AdbExe(), logcatArgs(m.Root.AdbServerPort(), m.Root.Ports().Serial(6)), m.ADB.Env(), &output, func() error {
		external = externalReviewCapture(t, m)
		return m.logcatStartGuard(6)
	}, nil)
	if !errors.Is(err, errLogcatPresent) || !external.Alive() || output.Len() != 0 {
		t.Fatalf("guard=%v, external=%v, bytes=%d", err, external, output.Len())
	}
}

func TestExternalCaptureWinsOwnedPolicyRestartGap(t *testing.T) {
	m, q, _ := reviewCaptureFixture(t)
	var inserted atomic.Bool
	var scans atomic.Int32
	var foreign win.Process
	saved := logcatListProcesses
	logcatListProcesses = func() ([]win.Process, error) {
		if scans.Add(1) == 3 { // initial helper discovery, pre-adb guard, then the next policy generation
			foreign = externalReviewCapture(t, m)
			inserted.Store(true)
		}
		return saved()
	}
	t.Cleanup(func() { logcatListProcesses = saved })
	var stderr bytes.Buffer
	done := runReviewCapture(t, m, q, &stderr)
	waitLogcatTest(t, func() bool { s, err := m.readCaptureState(6); return err == nil && s.ClientPID != 0 })
	if err := diagnostics.Set(m.Root, 6, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		done <- code // leave completion available to the cleanup
		if code != 0 {
			t.Fatalf("helper=%d: %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not yield restart gap to external capture")
	}
	if !inserted.Load() || !foreign.Alive() {
		t.Fatal("external capture was not preserved")
	}
	clients, err := m.findLogcatProcesses(m.Root.Ports().Serial(6))
	if err != nil || len(clients) != 1 || clients[0].PID != foreign.PID {
		t.Fatalf("duplicate after restart: %v/%v", clients, err)
	}
}

func TestUnknownServerObservationDoesNotRestartOrRedump(t *testing.T) {
	m, q, _ := reviewCaptureFixture(t)
	saved := captureServerProcess
	var observations atomic.Int32
	captureServerProcess = func(m *Manager) win.Process {
		if observations.Add(1) == 1 {
			return saved(m)
		}
		return win.Process{} // listener/process observation failed after a known server
	}
	t.Cleanup(func() { captureServerProcess = saved })
	var stderr bytes.Buffer
	runReviewCapture(t, m, q, &stderr)
	waitLogcatTest(t, func() bool { return observations.Load() >= 3 })
	clients, err := m.findLogcatProcesses(m.Root.Ports().Serial(6))
	_, paths := readLogcatSegments(t, m.Root.LogsDir(), 6)
	data, _ := readLogcatSegments(t, m.Root.LogsDir(), 6)
	if err != nil || len(clients) != 1 || len(paths) != 1 || bytes.Count(data, []byte("sequence=000000")) != 1 {
		t.Fatalf("unknown observation restarted: clients=%v files=%v/%v", clients, paths, err)
	}
}

func TestCaptureServerChangesRequireTwoKnownIdentities(t *testing.T) {
	var observed win.Process
	if captureServerChanged(&observed, win.Process{PID: 1, Created: 2}) || captureServerChanged(&observed, win.Process{}) {
		t.Fatal("unknown observation treated as replacement")
	}
	if !captureServerChanged(&observed, win.Process{PID: 1, Created: 3}) {
		t.Fatal("PID reuse was not a replacement")
	}
	if captureServerChanged(&observed, win.Process{PID: 1, Created: 3}) {
		t.Fatal("same server restarted")
	}
	if capturePollInterval < time.Second || capturePollInterval > 2*time.Second {
		t.Fatal("capture polling too dense", capturePollInterval)
	}
}

func TestCaptureParentIdentityRejectsReusedPID(t *testing.T) {
	helper := win.Process{PID: 12, Created: 100}
	for _, client := range []win.Process{{PPID: 12, Created: 99}, {PPID: 12}, {PPID: 13, Created: 101}} {
		if captureChildOf(client, helper) {
			t.Fatalf("impossible or unknown parent relation accepted: %+v", client)
		}
	}
	if !captureChildOf(win.Process{PPID: 12, Created: 100}, helper) || !captureChildOf(win.Process{PPID: 12, Created: 101}, helper) {
		t.Fatal("valid helper child rejected")
	}
}

func TestBadDiagnosticMetadataCapturesAndBuffersOrdinarily(t *testing.T) {
	for _, failure := range []string{"bad-value", "unreadable"} {
		t.Run(failure, func(t *testing.T) {
			m, q, adbLog := reviewCaptureFixture(t)
			if failure == "bad-value" {
				if err := m.Root.EditInstanceMeta(0, func(meta *jsonord.Obj) error { meta.Set("diagnostics", jsonord.NewString("bad")); return nil }); err != nil {
					t.Fatal(err)
				}
			} else {
				saved := readOperationalDiagnostics
				readOperationalDiagnostics = func(*root.Root) (map[int]diagnostics.State, error) { return nil, os.ErrPermission }
				t.Cleanup(func() { readOperationalDiagnostics = saved })
			}
			var warnings logx.Collector
			m = m.withLog(warnings.Sink())
			for n := 0; n < 3; n++ {
				if _, err := m.applyDiagnosticGuest(context.Background(), 6); err != nil {
					t.Fatal(err)
				}
			}
			if got := warnings.Messages(); len(got) != 1 {
				t.Fatalf("repeated warnings=%v", got)
			}
			var stderr bytes.Buffer
			done := runReviewCapture(t, m, q, &stderr)
			waitLogcatTest(t, func() bool {
				state, err := m.readCaptureState(6)
				return err == nil && !state.Enabled && state.ClientPID != 0
			})
			waitLogcatTest(t, func() bool {
				data, _ := readLogcatSegments(t, m.Root.LogsDir(), 6)
				return bytes.Contains(data, []byte("sequence="))
			})
			q.Kill()
			q.Wait(time.Second)
			select {
			case code := <-done:
				done <- code
				if code != 0 {
					t.Fatalf("capture failed=%d %s", code, stderr.String())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("helper stuck")
			}
			if strings.Count(stderr.String(), "诊断策略读不了") != 1 {
				t.Fatalf("helper warning episode=%s", stderr.String())
			}
			raw, _ := os.ReadFile(adbLog)
			if !bytes.Contains(raw, []byte("logcat -b all -G 16M")) || bytes.Contains(raw, []byte("-G 32M")) {
				t.Fatalf("ordinary buffers not applied: %s", raw)
			}
			data, _ := readLogcatSegments(t, m.Root.LogsDir(), 6)
			if !bytes.Contains(data, []byte("sequence=")) {
				t.Fatal("bad metadata dropped logcat")
			}
		})
	}
}
