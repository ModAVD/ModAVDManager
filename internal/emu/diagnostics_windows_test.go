//go:build windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

func diagnosticTestManager(t *testing.T) *Manager {
	t.Helper()
	m, _ := portPlanTestRoot(t, 0)
	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return nil }
	t.Cleanup(func() { qemuScan = saved })
	if err := os.MkdirAll(m.Root.LogsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	return m
}

func seedDiagnosticLogs(t *testing.T, m *Manager) {
	t.Helper()
	for k := 0; k < 12; k++ {
		mkLog(t, m.Root.LogsDir(), fmt.Sprintf("mas_0-20261028-1200%02d.log", k), 10, k)
		mkLog(t, m.Root.LogsDir(), fmt.Sprintf("logcat-mas_0-20261028-1200%02d.txt", k), 20, k)
		mkLog(t, m.Root.LogsDir(), fmt.Sprintf("modavd-bridge-0-20261028-1200%02d.log", k), 30, k)
	}
	for k := 0; k < 5; k++ {
		mkDiag(t, m.Root.LogsDir(), fmt.Sprintf("mas_0-20261028-1200%02d-bootfail", k), k)
	}
	dir := filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkLog(t, dir, "test.dmp", 100, 0)
}

func TestDiagnosticsPreviewAndOffUseSamePlan(t *testing.T) {
	m := diagnosticTestManager(t)
	if _, err := m.SetDiagnostics(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	seedDiagnosticLogs(t, m)
	m.pruneLogs(0)
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_0-20261028-120000.log")); err != nil {
		t.Fatal("diagnostics used the ordinary ten-file retention", err)
	}
	preview, err := m.PreviewDiagnostics(context.Background(), 0)
	if err != nil || preview.Files != 9 || preview.Bytes != 320 {
		t.Fatalf("preview = %+v, %v", preview, err)
	}
	result, err := m.SetDiagnostics(context.Background(), 0, false)
	if err != nil || result.Files != preview.Files || result.Bytes != preview.Bytes {
		t.Fatalf("off = %+v, %v; preview %+v", result, err, preview)
	}
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")); !os.IsNotExist(err) {
		t.Fatalf("off left the crash archive: %v", err)
	}
	states, err := diagnostics.Read(m.Root)
	if err != nil || states[0].Enabled || states[0].Since != "" {
		t.Fatalf("off metadata = %+v, %v", states, err)
	}
	if again, err := m.SetDiagnostics(context.Background(), 0, false); err != nil || again.Files != 0 || again.Bytes != 0 {
		t.Fatalf("repeated off = %+v, %v", again, err)
	}
}

func TestDiagnosticsRepeatedOffFinishesFailedCleanup(t *testing.T) {
	m := diagnosticTestManager(t)
	if _, err := m.SetDiagnostics(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	seedDiagnosticLogs(t, m)
	saved := removeRetainedLog
	t.Cleanup(func() { removeRetainedLog = saved })
	failed := false
	removeRetainedLog = func(f logFile, log *logx.Logger) unlinkResult {
		if !failed {
			failed = true
			return unlinkFailed
		}
		return saved(f, log)
	}
	first, err := m.SetDiagnostics(context.Background(), 0, false)
	if errs.CodeOf(err) != errs.Internal || !failed || first.Files != 8 {
		t.Fatalf("partial off = %+v, %v", first, err)
	}
	removeRetainedLog = saved
	second, err := m.SetDiagnostics(context.Background(), 0, false)
	if err != nil || second.Files != 1 {
		t.Fatalf("retry did not finish off = %+v, %v", second, err)
	}
}

func TestDiagnosticsUnreadableMetadataDeletesNothing(t *testing.T) {
	m := diagnosticTestManager(t)
	seedDiagnosticLogs(t, m)
	if err := os.WriteFile(m.Root.MetadataPath(), []byte(`{"instances":`), 0o644); err != nil {
		t.Fatal(err)
	}
	before := names(t, m.Root.LogsDir())
	if _, _, err := m.cleanupDiagnostics(context.Background(), 0); errs.CodeOf(err) != errs.MetadataUnreadable {
		t.Fatalf("corrupt metadata = %v", err)
	}
	for _, name := range before {
		if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), name)); err != nil {
			t.Fatal("corrupt metadata allowed deletion", name, err)
		}
	}
}

func TestDiagnosticsDeniedMetadataDeletesNothing(t *testing.T) {
	m := diagnosticTestManager(t)
	seedDiagnosticLogs(t, m)
	path, err := syscall.UTF16PtrFromString(m.Root.MetadataPath())
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(path, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(handle)
	before := names(t, m.Root.LogsDir())
	if _, _, err := m.cleanupDiagnostics(context.Background(), 0); errs.CodeOf(err) != errs.MetadataUnreadable {
		t.Fatalf("denied metadata = %v", err)
	}
	for _, name := range before {
		if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), name)); err != nil {
			t.Fatal("denied metadata allowed deletion", name, err)
		}
	}
}

