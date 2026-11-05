//go:build windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/root"
	"modavd/internal/win"
)

const crashTestLaunchArg = "__test_crash_archive_launch"
const crashTestGateEnv = "MODAVD_TEST_CRASH_GATE"

func crashTestRoot(t *testing.T) (*Manager, diagnostics.State, string) {
	t.Helper()
	m := diagnosticTestManager(t)
	if _, err := m.SetDiagnostics(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	local := t.TempDir()
	temp := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("TEMP", temp)
	t.Setenv("TMP", temp)
	source := filepath.Join(local, "Temp", "AndroidEmulator", "emu-crash-37.2.10-mas.db", "reports", "test.dmp")
	if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("PUBLIC"), 0o644); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-time.Minute)
	if err := os.Chtimes(source, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	states, err := diagnostics.Read(m.Root)
	if err != nil {
		t.Fatal(err)
	}
	return m, states[0], source
}

func TestDiagnosticCrashArchivePreservesPayloadAndWatermark(t *testing.T) {
	m, state, source := crashTestRoot(t)
	through := time.Now()
	if err := m.archiveDiagnosticCrashes(context.Background(), 0, state, through); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")
	files := names(t, dir)
	if len(files) != 1 || !strings.HasSuffix(files[0], "test.dmp") {
		t.Fatalf("archive = %q", files)
	}
	want, _ := os.ReadFile(source)
	got, err := os.ReadFile(filepath.Join(dir, files[0]))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("archive = %q/%v", got, err)
	}
	var scan crashScanState
	b, err := os.ReadFile(m.crashScanPath(0))
	if err != nil || json.Unmarshal(b, &scan) != nil || scan.Policy != state || !scan.Through.Equal(through) {
		t.Fatalf("watermark = %+v / %v", scan, err)
	}
	if err := m.archiveDiagnosticCrashes(context.Background(), 0, state, through.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if files := names(t, dir); len(files) != 1 {
		t.Fatalf("duplicate archive = %q", files)
	}
	if _, err := m.SetDiagnostics(context.Background(), 0, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("off archive remains: %v", err)
	}
	if _, err := os.Stat(m.crashScanPath(0)); !os.IsNotExist(err) {
		t.Fatalf("off watermark remains: %v", err)
	}
}

