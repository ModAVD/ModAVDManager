package report

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/testutil"
)

func putDiagnosticFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestDiagnosticLogBudgetsAndGrowth(t *testing.T) {
	dir := testutil.TempDir(t)
	stage := testutil.TempDir(t)
	old, oldDiag := logCapBytes, diagnosticLogCapBytes
	logCapBytes, diagnosticLogCapBytes = 12, 12
	t.Cleanup(func() { logCapBytes, diagnosticLogCapBytes = old, oldDiag })
	for _, i := range []int{0, 1, 2} {
		putDiagnosticFile(t, filepath.Join(dir, "logs", fmt.Sprintf("mas_%d-20261105-120000.log", i)), "123456")
		putDiagnosticFile(t, filepath.Join(dir, "logs", fmt.Sprintf("modavd-bridge-%d-20261105-120000.log", i)), "bridge")
	}
	c := &collector{ctx: context.Background(), root: dir, stage: stage, instances: []string{"mas_0", "mas_1", "mas_2"}, diagnostics: map[int]diagnostics.State{1: {Enabled: true}, 2: {Enabled: true}}}
	c.opt.beforeLogCopy = func() {
		for _, i := range []int{0, 1, 2} {
			p := filepath.Join(dir, "logs", fmt.Sprintf("mas_%d-20261105-120000.log", i))
			f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.WriteString(strings.Repeat("x", 100)); err != nil {
				t.Fatal(err)
			}
			f.Close()
		}
	}
	if err := c.emulatorLogs(); err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{0, 1, 2} {
		for _, name := range []string{fmt.Sprintf("mas_%d-20261105-120000.log", i), fmt.Sprintf("modavd-bridge-%d-20261105-120000.log", i)} {
			b, err := os.ReadFile(filepath.Join(stage, "logs", name))
			if err != nil {
				t.Fatal(err)
			}
			if len(b) != 6 {
				t.Fatalf("%s grew to %d", name, len(b))
			}
		}
	}
	if c.diagnosticBytes[1] != 12 || c.diagnosticBytes[2] != 12 {
		t.Fatalf("per instance budget: %v", c.diagnosticBytes)
	}
}

func TestDiagnosticSelectionAndBootIsolation(t *testing.T) {
	dir := testutil.TempDir(t)
	stage := testutil.TempDir(t)
	old := diagnosticBootCapBytes
	diagnosticBootCapBytes = 5
	t.Cleanup(func() { diagnosticBootCapBytes = old })
	for i := 0; i < 32; i++ {
		for _, n := range []string{fmt.Sprintf("mas_1-20261105-12%04d.log", i), fmt.Sprintf("logcat-mas_1-20261105-12%04d.txt", i)} {
			putDiagnosticFile(t, filepath.Join(dir, "logs", n), "x")
		}
	}
	for _, i := range []int{0, 1, 2} {
		for d := 0; d < 4; d++ {
			putDiagnosticFile(t, filepath.Join(dir, "logs", fmt.Sprintf("mas_%d-2026110%d-120000-bootfail", i, d+1), "logcat.txt"), "abc")
		}
	}
	c := &collector{ctx: context.Background(), root: dir, stage: stage, instances: []string{"mas_1"}, diagnostics: map[int]diagnostics.State{1: {Enabled: true}, 2: {Enabled: true}}}
	if err := c.emulatorLogs(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(stage, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	emus, cats, ordinary := 0, 0, 0
	sizes := map[int]int64{}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".log") {
			emus++
		}
		if strings.HasPrefix(n, "logcat-") {
			cats++
		}
		if e.IsDir() && strings.Contains(n, "bootfail") {
			if strings.HasPrefix(n, "mas_0-") {
				ordinary++
			}
			for _, i := range []int{1, 2} {
				if strings.HasPrefix(n, fmt.Sprintf("mas_%d-", i)) {
					f, err := os.Stat(filepath.Join(stage, "logs", n, "logcat.txt"))
					if err != nil {
						t.Fatal(err)
					}
					sizes[i] += f.Size()
				}
			}
		}
	}
	if emus != 30 || cats != 30 || ordinary != 3 || sizes[1] != 5 || sizes[2] != 5 {
		t.Fatalf("counts emu=%d cat=%d ordinary=%d diagnostic=%v", emus, cats, ordinary, sizes)
	}
	if !strings.Contains(strings.Join(c.lines, "\n"), "预算不足") {
		t.Fatal("missing budget explanation")
	}
}

