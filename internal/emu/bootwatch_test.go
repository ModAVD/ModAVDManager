//go:build windows

package emu

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/win"
)

const (
	testBootA = "6f1c2d3e-4a5b-4c6d-8e7f-0123456789ab"
	testBootB = "0a9b8c7d-6e5f-4a3b-9c2d-1e0f2a3b4c5d"
)

func TestParseGuestBoot(t *testing.T) {
	cases := []struct {
		name, out string
		booted    bool
		id        string
	}{
		{"booted", "1\n" + testBootA + "\n", true, testBootA},
		{"crlf", "1\r\n" + testBootA + "\r\n", true, testBootA},
		{"still booting", "\n" + testBootB, false, testBootB},
		{"upper case", "1\n" + strings.ToUpper(testBootA), true, testBootA},
		{"boot_id unreadable", "1\ncat: /proc/sys/kernel/random/boot_id: Permission denied", true, ""},
		{"adb error", "error: device offline", false, ""},
		{"device missing", "adb: device 'emulator-20030' not found", false, ""},
		{"nothing", "", false, ""},
		// the property is the first line only: a "1" further down is no boot
		{"one later", testBootA + "\n1", false, testBootA},
		{"dashes elsewhere", "1\n6f1c2d3e4-a5b-4c6d-8e7f-0123456789ab", true, ""},
		{"not hex", "1\n6f1c2d3e-4a5b-4c6d-8e7f-0123456789xz", true, ""},
	}
	for _, c := range cases {
		if booted, id := parseGuestBoot(c.out); booted != c.booted || id != c.id {
			t.Errorf("%s: parseGuestBoot = %v %q, want %v %q", c.name, booted, id, c.booted, c.id)
		}
	}
	if !strings.Contains(guestBootScript, "/proc/sys/kernel/random/boot_id") || strings.Contains(guestBootScript, "'") {
		t.Errorf("guestBootScript = %q", guestBootScript)
	}
}

// A guest-only reboot is the recorded qemu process (PID and creation time) running another kernel boot. An unknown
// boot_id on either side is no reboot: nothing is done on a guess.
func TestBootRecordGuestRebooted(t *testing.T) {
	q := win.Process{PID: 4000, Created: 1_000_500}
	rec := bootRecord{QemuPID: 4000, QemuCreated: 1_000_500, BootID: testBootA}
	cases := []struct {
		name   string
		rec    bootRecord
		q      win.Process
		bootID string
		names  bool
		reboot bool
	}{
		{"same boot", rec, q, testBootA, true, false},
		{"guest rebooted", rec, q, testBootB, true, true},
		{"boot_id unknown now", rec, q, "", true, false},
		{"boot_id not recorded", bootRecord{QemuPID: 4000, QemuCreated: 1_000_500}, q, testBootB, true, false},
		{"another qemu", rec, win.Process{PID: 4100, Created: 1_000_500}, testBootB, false, false},
		{"reused pid", rec, win.Process{PID: 4000, Created: 2_000_000}, testBootB, false, false},
		{"creation time unknown", rec, win.Process{PID: 4000}, testBootB, false, false},
		{"empty record", bootRecord{}, win.Process{}, testBootB, false, false},
	}
	for _, c := range cases {
		if got := c.rec.names(c.q); got != c.names {
			t.Errorf("%s: names = %v, want %v", c.name, got, c.names)
		}
		if got := c.rec.guestRebooted(c.q, c.bootID); got != c.reboot {
			t.Errorf("%s: guestRebooted = %v, want %v", c.name, got, c.reboot)
		}
	}
	if shortBootID(testBootA) != "6f1c2d3e" || shortBootID("") != "?" {
		t.Errorf("shortBootID = %q %q", shortBootID(testBootA), shortBootID(""))
	}
}

