//go:build windows

package emu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/win"
)

// stopFake records the steps runStop takes.
type stopFake struct {
	problem      string // prepare's answer
	homeApp      bool   // home sent an app home
	homeFailure  string // home's command failed
	gracefulWhy  string // graceful's answer
	syncOK       bool
	syncOKs      []bool // the answers of successive syncs, when they differ (the last one repeats); else syncOK
	killErr      error
	aliveAfter   bool  // qemu still runs after the console kill
	forceKillErr error // forceKill's answer (qemu still runs after the terminate)
	onGraceful   func()
	onSync       func()
	onKill       func() // runs inside consoleKill
	calls        []string
	syncTimeout  time.Duration   // the last sync's
	syncTimeouts []time.Duration // every sync's, in order
	window       time.Duration   // graceful's
	homeTimeout  time.Duration
	at           map[string]time.Time // when each step was called (the last time)
	killTimeout  time.Duration
	forceKillFor time.Duration
}

func (f *stopFake) ops() stopOps {
	call := func(name string) {
		f.calls = append(f.calls, name)
		if f.at == nil {
			f.at = map[string]time.Time{}
		}
		f.at[name] = time.Now()
	}
	return stopOps{
		prepare: func() string { call("prepare"); return f.problem },
		home: func(d time.Duration) (bool, string) {
			call("home")
			f.homeTimeout = d
			return f.homeApp && f.homeFailure == "", f.homeFailure
		},
		graceful: func(window time.Duration) string {
			call("graceful")
			f.window = window
			if f.onGraceful != nil {
				f.onGraceful()
			}
			return f.gracefulWhy
		},
		sync: func(d time.Duration) (bool, string) {
			call("sync")
			f.syncTimeout = d
			f.syncTimeouts = append(f.syncTimeouts, d)
			if f.onSync != nil {
				f.onSync()
			}
			ok := f.syncOK
			if len(f.syncOKs) > 0 {
				ok = f.syncOKs[0]
				if len(f.syncOKs) > 1 {
					f.syncOKs = f.syncOKs[1:]
				}
			}
			if ok {
				return true, ""
			}
			return false, "error: closed"
		},
		consoleKill: func(d time.Duration) error {
			f.calls = append(f.calls, "console_kill")
			f.killTimeout = d
			if f.onKill != nil {
				f.onKill()
			}
			return f.killErr
		},
		alive: func() bool { f.calls = append(f.calls, "alive"); return f.aliveAfter },
		forceKill: func(d time.Duration) error {
			f.calls = append(f.calls, "force_kill")
			f.forceKillFor = d
			return f.forceKillErr
		},
	}
}

func runStopFake(t *testing.T, ctx context.Context, force bool, f *stopFake) (string, bool, error, []string) {
	t.Helper()
	return runStopFakeAt(t, ctx, force, time.Now(), f)
}

// runStopFakeAt is runStopFake with the first look for qemu at started (the graceful path's limits count from it).
func runStopFakeAt(t *testing.T, ctx context.Context, force bool, started time.Time, f *stopFake) (string, bool, error, []string) {
	t.Helper()
	c := &logx.Collector{}
	method, synced, err := runStop(ctx, logx.New(c.Sink()), 3, force, started, f.ops())
	return method, synced, err, c.Messages()
}

// The graceful path syncs before the shutdown command (ModAVD/ModAVD#88: once Android shuts down, adbd is gone and a
// later sync gets "device offline"), and Android then has until gracefulDeadline after the first look.
func TestRunStopGraceful(t *testing.T) {
	f := &stopFake{syncOK: true}
	method, synced, err, warns := runStopFake(t, context.Background(), false, f)
	if err != nil || method != "graceful" || !synced || len(warns) != 0 {
		t.Fatalf("got %s %v %v %v", method, synced, err, warns)
	}
	if got := strings.Join(f.calls, ","); got != "prepare,home,sync,graceful" {
		t.Fatalf("calls = %s", got)
	}
	if !slices.Equal(f.syncTimeouts, []time.Duration{shutdownSyncTimeout}) {
		t.Fatalf("sync timeouts %v", f.syncTimeouts)
	}
	if f.window > gracefulDeadline || f.window < gracefulDeadline-time.Second {
		t.Fatalf("graceful window %v, want about %v", f.window, gracefulDeadline)
	}
	// the window counts from the first look: 5 s spent before the command (starting the private server, the sync)
	// leave Android 5 s less
	f = &stopFake{syncOK: true}
	if _, _, err, _ := runStopFakeAt(t, context.Background(), false, time.Now().Add(-5*time.Second), f); err != nil ||
		f.window > gracefulDeadline-5*time.Second || f.window < gracefulDeadline-6*time.Second {
		t.Fatalf("graceful window %v 5 s after the first look (%v)", f.window, err)
	}
	if f.homeTimeout != homeTimeout {
		t.Fatalf("home timeout %v", f.homeTimeout)
	}
}