func TestDiagnosticsLowDiskWarnsOnceAndUsesNormalRetention(t *testing.T) {
	m := diagnosticTestManager(t)
	if _, err := m.SetDiagnostics(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	seedDiagnosticLogs(t, m)
	saved := diagnosticDiskFree
	t.Cleanup(func() { diagnosticDiskFree = saved })
	diagnosticDiskFree = func(string) (uint64, error) { return 1, nil }
	var warnings []string
	m.Log = logx.New(func(level logx.Level, text string) {
		if level >= logx.Warn {
			warnings = append(warnings, text)
		}
	})
	for range 2 {
		m.pruneLogs(0)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "20 GiB") {
		t.Fatalf("warnings = %q", warnings)
	}
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_0-20261028-120000.log")); !os.IsNotExist(err) {
		t.Fatal("low disk did not fall back to ordinary retention")
	}
	diagnosticDiskFree = func(string) (uint64, error) { return 40 << 30, nil }
	m.pruneLogs(0)
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), ".mas_0-diagnostics-lowdisk")); !os.IsNotExist(err) {
		t.Fatal("recovered disk did not reset warning")
	}
}

func TestDiagnosticPoolsDoNotEvictOtherInstances(t *testing.T) {
	now := time.Now()
	files := []retainedLog{
		{logFile: logFile{path: "normal", size: 900 << 20, mtime: now}, index: 1, kind: emulatorLog},
		{logFile: logFile{path: "diag-old", size: 3 << 30, mtime: now.Add(-time.Hour)}, index: 0, kind: emulatorLog},
		{logFile: logFile{path: "diag-new", size: 2 << 30, mtime: now}, index: 0, kind: guestLog},
		{logFile: logFile{path: "other-diag", size: 3 << 30, mtime: now}, index: 2, kind: emulatorLog},
		{logFile: logFile{path: "expired", size: 1, mtime: now.Add(-31 * 24 * time.Hour)}, index: 2, kind: guestLog},
	}
	states := map[int]diagnostics.State{0: {Enabled: true}, 2: {Enabled: true}}
	plan := planCleanup(files, 0, 10, 1<<30, states, false, now)
	if len(plan.files) != 2 || plan.bytes != 3<<30+1 {
		t.Fatalf("pool plan = %+v", plan)
	}
	for _, f := range plan.files {
		if f.path != "diag-old" && f.path != "expired" {
			t.Fatalf("wrong pool evicted %s", f.path)
		}
	}
}

func TestUnpublishedBootDiagIsNeverClosedLog(t *testing.T) {
	m := diagnosticTestManager(t)
	dir := filepath.Join(m.Root.LogsDir(), ".mas_0-bootfail-writing")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkLog(t, dir, "logcat.txt", 100, 0)
	mkLog(t, m.Root.LogsDir(), "mas_1-old.log", 100, 0)
	if _, _, err := pruneLogDir(m.Root.LogsDir(), 1, 0, 0, logx.Nop()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logcat.txt")); err != nil {
		t.Fatal("incomplete snapshot was pruned", err)
	}
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_1-old.log")); !os.IsNotExist(err) {
		t.Fatal("test did not exercise deletion")
	}
}

func TestRetentionBindsSelectedFileIdentity(t *testing.T) {
	m := diagnosticTestManager(t)
	dir := m.Root.LogsDir()
	path := mkLog(t, dir, "mas_0-20261028-120000.log", 8, 0)
	files, err := managedLogFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("candidates = %+v, %v", files, err)
	}
	selected := files[0]
	if err := os.Rename(path, filepath.Join(dir, "original.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, selected.mtime, selected.mtime); err != nil {
		t.Fatal(err)
	}
	removed, freed, err := applyCleanup(cleanupPlan{files: files, bytes: selected.size}, logx.Nop())
	if err == nil || removed != 0 || freed != 0 {
		t.Fatalf("replaced file deleted: %d/%d/%v", removed, freed, err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "new-data" {
		t.Fatalf("replacement = %q, %v", b, err)
	}
}

func TestRetentionNeverFollowsReplacementLink(t *testing.T) {
	m := diagnosticTestManager(t)
	dir := m.Root.LogsDir()
	path := mkLog(t, dir, "mas_0-20261028-120000.log", 8, 0)
	files, err := managedLogFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("candidates = %+v, %v", files, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private.txt")
	if err := os.WriteFile(outside, []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	removed, freed, err := applyCleanup(cleanupPlan{files: files}, logx.Nop())
	if err == nil || removed != 0 || freed != 0 {
		t.Fatalf("replaced symlink accepted: %d/%d/%v", removed, freed, err)
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "private" {
		t.Fatalf("outside = %q, %v", b, err)
	}
}