func TestDiagnosticDumpsAgeDedupBudgetAndCancellation(t *testing.T) {
	dir := testutil.TempDir(t)
	stage := testutil.TempDir(t)
	now := time.Now()
	old := diagnosticLogCapBytes
	diagnosticLogCapBytes = 10
	t.Cleanup(func() { diagnosticLogCapBytes = old })
	putDiagnosticFile(t, filepath.Join(stage, "crash", "db", "ordinary.dmp"), "same")
	putDiagnosticFile(t, filepath.Join(dir, "logs", "mas_1-crashdumps", "duplicate.dmp"), "same")
	putDiagnosticFile(t, filepath.Join(dir, "logs", "mas_1-crashdumps", "unique.dmp"), "unique")
	putDiagnosticFile(t, filepath.Join(dir, "logs", "mas_1-crashdumps", "large.dmp"), "12345678901")
	putDiagnosticFile(t, filepath.Join(dir, "logs", "mas_1-crashdumps", "old.dmp"), "old")
	if err := os.Chtimes(filepath.Join(dir, "logs", "mas_1-crashdumps", "old.dmp"), now.Add(-31*24*time.Hour), now.Add(-31*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	putDiagnosticFile(t, filepath.Join(dir, "logs", "mas_2-crashdumps", "unrelated.dmp"), "not-enabled")
	c := &collector{ctx: context.Background(), root: dir, stage: stage, opt: CollectOptions{now: func() time.Time { return now }}, diagnostics: map[int]diagnostics.State{1: {Enabled: true}}, diagnosticBytes: map[int]int64{1: 3}}
	if err := c.diagnosticDumps(); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(stage, "logs", "mas_1-crashdumps"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name() != "unique.dmp" || c.diagnosticBytes[1] != 9 {
		t.Fatalf("files=%v bytes=%v", files, c.diagnosticBytes)
	}
	if _, err := os.Stat(filepath.Join(stage, "logs", "mas_2-crashdumps")); !os.IsNotExist(err) {
		t.Fatal("collected disabled instance")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ctx = ctx
	c.opt.beforeBoundedRead = cancel
	dst := filepath.Join(stage, "canceled")
	if _, err := c.copyBounded(filepath.Join(dir, "logs", "mas_1-crashdumps", "unique.dmp"), dst, 6); err == nil {
		t.Fatal("cancellation ignored")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("partial copy remains")
	}
}

func TestDiagnosticIndexIsolation(t *testing.T) {
	for _, name := range []string{"mas_01", "mas_1-other", "xmas_1", "mas_-1"} {
		if _, ok := diagnosticIndex(name); ok {
			t.Fatalf("accepted %q", name)
		}
	}
	if i, ok := diagnosticIndex("MAS_1"); !ok || i != 1 {
		t.Fatal("canonical name rejected")
	}
}

func TestDiagnosticArchiveFailureDoesNotHideOtherInstances(t *testing.T) {
	dir := testutil.TempDir(t)
	stage := testutil.TempDir(t)
	now := time.Now()
	putDiagnosticFile(t, filepath.Join(dir, "logs", "mas_1-crashdumps"), "not a directory")
	p := filepath.Join(dir, "logs", "mas_2-crashdumps", "good.dmp")
	putDiagnosticFile(t, p, "good")
	c := &collector{ctx: context.Background(), root: dir, stage: stage, opt: CollectOptions{now: func() time.Time { return now }}, diagnostics: map[int]diagnostics.State{1: {Enabled: true}, 2: {Enabled: true}}}
	if err := c.diagnosticDumps(); err == nil || !strings.Contains(err.Error(), "mas_1") {
		t.Fatalf("missing error: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(stage, "logs", "mas_2-crashdumps", "good.dmp")); err != nil || string(b) != "good" {
		t.Fatalf("other instance = %q, %v", b, err)
	}
}

func TestDiagnosticRejectsLinkAndFailedCleanup(t *testing.T) {
	dir := testutil.TempDir(t)
	c := &collector{ctx: context.Background()}
	target := filepath.Join(dir, "target")
	putDiagnosticFile(t, target, "secret")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err == nil {
		if _, err := c.copyBounded(link, filepath.Join(dir, "out"), 6); err == nil {
			t.Fatal("followed symlink")
		}
	}
	nonempty := filepath.Join(dir, "nonempty")
	putDiagnosticFile(t, filepath.Join(nonempty, "child"), "x")
	if err := c.discardStageFile(nonempty); err == nil || c.stageError == nil {
		t.Fatal("failed cleanup did not make the stage un-packable")
	}
}

func TestDiagnosticDumpSourceChange(t *testing.T) {
	dir := testutil.TempDir(t)
	p := filepath.Join(dir, "source.dmp")
	putDiagnosticFile(t, p, "original")
	before, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !sourceUnchanged(p, before, before.Size()) {
		t.Fatal("unchanged source rejected")
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("growth"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if sourceUnchanged(p, before, before.Size()) {
		t.Fatal("changed dump accepted")
	}
}
