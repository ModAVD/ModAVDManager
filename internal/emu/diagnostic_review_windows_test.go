//go:build windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

func TestDeleteDiagnosticHistoryBeforeRemovingPolicy(t *testing.T) {
	m := diagnosticTestManager(t)
	if _, err := m.SetDiagnostics(context.Background(), 0, true); err != nil {
		t.Fatal(err)
	}
	seedDiagnosticLogs(t, m)
	other := mkLog(t, m.Root.LogsDir(), "mas_1-20261028-120000.log", 100, -10)
	saved := removeRetainedLog
	failed := false
	removeRetainedLog = func(f logFile, log *logx.Logger) unlinkResult {
		lock, free, err := win.Lock(filepath.Join(m.Root.LogsDir(), ".modavd-retention.lock"), 0)
		if lock != nil {
			lock.Unlock()
		}
		if err != nil || free {
			t.Fatal("delete exposed off history before cleanup was serialized", err)
		}
		if !failed {
			failed = true
			return unlinkFailed
		}
		return saved(f, log)
	}
	t.Cleanup(func() { removeRetainedLog = saved })
	if _, err := m.Delete(context.Background(), 0); err == nil {
		t.Fatal("delete ignored incomplete diagnostic cleanup")
	}
	if _, err := os.Stat(filepath.Join(m.Root.AvdHome(), "mas_0.avd", "config.ini")); err != nil {
		t.Fatal("instance removed before cleanup completed", err)
	}
	removeRetainedLog = saved
	if _, err := m.Delete(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")); !os.IsNotExist(err) {
		t.Fatal("delete left dump archive", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("another instance's ordinary history removed", err)
	}
	for _, prefix := range []string{"mas_0-", "logcat-mas_0-", "modavd-bridge-0-"} {
		count := 0
		for _, name := range names(t, m.Root.LogsDir()) {
			if strings.HasPrefix(name, prefix) && !strings.HasSuffix(name, "-bootfail") {
				count++
			}
		}
		if count != 10 {
			t.Fatalf("history %s = %d", prefix, count)
		}
	}
}

func TestUnexpectedCrashArchiveSkipsOnlyThatItem(t *testing.T) {
	m := diagnosticTestManager(t)
	archive := filepath.Join(m.Root.LogsDir(), "mas_0-crashdumps")
	if err := os.WriteFile(archive, []byte("unrelated regular file"), 0o644); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 12; k++ {
		mkLog(t, m.Root.LogsDir(), "mas_0-20261105-1200"+string(rune('A'+k))+".log", 10, k)
	}
	removed, _, err := m.cleanupDiagnostics(context.Background(), 0)
	if err != nil || removed != 2 {
		t.Fatalf("cleanup stopped at unexpected archive: %d/%v", removed, err)
	}
	if raw, err := os.ReadFile(archive); err != nil || string(raw) != "unrelated regular file" {
		t.Fatalf("archive path touched: %q/%v", raw, err)
	}
}

func TestBootFailureSurvivesIdentityAndRetentionLockFailures(t *testing.T) {
	for _, failure := range []string{"identity", "retention-lock"} {
		t.Run(failure, func(t *testing.T) {
			r, _ := portPlanRoot(t)
			m := New(r, nil)
			i := 6
			if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
				t.Fatal(err)
			}
			if failure == "identity" {
				saved := bootDiagSelfProcess
				bootDiagSelfProcess = func() (win.Process, bool) { return win.Process{}, false }
				t.Cleanup(func() { bootDiagSelfProcess = saved })
			} else {
				if err := os.MkdirAll(filepath.Join(r.LogsDir(), ".modavd-retention.lock"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			got := m.saveBootFailure(context.Background(), 6, errs.New(errs.Timeout, "fixture boot timeout"), true)
			dir := errs.DetailsOf(got)[errs.BootDiagDir]
			if dir == "" || !strings.HasSuffix(dir, "-bootfail") {
				t.Fatalf("lost completed boot failure: %v", got)
			}
			if raw, err := os.ReadFile(filepath.Join(dir, "logcat.txt")); err != nil || !strings.Contains(string(raw), "zygote: fake") {
				t.Fatalf("lost collected logcat: %s/%v", raw, err)
			}
		})
	}
}
