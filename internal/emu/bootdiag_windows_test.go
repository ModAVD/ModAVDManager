//go:build windows

package emu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

// TestSaveBootFailure: through the fake adb of a root with portBase 21000, a failed boot of instance 6 leaves
// logs\mas_6-<time>-bootfail with what the guest gave, named in the error's details; a cancel, a stop of the instance
// and a qemu that is gone leave nothing and send nothing to adb. Create's own deadline running out counts as failed.
func TestSaveBootFailure(t *testing.T) {
	r, adbLog := portPlanRoot(t)
	ctx := context.Background()
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	running := true
	saved := startFindQemu
	t.Cleanup(func() { startFindQemu = saved })
	startFindQemu = func(*Manager, int) (win.Process, bool) { return win.Process{PID: 1}, running }

	failure := errs.New(errs.Timeout, "魔改 AVD 实例 6 启动超时")
	diagDirs := func() []string {
		matches, _ := filepath.Glob(filepath.Join(r.LogsDir(), "mas_6-*-bootfail"))
		return matches
	}
	clean := func() {
		for _, d := range diagDirs() {
			os.RemoveAll(d)
		}
		os.Remove(adbLog)
	}
	diagCalls := func() int {
		b, _ := os.ReadFile(adbLog)
		return strings.Count(string(b), "logcat")
	}
	untouched := func(name string, got error) {
		t.Helper()
		if got != error(failure) || errs.DetailsOf(got) != nil || len(diagDirs()) != 0 || diagCalls() != 0 {
			t.Errorf("%s: %v, details %v, dirs %v, adb calls %d", name, got, errs.DetailsOf(got), diagDirs(), diagCalls())
		}
		clean()
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	untouched("canceled", m.saveBootFailure(canceled, 6, failure))

	stop, err := m.lock(ctx, 6, "stop", time.Second, "正在关机")
	if err != nil {
		t.Fatal(err)
	}
	untouched("being stopped", m.saveBootFailure(ctx, 6, failure))
	stop.Unlock()

	running = false
	untouched("qemu gone", m.saveBootFailure(ctx, 6, failure))
	running = true

	got := m.saveBootFailure(ctx, 6, failure)
	dir := errs.DetailsOf(got)[errs.BootDiagDir]
	if errs.CodeOf(got) != errs.Timeout || dir == "" || failure.Details != nil {
		t.Fatalf("failed boot: %v, details %v (original %v)", got, errs.DetailsOf(got), failure.Details)
	}
	if dirs := diagDirs(); len(dirs) != 1 || dirs[0] != dir {
		t.Fatalf("directories %v, details name %s", dirs, dir)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "logcat.txt")); !strings.Contains(string(b), "zygote: fake") {
		t.Errorf("logcat.txt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "summary.txt")); !strings.Contains(string(b), "emulator-21060") ||
		!strings.Contains(string(b), "启动超时") {
		t.Errorf("summary.txt = %s", b)
	}
	// every command went to this root's instance 6, the dumpsys pipe as one argument
	b, _ := os.ReadFile(adbLog)
	var sawDumpsys bool
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var args []string
		if json.Unmarshal([]byte(line), &args) != nil {
			continue
		}
		if !slices.Contains(args, "emulator-21060") {
			t.Errorf("adb call for another serial: %q", args)
		}
		sawDumpsys = sawDumpsys || slices.Contains(args, "dumpsys activity processes | head -n 300")
	}
	if !sawDumpsys {
		t.Errorf("no dumpsys call as one argument:\n%s", b)
	}
	clean()

	expired, cancel2 := context.WithTimeout(ctx, -time.Second)
	defer cancel2()
	got = m.saveBootFailure(expired, 6, errs.Wrap(errs.Canceled, context.DeadlineExceeded, "已取消"))
	if errs.DetailsOf(got)[errs.BootDiagDir] == "" || len(diagDirs()) != 1 {
		t.Errorf("create's deadline: %v, dirs %v", errs.DetailsOf(got), diagDirs())
	}
}

// TestStartTimeoutSavesBootDiag: a start that adopts this root's (fake) qemu of instance 6, whose guest never finishes
// booting, times out with the guest state saved and named in the error; the same start canceled during the wait
// saves nothing.
func TestStartTimeoutSavesBootDiag(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir()) // the fake's emulator runtime file
	r, _ := portPlanRoot(t)
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	fakeQemu(t, t.TempDir(), 6, 21060, true, filepath.Join(r.AvdHome(), "mas_6.avd"))
	t.Setenv(fakeAdbNoBootEnv, "1")
	diagDirs := func() []string {
		matches, _ := filepath.Glob(filepath.Join(r.LogsDir(), "mas_6-*-bootfail"))
		return matches
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(1500*time.Millisecond, cancel)
	if _, err := m.Start(ctx, 6, StartOptions{Timeout: 30 * time.Second}); errs.CodeOf(err) != errs.Canceled ||
		errs.DetailsOf(err) != nil || len(diagDirs()) != 0 {
		t.Fatalf("canceled start: %v, details %v, dirs %v", err, errs.DetailsOf(err), diagDirs())
	}

	_, err := m.Start(context.Background(), 6, StartOptions{Timeout: 2 * time.Second})
	dir := errs.DetailsOf(err)[errs.BootDiagDir]
	if errs.CodeOf(err) != errs.Timeout || dir == "" {
		t.Fatalf("timed-out start: %v (%s), details %v", err, errs.CodeOf(err), errs.DetailsOf(err))
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "logcat.txt")); !strings.Contains(string(b), "zygote: fake") {
		t.Errorf("logcat.txt = %q", b)
	}
}

// TestCreateKeepsBootDiagDir: when create's setup runs out of time, the error it returns (wrapped as timeout) still
// names the guest state the failed start saved.
func TestCreateKeepsBootDiagDir(t *testing.T) {
	m, _ := opsTestManager(t)
	var h createHooks
	h.start = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		<-ctx.Done()
		return nil, errs.WithDetail(ctxErr(ctx), errs.BootDiagDir, `D:\r\logs\mas_6-x-bootfail`)
	}
	h.install(t)
	i := 6
	_, err := m.Create(context.Background(), CreateRequest{Index: &i, NoBridge: true, Timeout: 200 * time.Millisecond})
	opsSkipIfNotImplemented(t, err)
	if errs.CodeOf(err) != errs.Timeout || errs.DetailsOf(err)[errs.BootDiagDir] != `D:\r\logs\mas_6-x-bootfail` {
		t.Fatalf("create: %v (%s), details %v", err, errs.CodeOf(err), errs.DetailsOf(err))
	}
}