// An app in the foreground goes home before the sync and gets homeWait first, which Android's window then lacks; the
// home screen in front costs no wait; a failed HOME is a warning; a cancel during the wait sends no shutdown.
func TestRunStopSendsAppHome(t *testing.T) {
	saved := homeWait
	t.Cleanup(func() { homeWait = saved })
	homeWait = 300 * time.Millisecond

	f := &stopFake{homeApp: true, syncOK: true}
	method, _, err, warns := runStopFake(t, context.Background(), false, f)
	if err != nil || method != "graceful" || len(warns) != 0 || strings.Join(f.calls, ",") != "prepare,home,sync,graceful" {
		t.Fatalf("app: %s %v %q %v", method, err, warns, f.calls)
	}
	if d := f.at["sync"].Sub(f.at["home"]); d < homeWait {
		t.Fatalf("app: synced %v after HOME, want at least %v", d, homeWait)
	}
	if f.window > gracefulDeadline-homeWait {
		t.Fatalf("app: graceful window %v after waiting %v", f.window, homeWait)
	}

	f = &stopFake{syncOK: true}
	if _, _, err, _ := runStopFake(t, context.Background(), false, f); err != nil || f.at["sync"].Sub(f.at["home"]) >= homeWait {
		t.Fatalf("home screen: waited %v (%v)", f.at["sync"].Sub(f.at["home"]), err)
	}

	f = &stopFake{homeFailure: "返回码 1: error: closed", syncOK: true}
	method, _, err, warns = runStopFake(t, context.Background(), false, f)
	if err != nil || method != "graceful" || len(warns) != 1 ||
		warns[0] != "魔改 AVD 实例 3 关机前按 HOME 失败，照常关机: 返回码 1: error: closed" ||
		f.at["sync"].Sub(f.at["home"]) >= homeWait {
		t.Fatalf("failed: %s %v %q", method, err, warns)
	}

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	f = &stopFake{homeApp: true, syncOK: true}
	if _, _, err, _ := runStopFake(t, ctx, false, f); errs.CodeOf(err) != errs.Canceled || strings.Join(f.calls, ",") != "prepare,home" {
		t.Fatalf("canceled: %v %v", err, f.calls)
	}
}

// A failed sync before the shutdown command is a warning; the shutdown goes ahead.
func TestRunStopSyncBeforeShutdownFails(t *testing.T) {
	f := &stopFake{}
	method, synced, err, warns := runStopFake(t, context.Background(), false, f)
	if err != nil || method != "graceful" || !synced {
		t.Fatalf("got %s %v %v", method, synced, err)
	}
	if got := strings.Join(f.calls, ","); got != "prepare,home,sync,graceful" {
		t.Fatalf("calls = %s", got)
	}
	if len(warns) != 1 || warns[0] != "魔改 AVD 实例 3 发关机命令前 sync 失败，照常关机: error: closed" {
		t.Fatalf("warnings = %q", warns)
	}
}

func TestRunStopFallbackSyncKill(t *testing.T) {
	f := &stopFake{gracefulWhy: "24 秒内没有自己退出", syncOK: true}
	// the graceful shutdown ran until its deadline
	method, synced, err, warns := runStopFakeAt(t, context.Background(), false, time.Now().Add(-gracefulDeadline), f)
	if err != nil || method != "sync_kill" || !synced {
		t.Fatalf("got %s %v %v", method, synced, err)
	}
	if got := strings.Join(f.calls, ","); got != "prepare,home,sync,graceful,sync,console_kill,alive" {
		t.Fatalf("calls = %s", got)
	}
	if !slices.Equal(f.syncTimeouts, []time.Duration{shutdownSyncTimeout, fallbackSyncTimeout}) {
		t.Fatalf("sync timeouts %v", f.syncTimeouts)
	}
	// what stopBudget leaves after the deadline and the terminate wait: 28 - 24 - 1
	if want := stopBudget - gracefulDeadline - fallbackForceKillWait; f.killTimeout > want || f.killTimeout < want-time.Second {
		t.Fatalf("kill timeout %v, want about %v", f.killTimeout, want)
	}
	if len(warns) != 1 || warns[0] != "魔改 AVD 实例 3 正常关机没成（24 秒内没有自己退出），退回 sync + 控制台 kill" {
		t.Fatalf("warnings = %q", warns)
	}
}

// Synced on the fallback: the sync before the shutdown command counts, so a fallback whose own sync only gets "device
// offline" (adbd gone, ModAVD/ModAVD#88) still reports synced.
func TestRunStopFallbackSynced(t *testing.T) {
	for _, c := range []struct {
		name    string
		oks     []bool // the sync before the shutdown command, the fallback's
		synced  bool
		warning string
	}{
		{"before the command only", []bool{true, false}, true, "关机前 sync 失败，照常关机: error: closed"},
		{"the fallback's only", []bool{false, true}, true, "发关机命令前 sync 失败，照常关机: error: closed"},
		{"neither", []bool{false, false}, false, "发关机命令前 sync 失败"},
	} {
		f := &stopFake{gracefulWhy: "24 秒内没有自己退出", syncOKs: c.oks}
		method, synced, err, warns := runStopFakeAt(t, context.Background(), false, time.Now().Add(-gracefulDeadline), f)
		if err != nil || method != "sync_kill" || synced != c.synced || !strings.Contains(strings.Join(warns, "\n"), c.warning) {
			t.Errorf("%s: %s synced %v %v %q", c.name, method, synced, err, warns)
		}
	}
}

func TestRunStopFallbackTerminate(t *testing.T) {
	f := &stopFake{gracefulWhy: "24 秒内没有自己退出", killErr: errors.New("连不上控制台 20030"), aliveAfter: true}
	method, synced, err, warns := runStopFakeAt(t, context.Background(), false, time.Now().Add(-gracefulDeadline), f)
	if err != nil || method != "sync_kill+terminate" || synced {
		t.Fatalf("got %s %v %v", method, synced, err)
	}
	if got := strings.Join(f.calls, ","); got != "prepare,home,sync,graceful,sync,console_kill,alive,force_kill" {
		t.Fatalf("calls = %s", got)
	}
	if f.forceKillFor != fallbackForceKillWait {
		t.Fatalf("force kill wait %v", f.forceKillFor)
	}
	// sync before the command failed, graceful failed, sync failed, console kill failed, did not exit
	if len(warns) != 5 || !strings.Contains(warns[0], "发关机命令前 sync 失败，照常关机: error: closed") ||
		!strings.Contains(warns[2], "关机前 sync 失败，照常关机: error: closed") ||
		warns[3] != "实例 3 控制台 kill 失败，改为强制结束: 连不上控制台 20030" ||
		warns[4] != "魔改 AVD 实例 3 未在 3 秒内退出，强制结束" {
		t.Fatalf("warnings = %q", warns)
	}
}