func TestBootRecordFile(t *testing.T) {
	m := markerTestManager(t)
	path := m.bootRecordPath(3)
	if filepath.Dir(path) != m.Root.AvdHome() || filepath.Base(path) != ".mas_3.boot" {
		t.Fatalf("record path = %s", path)
	}
	if _, ok := m.readBootRecord(3); ok {
		t.Fatal("a missing record was read")
	}
	rec := bootRecord{QemuPID: 4000, QemuCreated: 1_000_500, BootID: testBootA}
	m.writeBootRecord(3, rec)
	if got, ok := m.readBootRecord(3); !ok || got != rec {
		t.Fatalf("read back %+v %v, want %+v", got, ok, rec)
	}
	rec.BootID = testBootB
	m.writeBootRecord(3, rec)
	if got, ok := m.readBootRecord(3); !ok || got != rec {
		t.Fatalf("after the update %+v %v, want %+v", got, ok, rec)
	}
	if _, ok := m.readBootRecord(4); ok {
		t.Error("another instance's record was read")
	}
	entries, _ := os.ReadDir(m.Root.AvdHome())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
	for _, text := range []string{`{"qemu_pid": 40`, `{}`, `[1]`, ``} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := m.readBootRecord(3); ok {
			t.Errorf("%q read as %+v", text, got)
		}
	}

	// removeLockFiles (after Delete) takes the record and the watch lock along
	m.writeBootRecord(3, rec)
	if err := os.WriteFile(m.lockPath(3, "watch"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m.removeLockFiles(3)
	for _, p := range []string{path, m.lockPath(3, "watch")} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("removeLockFiles left %s: %v", filepath.Base(p), err)
		}
	}
	m.removeBootRecord(3) // already gone: nothing to report
}

// The watch's start never waits: a start or a stop of the instance running makes it Busy at once, and a stop found
// running does not leave the start lock held.
func TestWatchStartLock(t *testing.T) {
	m := markerTestManager(t)
	l, err := m.watchStartLock(3)
	if err != nil {
		t.Fatal(err)
	}
	if !m.lockHeld(3, "start") {
		t.Error("start lock not held")
	}
	started := time.Now()
	if _, err := m.watchStartLock(3); errs.CodeOf(err) != errs.Busy {
		t.Errorf("second start: %v", err)
	}
	if d := time.Since(started); d > time.Second {
		t.Errorf("waited %v for the start lock", d)
	}
	l.Unlock()

	stop, ok, err := win.TryLock(m.lockPath(3, "stop"))
	if err != nil || !ok {
		t.Fatalf("stop lock: %v %v", ok, err)
	}
	if _, err := m.watchStartLock(3); errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在关机") {
		t.Errorf("while stopping: %v", err)
	}
	if m.lockHeld(3, "start") {
		t.Error("start lock left held after a stop was found")
	}
	stop.Unlock()
}

// guestStub is what the stubbed guest read answers, and what it was asked.
type guestStub struct {
	booted, missing bool
	bootID          string
	reads           atomic.Int32
	reregisters     []bool // the reregister argument of each read
}

// stubGuestBoot replaces the guest read for the test.
func stubGuestBoot(t *testing.T) *guestStub {
	t.Helper()
	g := &guestStub{}
	saved := guestBootRead
	guestBootRead = func(_ int, reregister bool) (bool, string, bool) {
		g.reads.Add(1)
		g.reregisters = append(g.reregisters, reregister)
		return g.booted, g.bootID, g.missing
	}
	t.Cleanup(func() { guestBootRead = saved })
	return g
}

// One look of the watch: no guest read while a start or a stop runs or while the record does not name its qemu; a
// start only for a booted guest with another boot_id. That start adopts exactly the watched qemu, so with the qemu
// gone it fails without launching anything.
func TestBootWatchTick(t *testing.T) {
	m, logs := opsTestManager(t)
	created, err := m.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	i := created.Index
	ctx := context.Background()
	q := win.Process{PID: 4242, Created: 1_000_500}
	g := stubGuestBoot(t)
	g.booted, g.bootID = true, testBootA
	warnings := logs.Messages
	bw := &bootWatch{m: m, i: i, q: q}
	tick := func() { bw.tick(ctx, time.Now()) }

	tick() // no record
	m.writeBootRecord(i, bootRecord{QemuPID: 4100, QemuCreated: 1_000_500, BootID: testBootA})
	tick() // record of another qemu
	m.writeBootRecord(i, bootRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: testBootA})
	for _, kind := range []string{"start", "stop"} {
		l, ok, err := win.TryLock(m.lockPath(i, kind))
		if err != nil || !ok {
			t.Fatalf("%s lock: %v %v", kind, ok, err)
		}
		tick()
		l.Unlock()
	}
	if n := g.reads.Load(); n != 0 {
		t.Fatalf("%d guest reads before the record named the qemu or while locked", n)
	}

	tick() // same boot
	g.booted, g.bootID = false, testBootB
	tick() // rebooting, not booted yet
	g.booted, g.bootID = true, ""
	tick() // boot_id unreadable
	if n := g.reads.Load(); n != 3 {
		t.Fatalf("guest reads = %d, want 3", n)
	}
	if w := warnings(); len(w) != 0 {
		t.Fatalf("started without a reboot: %q", w)
	}

	g.booted, g.bootID = true, testBootB
	tick()
	w := warnings()
	if len(w) != 1 || !strings.Contains(w[0], "补做开机后调优没做完") || !strings.Contains(w[0], "已不在运行") {
		t.Fatalf("after the reboot: %q", w)
	}
	if _, ok := m.readSetupMarker(i); ok {
		t.Error("the failed start left a setup marker")
	}
	if rec, _ := m.readBootRecord(i); rec.BootID != testBootA {
		t.Errorf("record = %+v, want the old boot kept", rec)
	}
	if m.lockHeld(i, "start") {
		t.Error("start lock still held")
	}
}