func TestDiagnosticCrashGrowthDoesNotAdvanceWatermark(t *testing.T) {
	m, state, source := crashTestRoot(t)
	saved := copyDiagnosticDump
	t.Cleanup(func() { copyDiagnosticDump = saved })
	copyDiagnosticDump = func(ctx context.Context, out io.Writer, in io.Reader, size int64) (int64, error) {
		f, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return 0, err
		}
		_, err = f.Write([]byte("-growth"))
		f.Close()
		if err != nil {
			return 0, err
		}
		return saved(ctx, out, in, size)
	}
	if err := m.archiveDiagnosticCrashes(context.Background(), 0, state, time.Now()); err == nil {
		t.Fatal("growing dump was published")
	}
	if _, err := os.Stat(m.crashScanPath(0)); !os.IsNotExist(err) {
		t.Fatal("failed copy advanced watermark")
	}
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")); !os.IsNotExist(err) {
		t.Fatal("failed dump archive exists")
	}
	copyDiagnosticDump = saved
	if err := m.archiveDiagnosticCrashes(context.Background(), 0, state, time.Now().Add(time.Second)); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestDiagnosticCrashRejectsSameSizeReplacement(t *testing.T) {
	m, state, source := crashTestRoot(t)
	candidates, err := diagnosticCrashSources(time.Now().Add(-diagnostics.MaxAge), time.Now())
	if err != nil || len(candidates) != 1 {
		t.Fatalf("sources = %+v/%v", candidates, err)
	}
	selected := candidates[0]
	if err := os.Rename(source, source+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(source, selected.info.ModTime(), selected.info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := m.copyAndPublishCrash(context.Background(), 0, state, selected); err == nil {
		t.Fatal("replacement dump was copied")
	}
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")); !os.IsNotExist(err) {
		t.Fatal("replacement archive exists")
	}
}

func TestOldCrashCopyCannotPublishAfterOffOnOrRecreate(t *testing.T) {
	for _, recreate := range []bool{false, true} {
		t.Run(fmt.Sprint(recreate), func(t *testing.T) {
			m, state, _ := crashTestRoot(t)
			sources, err := diagnosticCrashSources(time.Now().Add(-diagnostics.MaxAge), time.Now())
			if err != nil || len(sources) != 1 {
				t.Fatalf("sources = %v/%v", sources, err)
			}
			started, release := make(chan struct{}), make(chan struct{})
			saved := copyDiagnosticDump
			t.Cleanup(func() { copyDiagnosticDump = saved })
			copyDiagnosticDump = func(ctx context.Context, out io.Writer, in io.Reader, size int64) (int64, error) {
				close(started)
				<-release
				return saved(ctx, out, in, size)
			}
			done := make(chan error, 1)
			go func() { done <- m.copyAndPublishCrash(context.Background(), 0, state, sources[0]) }()
			<-started
			// Copy does not hold retention while blocked on I/O.
			lock, err := m.retentionLock()
			if err != nil {
				close(release)
				t.Fatal("copy held retention", err)
			}
			lock.Unlock()
			if recreate {
				if _, err := m.Delete(context.Background(), 0); err != nil {
					close(release)
					t.Fatal(err)
				}
				zero := 0
				if _, err := m.Create(context.Background(), CreateRequest{Index: &zero, filesOnly: true}); err != nil {
					close(release)
					t.Fatal(err)
				}
			} else {
				if _, err := m.SetDiagnostics(context.Background(), 0, false); err != nil {
					close(release)
					t.Fatal(err)
				}
			}
			if _, err := m.SetDiagnostics(context.Background(), 0, true); err != nil {
				close(release)
				t.Fatal(err)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")); !os.IsNotExist(err) {
				t.Fatal("old generation revived archive", err)
			}
		})
	}
}

func TestDiagnosticCrashHelperSurvivesShortLauncher(t *testing.T) {
	m, _, _ := crashTestRoot(t)
	copyTestExe(t, filepath.Join(m.Root.Dir, "tools", "modavd.exe"))
	gate := filepath.Join(m.Root.Dir, "copy-release")
	t.Setenv(crashTestGateEnv, gate)
	self, _ := os.Executable()
	cmd := exec.Command(self, crashTestLaunchArg, m.Root.Dir, "0")
	win.HideConsoleCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("launcher = %s/%v", out, err)
	}
	waitLogcatTest(t, func() bool { _, err := os.Stat(gate + ".started"); return err == nil })
	var helper win.Process
	processes, err := win.ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	for _, process := range processes {
		args, err := process.CommandLine()
		if err == nil && len(args) == 7 && args[1] == DiagnosticArchiveArg && strings.EqualFold(args[2], m.Root.Dir) {
			helper = process
			break
		}
	}
	if helper.PID == 0 || !helper.Alive() {
		t.Fatal("orphan did not survive launcher")
	}
	t.Cleanup(func() { helper.Kill(); helper.Wait(time.Second) })
	if _, err := os.Stat(m.crashScanPath(0)); !os.IsNotExist(err) {
		t.Fatal("unfinished copy advanced watermark")
	}
	lock, err := m.retentionLock()
	if err != nil {
		t.Fatal("blocked copy held retention", err)
	}
	lock.Unlock()
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !helper.Wait(10 * time.Second) {
		t.Fatal("copy helper did not exit")
	}
	if code, ok := helper.ExitCode(); !ok || code != 0 {
		t.Fatalf("copy helper exit = %d/%v", code, ok)
	}
	if _, err := os.Stat(m.crashScanPath(0)); err != nil {
		t.Fatal("completed copy missing watermark", err)
	}
}

func configureCrashTestCopyGate() {
	gate := os.Getenv(crashTestGateEnv)
	if gate == "" {
		return
	}
	saved := copyDiagnosticDump
	copyDiagnosticDump = func(ctx context.Context, out io.Writer, in io.Reader, size int64) (int64, error) {
		if err := os.WriteFile(gate+".started", nil, 0o644); err != nil {
			return 0, err
		}
		for {
			if _, err := os.Stat(gate); err == nil {
				return saved(ctx, out, in, size)
			}
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
}

func runCrashTestLauncher(args []string) int {
	if len(args) != 2 {
		return 2
	}
	r, err := root.New(args[0])
	if err != nil {
		return 1
	}
	i, err := strconv.Atoi(args[1])
	if err != nil {
		return 2
	}
	win.OrphanHelper, _ = os.Executable()
	New(r, nil).startDiagnosticArchive(i)
	return 0
}