// The graceful path's limits stay within stopBudget, whatever starting the private server, HOME (and the wait after
// it) and the sync before the shutdown command take (at most adbServerStartOnClose, homeTimeout + homeWait,
// shutdownSyncTimeout): Android gets until gracefulDeadline, at least 14 s, and the fallback (sync, console kill,
// terminate wait) fits in the rest.
func TestGracefulBudget(t *testing.T) {
	if homeWait != 2*time.Second {
		t.Fatalf("homeWait = %v; check the shutdown constants in manager.go", homeWait)
	}
	for _, c := range []struct{ elapsed, want time.Duration }{
		{0, gracefulDeadline},
		{6 * time.Second, 18 * time.Second},
		{10 * time.Second, 14 * time.Second},
		{gracefulDeadline + time.Second, 0},
	} {
		if got := gracefulWindow(c.elapsed); got != c.want {
			t.Errorf("gracefulWindow(%v) = %v, want %v", c.elapsed, got, c.want)
		}
	}
	for _, c := range []struct{ elapsed, want time.Duration }{
		{0, fallbackKillMax},
		{gracefulDeadline, 3 * time.Second},                       // its sync failed at once (adbd gone)
		{gracefulDeadline + fallbackSyncTimeout, fallbackKillMin}, // its sync took all its time
		{stopBudget, fallbackKillMin},
	} {
		if got := fallbackKillTimeout(c.elapsed); got != c.want {
			t.Errorf("fallbackKillTimeout(%v) = %v, want %v", c.elapsed, got, c.want)
		}
	}
	worstBefore := adbServerStartOnClose + homeTimeout + homeWait + shutdownSyncTimeout
	for before := time.Duration(0); before <= worstBefore; before += 250 * time.Millisecond {
		window := gracefulWindow(before)
		if window < 14*time.Second {
			t.Errorf("%v before the shutdown command: Android gets %v", before, window)
		}
		for sync := time.Duration(0); sync <= fallbackSyncTimeout; sync += 250 * time.Millisecond {
			fallback := before + window + sync
			if total := fallback + fallbackKillTimeout(fallback) + fallbackForceKillWait; total > stopBudget {
				t.Errorf("%v before the command, fallback sync %v: %v in all", before, sync, total)
			}
		}
	}
}

// The real side effects send what the graceful path is about: HOME (guestHomeScript), `sync`, then svc power shutdown
// (Android's own shutdown, as shell; not su + sys.powerctl), all to the instance on the private server.
func TestStopOpsShutdownCommands(t *testing.T) {
	r, log := portPlanRoot(t)
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	gone := win.Process{PID: 0xFFFFFFFC} // an unused PID: qemu has exited
	ops := m.stopOpsFor(context.Background(), 6, gone)
	if sent, failure := ops.home(homeTimeout); sent || failure != "" {
		t.Fatalf("home screen in front: sent %v, %q", sent, failure)
	}
	t.Setenv(fakeAdbHomeEnv, "app")
	if sent, failure := ops.home(homeTimeout); !sent || failure != "" {
		t.Fatalf("app in front: sent %v, %q", sent, failure)
	}
	if ok, out := ops.sync(shutdownSyncTimeout); !ok {
		t.Fatalf("sync: %s", out)
	}
	if why := ops.graceful(5 * time.Second); why != "" {
		t.Fatalf("graceful: %s", why)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("fake adb log line %q: %v", line, err)
		}
		all := strings.Join(args, " ")
		if !strings.Contains(all, "-s emulator-21060 ") || !strings.HasPrefix(all, fmt.Sprintf("-P %d ", r.AdbServerPort())) {
			t.Errorf("adb %q: not instance 6 on the private server", args)
		}
		if i := slices.Index(args, "shell"); i >= 0 {
			cmds = append(cmds, strings.Join(args[i+1:], " "))
		}
	}
	if !slices.Equal(cmds, []string{guestHomeScript, guestHomeScript, "sync", "svc power shutdown"}) {
		t.Fatalf("shell commands %q", cmds)
	}
}

// svc power shutdown's exit code (measured: adb exits 255 about 2.4 s after it, when the shutdown ends adbd): anything
// but 0 waits for qemu to the end of the window; 0 means it came back by itself and no shutdown is under way, so the
// fallback starts at once instead of after the whole window.
func TestStopOpsShutdownExitCode(t *testing.T) {
	r, _ := portPlanRoot(t)
	m := New(r, logx.Nop())
	six := 6
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	running, ok := win.FindProcess(uint32(os.Getpid())) // a qemu that does not exit
	if !ok {
		t.Fatal("cannot read our own process")
	}
	ops := m.stopOpsFor(context.Background(), 6, running)
	const window = 1500 * time.Millisecond

	t.Setenv(fakeAdbSvcExitEnv, "255")
	began := time.Now()
	why := ops.graceful(window)
	if took := time.Since(began); took < window || why != "2 秒内没有自己退出" {
		t.Errorf("exit 255: %q after %v, want the whole %v", why, took, window)
	}

	t.Setenv(fakeAdbSvcExitEnv, "0")
	began = time.Now()
	why = ops.graceful(window)
	if took := time.Since(began); took >= window || !strings.HasPrefix(why, "关机命令自己返回了，安卓没有开始关机") {
		t.Errorf("exit 0: %q after %v, want it at once", why, took)
	}
}