// The watch reads without re-registering: during a guest reboot the instance is off the private server for about a
// second and the emulator registers it again itself. Only an instance missing for bootWatchReregisterAfter (a
// restarted server) is registered again, and then at most that often.
func TestBootWatchReregisterSchedule(t *testing.T) {
	m := markerTestManager(t)
	q := win.Process{PID: 4242, Created: 1_000_500}
	m.writeBootRecord(3, bootRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: testBootA})
	g := stubGuestBoot(t)
	g.bootID = testBootA
	bw := &bootWatch{m: m, i: 3, q: q}
	t0 := time.Date(2026, 10, 29, 1, 0, 0, 0, time.UTC)
	at := func(d time.Duration) { bw.tick(context.Background(), t0.Add(d)) }

	g.missing = true
	at(0)                                      // gone: noted
	at(5 * time.Second)                        // a reboot's second off the server: left to the emulator
	at(bootWatchReregisterAfter)               // missing that long: registered again
	at(bootWatchReregisterAfter + time.Second) // that did not help: not again at once
	at(2 * bootWatchReregisterAfter)           // only after another period
	g.missing = false
	at(2*bootWatchReregisterAfter + 5*time.Second) // back
	g.missing = true
	at(2*bootWatchReregisterAfter + 10*time.Second) // gone again: the period starts over
	at(2*bootWatchReregisterAfter + 15*time.Second)
	want := []bool{false, false, true, false, true, false, false, false}
	if len(g.reregisters) != len(want) {
		t.Fatalf("reads %v, want %v", g.reregisters, want)
	}
	for k := range want {
		if g.reregisters[k] != want[k] {
			t.Fatalf("reregister per read = %v, want %v", g.reregisters, want)
		}
	}
}

// A start for the boot watch never launches: the watched qemu not running (or another one running) is LaunchFailed
// before anything else, and nothing is written.
func TestStartForWatchNeverLaunches(t *testing.T) {
	m, i := opsTestInstance(t)
	q := win.Process{PID: 4242, Created: 1_000_500}
	_, err := m.Start(context.Background(), i, StartOptions{watchQemu: &q})
	if errs.CodeOf(err) != errs.LaunchFailed || !strings.Contains(err.Error(), "pid 4242") {
		t.Fatalf("start: %v (%s)", err, errs.CodeOf(err))
	}
	if _, ok := m.readSetupMarker(i); ok {
		t.Error("setup marker written")
	}
	if entries, _ := os.ReadDir(m.Root.LogsDir()); len(entries) != 0 {
		t.Errorf("logs written: %v", entries)
	}
}

func TestBootWatchArgvAndLog(t *testing.T) {
	q := win.Process{PID: 4242, Created: 133_000_000_000_000_000}
	argv, err := bootWatchArgv(`D:\r`, 3, q)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{BootWatchArg, `D:\r`, "3", "4242", "133000000000000000"}
	if len(argv) != 6 || strings.Join(argv[1:], "|") != strings.Join(want, "|") {
		t.Fatalf("argv = %q", argv)
	}

	m := markerTestManager(t)
	path := filepath.Join(m.Root.Dir, "w.log")
	if err := os.WriteFile(path, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	rotateLog(path, 101)
	if _, err := os.Stat(path); err != nil {
		t.Errorf("small log moved: %v", err)
	}
	rotateLog(path, 100)
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("full log not moved: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("full log still there: %v", err)
	}

	// a watch of this instance running already (its lock held): no second one, no warning
	saved := bootWatchArgv
	t.Cleanup(func() { bootWatchArgv = saved })
	called := false
	bootWatchArgv = func(string, int, win.Process) ([]string, error) {
		called = true
		return nil, errors.New("no executable")
	}
	l, ok, err := win.TryLock(m.lockPath(3, "watch"))
	if err != nil || !ok {
		t.Fatalf("watch lock: %v %v", ok, err)
	}
	m.startBootWatch(3, q)
	l.Unlock()
	if called {
		t.Error("started a second watch")
	}
	m2, logs := opsTestManager(t)
	m2.startBootWatch(3, q)
	if w := logs.Messages(); !called || len(w) != 1 || !strings.Contains(w[0], "开机守护没起来") {
		t.Errorf("failed watch start: called %v, %q", called, w)
	}
}

// TestBootWatchHelperSleeper is the stand-in qemu TestRunBootWatch watches, not a test of its own.
func TestBootWatchHelperSleeper(t *testing.T) {
	if os.Getenv("MODAVD_TEST_BOOTWATCH_SLEEPER") != "1" {
		t.Skip("helper process")
	}
	time.Sleep(time.Minute)
}

// RunBootWatch: bad arguments are 2; a watch of the instance already running, or a qemu already gone, end it at
// once; otherwise it holds the watch lock until the watched process exits.
func TestRunBootWatch(t *testing.T) {
	savedHelper, savedInterval := win.OrphanHelper, bootWatchInterval
	t.Cleanup(func() { win.OrphanHelper, bootWatchInterval = savedHelper, savedInterval })
	bootWatchInterval = 20 * time.Millisecond
	m, _ := opsTestManager(t)
	dir := m.Root.Dir

	for _, args := range [][]string{nil, {dir, "3", "4242"}, {dir, "5", "4242", "1"}, {dir, "x", "4242", "1"},
		{dir, "3", "0", "1"}, {dir, "3", "4242", "0"}, {dir, "3", "-1", "1"}} {
		if code := RunBootWatch(args, io.Discard); code != 2 {
			t.Errorf("RunBootWatch(%q) = %d, want 2", args, code)
		}
	}
	if code := RunBootWatch([]string{filepath.Join(dir, "missing"), "3", "4242", "1"}, io.Discard); code == 0 {
		t.Error("a missing root was accepted")
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBootWatchHelperSleeper$")
	cmd.Env = append(os.Environ(), "MODAVD_TEST_BOOTWATCH_SLEEPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-exited })
	q, ok := win.FindProcess(uint32(cmd.Process.Pid))
	if !ok {
		t.Fatal("child not found")
	}
	args := func(p win.Process) []string {
		argv, err := bootWatchArgv(dir, 3, p)
		if err != nil {
			t.Fatal(err)
		}
		return argv[2:]
	}

	// another watch holds the lock: this one ends at once
	l, ok, err := win.TryLock(m.lockPath(3, "watch"))
	if err != nil || !ok {
		t.Fatalf("watch lock: %v %v", ok, err)
	}
	if code := runBootWatchWithin(t, args(q), 2*time.Second); code != 0 {
		t.Errorf("second watch = %d", code)
	}
	l.Unlock()
	// the same PID with another creation time is not the watched process
	if code := runBootWatchWithin(t, args(win.Process{PID: q.PID, Created: q.Created + 1}), 2*time.Second); code != 0 {
		t.Errorf("gone qemu = %d", code)
	}
	if m.lockHeld(3, "watch") {
		t.Fatal("watch lock left held")
	}

	done := make(chan int, 1)
	go func() { done <- RunBootWatch(args(q), io.Discard) }()
	deadline := time.Now().Add(5 * time.Second)
	for !m.lockHeld(3, "watch") {
		if time.Now().After(deadline) {
			t.Fatal("the watch never took its lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case code := <-done:
		t.Fatalf("watch ended while its process runs: %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	cmd.Process.Kill()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("watch = %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch still running after its process exited")
	}
	if m.lockHeld(3, "watch") {
		t.Error("watch lock still held after the watch ended")
	}
	if want, _ := os.Executable(); win.OrphanHelper != want {
		t.Errorf("OrphanHelper = %q", win.OrphanHelper)
	}
}

// runBootWatchWithin runs RunBootWatch and fails the test when it takes longer than d.
func runBootWatchWithin(t *testing.T, args []string, d time.Duration) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- RunBootWatch(args, io.Discard) }()
	select {
	case code := <-done:
		return code
	case <-time.After(d):
		t.Fatalf("RunBootWatch(%q) still running after %v", args, d)
		return -1
	}
}