func TestRunStopConsoleOnly(t *testing.T) {
	f := &stopFake{problem: "私有 adb server（端口 20050）4 秒内没起来", aliveAfter: true}
	method, synced, err, warns := runStopFake(t, context.Background(), false, f)
	if err != nil || method != "console_kill+terminate" || synced {
		t.Fatalf("got %s %v %v", method, synced, err)
	}
	// No graceful shutdown and no sync without the private adb.
	if got := strings.Join(f.calls, ","); got != "prepare,console_kill,alive,force_kill" {
		t.Fatalf("calls = %s", got)
	}
	if f.killTimeout != consoleKillTimeout+closeTimeout || f.forceKillFor != forceKillWait {
		t.Fatalf("timeouts kill %v force %v", f.killTimeout, f.forceKillFor)
	}
	if !strings.HasPrefix(warns[0], "魔改 AVD 实例 3 私有 adb 用不了，不能正常关机也不能 sync，直接用控制台关: 私有 adb server") {
		t.Fatalf("warnings = %q", warns)
	}
	f = &stopFake{problem: "x"}
	if method, _, _, _ := runStopFake(t, context.Background(), false, f); method != "console_kill" {
		t.Fatalf("exited after the console kill: %s", method)
	}
}

func TestRunStopForce(t *testing.T) {
	f := &stopFake{syncOK: true}
	method, synced, err, _ := runStopFake(t, context.Background(), true, f)
	if err != nil || method != "force" || !synced {
		t.Fatalf("got %s %v %v", method, synced, err)
	}
	if got := strings.Join(f.calls, ","); got != "prepare,sync,force_kill" {
		t.Fatalf("calls = %s", got)
	}
	if f.syncTimeout != syncTimeout || f.forceKillFor != forceKillWait {
		t.Fatalf("timeouts sync %v force %v", f.syncTimeout, f.forceKillFor)
	}
	// Private adb unusable: no sync attempt, still killed.
	f = &stopFake{problem: "adb 端口 20031 上不是这台实例的模拟器，不替它登记"}
	method, synced, err, warns := runStopFake(t, context.Background(), true, f)
	if err != nil || method != "force" || synced || strings.Join(f.calls, ",") != "prepare,force_kill" {
		t.Fatalf("got %s %v %v %v", method, synced, err, f.calls)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "关机前 sync 失败，照常关机: adb 端口 20031") {
		t.Fatalf("warnings = %q", warns)
	}
}

// A cancel during the graceful shutdown must not escalate to a kill without sync.
func TestRunStopCancelNeverEscalates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &stopFake{gracefulWhy: "24 秒内没有自己退出", onGraceful: cancel}
	_, _, err, _ := runStopFake(t, ctx, false, f)
	if errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("err = %v", err)
	}
	if got := strings.Join(f.calls, ","); got != "prepare,home,sync,graceful" {
		t.Fatalf("calls = %s", got)
	}

	// canceled during the sync before the shutdown command: the command is not sent
	ctx, cancel = context.WithCancel(context.Background())
	f = &stopFake{syncOK: true, onSync: cancel}
	if _, _, err, _ := runStopFake(t, ctx, false, f); errs.CodeOf(err) != errs.Canceled || strings.Join(f.calls, ",") != "prepare,home,sync" {
		t.Fatalf("canceled during the sync: err %v calls %v", err, f.calls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	f = &stopFake{problem: "已取消"}
	if _, _, err, _ := runStopFake(t, ctx, false, f); errs.CodeOf(err) != errs.Canceled || strings.Join(f.calls, ",") != "prepare" {
		t.Fatalf("console path: err %v calls %v", err, f.calls)
	}
	f = &stopFake{}
	if _, _, err, _ := runStopFake(t, ctx, true, f); errs.CodeOf(err) != errs.Canceled || strings.Contains(fmt.Sprint(f.calls), "force_kill") {
		t.Fatalf("force path: err %v calls %v", err, f.calls)
	}
}

// A terminate Windows refuses is an error on every path, never a success result: AUTO-MAS's close raises then
// (psutil.AccessDenied), and a success would report a qemu that keeps running as stopped. A terminated qemu that is
// merely still exiting is a success (forceKill returns nil then, see TestQemuKillRun).
func TestRunStopQemuStillRunning(t *testing.T) {
	denied := qemuNotEnded(3, 4242, errors.New("Access is denied."))
	f := &stopFake{syncOK: true, forceKillErr: denied}
	method, synced, err, _ := runStopFake(t, context.Background(), true, f)
	if err != denied || method != "force" || !synced || strings.Join(f.calls, ",") != "prepare,sync,force_kill" {
		t.Fatalf("force: %s %v %v %v", method, synced, err, f.calls)
	}

	f = &stopFake{gracefulWhy: "15 秒内没有自己退出", syncOK: true, aliveAfter: true, forceKillErr: denied}
	method, synced, err, _ = runStopFake(t, context.Background(), false, f)
	if err != denied || method != "sync_kill+terminate" || !synced {
		t.Fatalf("sync_kill: %s %v %v", method, synced, err)
	}

	f = &stopFake{problem: "x", aliveAfter: true, forceKillErr: denied}
	if method, _, err, _ := runStopFake(t, context.Background(), false, f); err != denied || method != "console_kill+terminate" {
		t.Fatalf("console_kill: %s %v", method, err)
	}

	// still exiting after the terminate: forceKill succeeded, so the stop did
	f = &stopFake{gracefulWhy: "15 秒内没有自己退出", syncOK: true, aliveAfter: true}
	if method, _, err, _ := runStopFake(t, context.Background(), false, f); err != nil || method != "sync_kill+terminate" {
		t.Fatalf("still exiting: %s %v", method, err)
	}
}

func TestQemuNotEnded(t *testing.T) {
	cause := errors.New("Access is denied.")
	err := qemuNotEnded(3, 4242, cause)
	if errs.CodeOf(err) != errs.Internal || errs.ExitCode(err) != 1 || !errors.Is(err, cause) ||
		!strings.HasPrefix(err.Error(), "魔改 AVD 实例 3 的 qemu（pid 4242）没能结束：Access is denied.。") ||
		!strings.Contains(err.Error(), "管理员") {
		t.Fatalf("denied: %v (%s)", err, errs.CodeOf(err))
	}
}

// killScript drives qemuKill.run: the answers of successive alive() calls (the last one repeats), KillTree's error,
// whether qemu exits within the wait and whether Windows refuses to open it for terminating.
type killScript struct {
	alive   []bool
	killErr error
	exits   bool
	refused bool
	calls   []string
	waited  time.Duration
}

func (k *killScript) run(t *testing.T, ctx context.Context, wait time.Duration) ([]string, error) {
	t.Helper()
	c := &logx.Collector{}
	q := qemuKill{
		alive: func() bool {
			k.calls = append(k.calls, "alive")
			a := k.alive[0]
			if len(k.alive) > 1 {
				k.alive = k.alive[1:]
			}
			return a
		},
		kill:     func() error { k.calls = append(k.calls, "kill"); return k.killErr },
		waitExit: func(d time.Duration) bool { k.calls = append(k.calls, "wait"); k.waited = d; return k.exits },
		refused:  func() bool { k.calls = append(k.calls, "refused"); return k.refused },
	}
	err := q.run(ctx, logx.New(c.Sink()), 3, 4242, wait)
	return c.Messages(), err
}

// AUTO-MAS _force_kill: only a qemu Windows will not even open for terminating, still running after the wait, is an
// error; terminated-but-still-exiting is a warning, and a kill error from a qemu already exiting is a success.
func TestQemuKillRun(t *testing.T) {
	ctx := context.Background()
	denied := errors.New("结束qemu-system-x86_64.exe（PID 4242）失败: Access is denied.")
	stillExiting := "魔改 AVD 实例 3 的 qemu（pid 4242）已强制结束，还在退出（等了 2 秒）；稍后用 modavd status 3 确认它已退出"
	cases := []struct {
		name   string
		script killScript
		calls  string
		code   string // errs code of the result, "" for success
		warns  []string
	}{
		{"already gone", killScript{alive: []bool{false}}, "alive", "", nil},
		{"terminated, exits", killScript{alive: []bool{true}, exits: true}, "alive,kill,wait", "", nil},
		{"terminated, still exiting", killScript{alive: []bool{true}}, "alive,kill,wait", "", []string{stillExiting}},
		{"kill error, exits within the wait", killScript{alive: []bool{true}, killErr: denied, exits: true},
			"alive,kill,wait", "", nil},
		// STATUS_PROCESS_IS_TERMINATING: we may open it for terminating, so it is on its way out
		{"kill error, already terminating", killScript{alive: []bool{true}, killErr: denied},
			"alive,kill,wait,refused", "", []string{stillExiting}},
		{"refused, still running", killScript{alive: []bool{true}, killErr: denied, refused: true},
			"alive,kill,wait,refused,alive", errs.Internal, nil},
		{"refused, gone meanwhile", killScript{alive: []bool{true, false}, killErr: denied, refused: true},
			"alive,kill,wait,refused,alive", "", nil},
	}
	for _, c := range cases {
		k := c.script
		warns, err := k.run(t, ctx, 2*time.Second)
		if errs.CodeOf(err) != c.code || strings.Join(k.calls, ",") != c.calls || !slices.Equal(warns, c.warns) {
			t.Errorf("%s: err %v (%q) calls %v warns %q", c.name, err, errs.CodeOf(err), k.calls, warns)
		}
		if slices.Contains(k.calls, "wait") && k.waited != 2*time.Second {
			t.Errorf("%s: waited %v", c.name, k.waited)
		}
		if c.code == errs.Internal && (!errors.Is(err, denied) || !strings.Contains(err.Error(), "管理员")) {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	// A cancel during the wait is a cancel, whatever the kill did.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	k := killScript{alive: []bool{true}, killErr: denied, refused: true}
	if _, err := k.run(t, cctx, forceKillWait); errs.CodeOf(err) != errs.Canceled || strings.Join(k.calls, ",") != "alive,kill,wait" {
		t.Fatalf("canceled: %v %v", err, k.calls)
	}
}

func TestTerminateRefused(t *testing.T) {
	if terminateRefused(win.Process{}) {
		t.Error("PID 0")
	}
	if terminateRefused(win.Process{PID: uint32(os.Getpid())}) {
		t.Error("this very process can be opened for terminating")
	}
	// PIDs are multiples of 4 well below this one: ERROR_INVALID_PARAMETER, gone.
	if terminateRefused(win.Process{PID: 0xFFFFFFFC}) {
		t.Error("an unused PID")
	}
	// System (PID 4) refuses PROCESS_TERMINATE unless SeDebugPrivilege is enabled. Only opened, never terminated.
	if h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, 4); err == nil {
		syscall.CloseHandle(h)
		t.Skip("this process may open System for terminating (SeDebugPrivilege enabled)")
	}
	if !terminateRefused(win.Process{PID: 4}) {
		t.Error("System")
	}
	// With a creation time: a system process that refuses PROCESS_TERMINATE but whose creation time is readable
	// (services.exe, lsass.exe ...). Again only opened.
	procs, err := win.ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	var denied win.Process
	for _, p := range procs {
		if p.PID == 0 || p.Created == 0 {
			continue
		}
		h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, p.PID)
		if err == nil {
			syscall.CloseHandle(h)
			continue
		}
		if err == syscall.ERROR_ACCESS_DENIED {
			denied = p
			break
		}
	}
	if denied.PID == 0 {
		t.Skip("no process here refuses PROCESS_TERMINATE with a readable creation time")
	}
	if !terminateRefused(denied) {
		t.Errorf("%s (pid %d) with its creation time", denied.Name, denied.PID)
	}
	// Another creation time: the process we meant is gone, the PID names a newer one.
	if terminateRefused(win.Process{PID: denied.PID, Created: denied.Created + 1}) {
		t.Errorf("%s (pid %d) as a reused PID", denied.Name, denied.PID)
	}
}

// TestStopHelperSleeper is the child process TestForceKillRealProcess terminates, not a test of its own.
func TestStopHelperSleeper(t *testing.T) {
	if os.Getenv("MODAVD_TEST_STOP_SLEEPER") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(time.Minute)
}

// The real forceKill on a child of this test (never an emulator): terminated and gone, no warning.
func TestForceKillRealProcess(t *testing.T) {
	m, logs := opsTestManager(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestStopHelperSleeper$")
	cmd.Env = append(os.Environ(), "MODAVD_TEST_STOP_SLEEPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	p, ok := win.FindProcess(uint32(cmd.Process.Pid))
	if !ok {
		t.Fatal("child not found")
	}
	ops := m.stopOpsFor(context.Background(), 3, p)
	if err := ops.forceKill(forceKillWait); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		done <- nil // for the cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("child still running after forceKill")
	}
	if ops.alive() {
		t.Fatal("alive after forceKill")
	}
	if err := ops.forceKill(forceKillWait); err != nil {
		t.Fatalf("forceKill of a gone process: %v", err)
	}
	if w := logs.Messages(); len(w) != 0 {
		t.Fatalf("warnings: %q", w)
	}
}

func TestLaunchingKillTimeout(t *testing.T) {
	for _, c := range []struct{ elapsed, want time.Duration }{
		{0, consoleKillTimeout + closeTimeout},
		{8 * time.Second, consoleKillTimeout + closeTimeout},
		{9 * time.Second, 14 * time.Second},
		{10 * time.Second, 13 * time.Second},
		{25 * time.Second, consoleKillTimeout},
	} {
		if got := launchingKillTimeout(c.elapsed); got != c.want {
			t.Errorf("launchingKillTimeout(%v) = %v, want %v", c.elapsed, got, c.want)
		}
	}
	// The whole launching path stays within the budget for every wait the default startingQemuWait allows (plus
	// the time to take the lock back and look once more).
	const wait = 10 * time.Second
	if startingQemuWait != wait {
		t.Fatalf("startingQemuWait = %v; check the formula in stop.go's header", startingQemuWait)
	}
	for w := time.Duration(0); w <= wait+time.Second; w += 250 * time.Millisecond {
		if total := w + launchingKillTimeout(w) + forceKillWait; total > stopBudget {
			t.Errorf("wait %v: %v in all", w, total)
		}
	}
}

// The launching path: no private adb, no graceful shutdown, no sync, Force or not; straight to the console kill, under
// its own method name (console_kill means a booted guest whose private adb was unusable).
func TestRunLaunchingStop(t *testing.T) {
	f := &stopFake{}
	c := &logx.Collector{}
	method, synced, err := runLaunchingStop(context.Background(), logx.New(c.Sink()), 3, 13*time.Second, f.ops())
	if err != nil || method != "launching_kill" || synced || strings.Join(f.calls, ",") != "console_kill,alive" ||
		f.killTimeout != 13*time.Second || len(c.Messages()) != 0 {
		t.Fatalf("exits: %s %v %v %v %v %q", method, synced, err, f.calls, f.killTimeout, c.Messages())
	}

	f = &stopFake{killErr: errors.New("连不上控制台 20030"), aliveAfter: true}
	method, _, err = runLaunchingStop(context.Background(), logx.Nop(), 3, 15*time.Second, f.ops())
	if err != nil || method != "launching_kill+terminate" || strings.Join(f.calls, ",") != "console_kill,alive,force_kill" ||
		f.forceKillFor != forceKillWait {
		t.Fatalf("terminate: %s %v %v %v", method, err, f.calls, f.forceKillFor)
	}
	// Windows refuses the terminate: an error, never a success.
	denied := qemuNotEnded(3, 4242, errors.New("Access is denied."))
	f = &stopFake{aliveAfter: true, forceKillErr: denied}
	if method, _, err := runLaunchingStop(context.Background(), logx.Nop(), 3, 15*time.Second, f.ops()); err != denied ||
		method != "launching_kill+terminate" {
		t.Fatalf("refused: %s %v", method, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f = &stopFake{}
	if _, _, err := runLaunchingStop(ctx, logx.Nop(), 3, 15*time.Second, f.ops()); errs.CodeOf(err) != errs.Canceled || len(f.calls) != 0 {
		t.Fatalf("canceled: %v %v", err, f.calls)
	}
}

// stopTestSetup is a Manager with instance i, a start lock held by the test (returned, so a test can let go of it
// like a start that failed; Unlock is idempotent), short waits, and stopOpsOf answering with f (no emulator, no adb).
// scan replaces the process scan.
func stopTestSetup(t *testing.T, f *stopFake, scan func() map[int]win.Process) (*Manager, int, *win.FileLock) {
	t.Helper()
	m, _ := opsTestManager(t)
	created, err := m.Create(context.Background(), CreateRequest{filesOnly: true})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	i := created.Index
	savedWait, savedPoll, savedScan, savedOps := startingQemuWait, startingQemuPoll, qemuScan, stopOpsOf
	t.Cleanup(func() {
		startingQemuWait, startingQemuPoll, qemuScan, stopOpsOf = savedWait, savedPoll, savedScan, savedOps
	})
	startingQemuWait, startingQemuPoll = 3*time.Second, 20*time.Millisecond
	qemuScan = func([]int) map[int]win.Process { return scan() }
	stopOpsOf = func(_ *Manager, _ context.Context, _ int, _ win.Process) stopOps { return f.ops() }
	l, ok, err := win.TryLock(m.lockPath(i, "start"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	t.Cleanup(l.Unlock)
	return m, i, l
}

// A qemu that appears while the stop waits for the start goes straight to the console kill: no private adb, no
// graceful shutdown and no sync (adbd is not up yet), even with Force. Method launching_kill, the stop lock held
// through the wait, and the result names that qemu.
func TestStopKillsQemuLaunchedDuringWait(t *testing.T) {
	var looks atomic.Int32
	var heldDuringWait atomic.Bool
	var m *Manager
	var i int
	qemu := win.Process{PID: 4242, Name: "qemu-system-x86_64-headless.exe", Created: filetimeOf(time.Now())}
	f := &stopFake{}
	m, i, _ = stopTestSetup(t, f, func() map[int]win.Process {
		// 1: Stop's first look; 2, 3: the wait; 4: it appears
		n := looks.Add(1)
		if n == 2 {
			heldDuringWait.Store(m.lockHeld(i, "stop"))
		}
		if n >= 4 {
			return map[int]win.Process{i: qemu}
		}
		return map[int]win.Process{}
	})
	st, err := m.Stop(context.Background(), i, StopOptions{Force: true})
	if err != nil || st.Method != "launching_kill" || st.Synced || len(st.Warnings) != 0 {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if st.PID != 4242 || st.Process != qemu {
		t.Fatalf("qemu of the result: pid %d process %+v", st.PID, st.Process)
	}
	if got := strings.Join(f.calls, ","); got != "console_kill,alive" {
		t.Fatalf("calls = %s", got)
	}
	// a quick wait leaves the console path its full share
	if f.killTimeout != consoleKillTimeout+closeTimeout {
		t.Fatalf("kill timeout %v", f.killTimeout)
	}
	if !heldDuringWait.Load() {
		t.Fatal("the stop lock was let go while waiting for the start's qemu")
	}
	if m.lockHeld(i, "stop") {
		t.Fatal("stop lock still held")
	}

	// It had to be terminated in the end (stopOpsOf answers with *f, so change it in place).
	looks.Store(0)
	*f = stopFake{aliveAfter: true}
	st, err = m.Stop(context.Background(), i, StopOptions{})
	if err != nil || st.Method != "launching_kill+terminate" || strings.Join(f.calls, ",") != "console_kill,alive,force_kill" ||
		f.forceKillFor != forceKillWait {
		t.Fatalf("terminate: %+v %v %v %v", st, err, f.calls, f.forceKillFor)
	}
}

func TestJustLaunched(t *testing.T) {
	waitStarted := time.Now()
	for _, c := range []struct {
		name    string
		created uint64
		booting bool
		want    bool
	}{
		{"boot lock held", 0, true, true},
		{"created during the wait", filetimeOf(waitStarted.Add(3 * time.Second)), false, true},
		{"created just before (clock slack)", filetimeOf(waitStarted.Add(-time.Second)), false, true},
		{"there all along", filetimeOf(waitStarted.Add(-10 * time.Minute)), false, false},
		{"creation time unknown", 0, false, false},
	} {
		p := win.Process{PID: 1, Created: c.created}
		if got := justLaunched(p, waitStarted, c.booting); got != c.want {
			t.Errorf("%s: justLaunched = %v, want %v", c.name, got, c.want)
		}
	}
}

// The first look finds nothing (a process snapshot can fail), the start lock is held, and the wait then finds a qemu
// that was running long before: that may be a booted guest, so it must get the normal path with prepare / graceful /
// sync, never the sync-less launching_kill.
func TestStopQemuFoundDuringWaitButOldGetsNormalPath(t *testing.T) {
	var looks atomic.Int32
	old := win.Process{PID: 4343, Name: "qemu-system-x86_64-headless.exe", Created: filetimeOf(time.Now().Add(-time.Hour))}
	f := &stopFake{gracefulWhy: ""}
	m, i, _ := stopTestSetup(t, f, func() map[int]win.Process {
		if looks.Add(1) >= 2 {
			return map[int]win.Process{0: old, 1: old, 2: old, 3: old, 4: old, 6: old, 7: old, 8: old, 9: old}
		}
		return map[int]win.Process{}
	})
	st, err := m.Stop(context.Background(), i, StopOptions{})
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if strings.HasPrefix(st.Method, "launching_kill") {
		t.Fatalf("an old qemu went without a sync: %+v", st)
	}
	if len(f.calls) == 0 || f.calls[0] != "prepare" {
		t.Fatalf("calls = %v, want the normal path starting with prepare", f.calls)
	}
}

// A second stop that comes while the first waits for the start's qemu waits for the first one's lock instead of
// slipping in between: the first ends the qemu (launching_kill; the start then fails and lets go), and the second then
// finds the instance offline.
func TestStopWaitingForStartBlocksOtherStops(t *testing.T) {
	var looks atomic.Int32
	var appeared, ended atomic.Bool
	var m *Manager
	var i int
	var startLock *win.FileLock
	qemu := win.Process{PID: 4242, Created: filetimeOf(time.Now())}
	f := &stopFake{onKill: func() {
		ended.Store(true)
		startLock.Unlock() // its qemu is gone: the start fails and lets go
	}}
	m, i, startLock = stopTestSetup(t, f, func() map[int]win.Process {
		looks.Add(1)
		if appeared.Load() && !ended.Load() {
			return map[int]win.Process{i: qemu}
		}
		return map[int]win.Process{}
	})
	type outcome struct {
		st   *StopResult
		err  error
		done time.Time
	}
	second := make(chan outcome, 1)
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for looks.Load() < 2 { // the first stop is waiting
			if time.Now().After(deadline) {
				second <- outcome{err: errors.New("the first stop never waited")}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		go func() {
			time.Sleep(300 * time.Millisecond) // the second stop is blocked on the lock by now
			appeared.Store(true)
		}()
		st, err := m.Stop(context.Background(), i, StopOptions{})
		second <- outcome{st, err, time.Now()}
	}()
	st, err := m.Stop(context.Background(), i, StopOptions{})
	firstDone := time.Now()
	o := <-second
	if err != nil || st.Method != "launching_kill" || st.PID != 4242 {
		t.Fatalf("first stop: %+v %v", st, err)
	}
	if o.err != nil || o.st.Method != "already_offline" || o.st.PID != 0 || o.st.Process != (win.Process{}) {
		t.Fatalf("second stop: %+v %v", o.st, o.err)
	}
	if o.done.Before(firstDone.Add(-50 * time.Millisecond)) {
		t.Fatalf("the second stop finished %v before the first", firstDone.Sub(o.done))
	}
	if got := strings.Join(f.calls, ","); got != "console_kill,alive" {
		t.Fatalf("calls = %s", got)
	}
}

// A qemu that runs at the first look takes the normal path, and the result names it: the PID in JSON, the process
// (with its creation time) only for callers in this process.
func TestStopReportsItsQemu(t *testing.T) {
	var i int
	qemu := win.Process{PID: 4242, Name: "qemu-system-x86_64-headless.exe", Created: 77}
	f := &stopFake{syncOK: true}
	m, i, _ := stopTestSetup(t, f, func() map[int]win.Process { return map[int]win.Process{i: qemu} })
	st, err := m.Stop(context.Background(), i, StopOptions{})
	if err != nil || st.Method != "graceful" || !st.Synced || st.PID != 4242 || st.Process != qemu {
		t.Fatalf("stop: %+v %v", st, err)
	}
	if got := strings.Join(f.calls, ","); got != "prepare,home,sync,graceful" {
		t.Fatalf("calls = %s", got)
	}
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if s := string(b); !strings.Contains(s, `"pid":4242`) || strings.Contains(s, "Created") || strings.Contains(s, `"process"`) {
		t.Fatalf("json: %s", s)
	}
}

func TestWaitStartingQemu(t *testing.T) {
	ctx := context.Background()
	qemu := win.Process{PID: 4242}
	none := func() (win.Process, bool) { return win.Process{}, false }
	always := func() bool { return true }
	const poll = 10 * time.Millisecond

	// The qemu appears on the third look while the start holds its lock.
	looks := 0
	find := func() (win.Process, bool) { looks++; return qemu, looks >= 3 }
	p, found, starting, err := waitStartingQemu(ctx, 5*time.Second, poll, find, always)
	if err != nil || !found || starting || p.PID != 4242 || looks != 3 {
		t.Fatalf("appears: %+v %v %v %v looks %d", p, found, starting, err, looks)
	}

	// The start lets go without a qemu (it failed before launching): simply offline.
	held := 3
	_, found, starting, err = waitStartingQemu(ctx, 5*time.Second, poll, none, func() bool { held--; return held > 0 })
	if err != nil || found || starting {
		t.Fatalf("start ended: %v %v %v", found, starting, err)
	}

	// The start lets go right after launching: the look after it finds the qemu.
	looks = 0
	find = func() (win.Process, bool) { looks++; return qemu, looks >= 2 }
	if _, found, starting, _ := waitStartingQemu(ctx, 5*time.Second, poll, find, func() bool { return false }); !found || starting {
		t.Fatalf("launched then let go: %v %v", found, starting)
	}

	// Still starting when the wait is over.
	started := time.Now()
	_, found, starting, err = waitStartingQemu(ctx, 100*time.Millisecond, 20*time.Millisecond, none, always)
	if err != nil || found || !starting || time.Since(started) < 100*time.Millisecond {
		t.Fatalf("still starting: %v %v %v after %v", found, starting, err, time.Since(started))
	}

	cctx, cancel := context.WithCancel(ctx)
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, _, _, err := waitStartingQemu(cctx, 5*time.Second, poll, none, always); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

// A stop while a start holds the start lock but has no qemu yet is Busy, not already_offline (the start would boot
// the instance right after); it holds the stop lock all the while, so a start that has not got past its stop-lock
// check cannot launch before the stop is over. Offline once the start lets go without a qemu.
func TestStopWhileStartHasNotLaunched(t *testing.T) {
	m, _ := opsTestManager(t)
	ctx := context.Background()
	created, err := m.Create(ctx, CreateRequest{filesOnly: true})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	i := created.Index
	savedWait, savedPoll := startingQemuWait, startingQemuPoll
	startingQemuWait, startingQemuPoll = 400*time.Millisecond, 20*time.Millisecond
	defer func() { startingQemuWait, startingQemuPoll = savedWait, savedPoll }()

	l, ok, err := win.TryLock(m.lockPath(i, "start"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	defer l.Unlock()
	// What a start that comes after the stop does before it launches (Start: m.lock(i, "stop", ...)): it gets the
	// stop lock only once the stop's whole wait is over.
	type lateStart struct {
		heldMidWait bool
		got         time.Time
		err         error
	}
	late := make(chan lateStart, 1)
	go func() {
		time.Sleep(200 * time.Millisecond) // the stop is waiting by now
		held := m.lockHeld(i, "stop")
		sl, ok, err := win.Lock(m.lockPath(i, "stop"), 5*time.Second)
		if err != nil || !ok {
			late <- lateStart{held, time.Time{}, fmt.Errorf("stop lock: %v %v", ok, err)}
			return
		}
		got := time.Now()
		sl.Unlock()
		late <- lateStart{held, got, nil}
	}()
	began := time.Now()
	_, err = m.Stop(ctx, i, StopOptions{})
	if errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在开机，模拟器还没起来，稍后再关") {
		t.Fatalf("stop while starting: %v", err)
	}
	ls := <-late
	if ls.err != nil {
		t.Fatal(ls.err)
	}
	if !ls.heldMidWait {
		t.Fatal("the stop lock was not held while waiting for the start's qemu")
	}
	if d := ls.got.Sub(began); d < startingQemuWait {
		t.Fatalf("a later start got past the stop lock %v into the stop's %v wait", d, startingQemuWait)
	}

	go func() { time.Sleep(100 * time.Millisecond); l.Unlock() }()
	st, err := m.Stop(ctx, i, StopOptions{})
	if err != nil || st.Method != "already_offline" || st.PID != 0 || st.Process != (win.Process{}) {
		t.Fatalf("after the start let go: %+v %v", st, err)
	}
}
