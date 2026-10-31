//go:build windows

package emu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// TestSetupMarkerMatches: only the exact qemu process of the boot the marker names (PID + creation time), or, while
// only its emulator.exe is recorded, a child of that emulator.exe that is not older than it.
func TestSetupMarkerMatches(t *testing.T) {
	q := win.Process{PID: 4000, PPID: 3000, Name: "qemu-system-x86_64-headless.exe", Created: 1_000_500}
	full := setupMarker{QemuPID: 4000, QemuCreated: 1_000_500, EmulatorPID: 3000, EmulatorCreated: 1_000_000}
	early := setupMarker{EmulatorPID: 3000, EmulatorCreated: 1_000_000}
	cases := []struct {
		name string
		mk   setupMarker
		q    win.Process
		want bool
	}{
		{"same qemu", full, q, true},
		{"reused pid", full, win.Process{PID: 4000, PPID: 3000, Created: 2_000_000}, false},
		{"other qemu", full, win.Process{PID: 4100, PPID: 3000, Created: 1_000_500}, false},
		// qemu recorded: the parent rule no longer applies (another qemu of the same emulator.exe is no match)
		{"recorded qemu wins", full, win.Process{PID: 4100, PPID: 3000, Created: 1_000_600}, false},
		{"child of the emulator", early, q, true},
		{"child of another emulator", early, win.Process{PID: 4000, PPID: 3100, Created: 1_000_500}, false},
		{"older than the emulator", early, win.Process{PID: 4000, PPID: 3000, Created: 999_000}, false},
		{"creation time unknown", full, win.Process{PID: 4000, PPID: 3000}, false},
		{"empty marker", setupMarker{}, q, false},
		{"emulator creation unknown", setupMarker{EmulatorPID: 3000}, q, false},
	}
	for _, c := range cases {
		if got := c.mk.matches(c.q); got != c.want {
			t.Errorf("%s: matches = %v, want %v", c.name, got, c.want)
		}
	}
}

// filetimeOf is t as a process creation time (win.Process.Created: FILETIME, 100 ns since 1601).
func filetimeOf(t time.Time) uint64 {
	ft := syscall.NsecToFiletime(t.UnixNano())
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

// TestSetupMarkerWindow: the window of a start counts its own boot timeout, never less than the default one.
func TestSetupMarkerWindow(t *testing.T) {
	// DefaultBootTimeout (5 min) + 10 min: the default window, and the fallback for markers without valid_until
	if SetupMarkerMaxAge != 15*time.Minute || SetupMarkerGrace != 10*time.Minute {
		t.Fatalf("SetupMarkerMaxAge = %v, SetupMarkerGrace = %v", SetupMarkerMaxAge, SetupMarkerGrace)
	}
	cases := []struct{ timeout, want time.Duration }{
		{DefaultBootTimeout, 15 * time.Minute},
		{time.Minute, 15 * time.Minute}, // a quick timeout keeps the time to call start again
		{DefaultBootTimeout + time.Second, 15*time.Minute + time.Second},
		{30 * time.Minute, 40 * time.Minute},
		{time.Hour, 70 * time.Minute}, // MCP's largest timeout_s
	}
	for _, c := range cases {
		if got := setupMarkerWindow(c.timeout); got != c.want {
			t.Errorf("setupMarkerWindow(%v) = %v, want %v", c.timeout, got, c.want)
		}
	}
}

// TestSetupMarkerUse: a marker is picked up only for its own boot and only before its deadline: valid_until (set by
// the start that launched the boot from its own timeout), else SetupMarkerMaxAge after qemu started (markers of an
// older modavd). A marker past it is stale (deleted), one of another boot is simply not ours.
func TestSetupMarkerUse(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC)
	qemuAt := func(age time.Duration) win.Process {
		return win.Process{PID: 4000, PPID: 3000, Name: "qemu-system-x86_64-headless.exe", Created: filetimeOf(now.Add(-age))}
	}
	markerOf := func(q win.Process) setupMarker {
		return setupMarker{QemuPID: q.PID, QemuCreated: q.Created, EmulatorPID: 3000, EmulatorCreated: q.Created - 1}
	}
	// the marker a start with this timeout writes once it has found qemu
	launchedWith := func(q win.Process, timeout time.Duration) setupMarker {
		mk := markerOf(q)
		mk.ValidUntil = q.StartedAt().Add(setupMarkerWindow(timeout)).Unix()
		return mk
	}
	young := qemuAt(2 * time.Minute)
	cases := []struct {
		name       string
		mk         setupMarker
		ok         bool
		q          win.Process
		use, stale bool
	}{
		// markers without valid_until: SetupMarkerMaxAge
		{"old marker, 2 min old", markerOf(young), true, young, true, false},
		{"old marker, just under the limit", markerOf(qemuAt(SetupMarkerMaxAge - time.Second)),
			true, qemuAt(SetupMarkerMaxAge - time.Second), true, false},
		{"old marker, at the limit", markerOf(qemuAt(SetupMarkerMaxAge)), true, qemuAt(SetupMarkerMaxAge), false, true},
		{"old marker, 2 h old", markerOf(qemuAt(2 * time.Hour)), true, qemuAt(2 * time.Hour), false, true},
		// the host clock went back: a boot "from the future" is still this one
		{"old marker, clock skew", markerOf(qemuAt(-time.Minute)), true, qemuAt(-time.Minute), true, false},
		{"only the emulator recorded", setupMarker{EmulatorPID: 3000, EmulatorCreated: young.Created - 1}, true, young,
			true, false},
		// valid_until from the launching start's own timeout
		{"default timeout, 14 min", launchedWith(qemuAt(14*time.Minute), DefaultBootTimeout), true,
			qemuAt(14 * time.Minute), true, false},
		{"default timeout, 15 min", launchedWith(qemuAt(15*time.Minute), DefaultBootTimeout), true,
			qemuAt(15 * time.Minute), false, true},
		{"30 min timeout, 20 min", launchedWith(qemuAt(20*time.Minute), 30*time.Minute), true,
			qemuAt(20 * time.Minute), true, false},
		{"30 min timeout, 32 min (it has just timed out)", launchedWith(qemuAt(32*time.Minute), 30*time.Minute), true,
			qemuAt(32 * time.Minute), true, false},
		{"30 min timeout, just under 40 min", launchedWith(qemuAt(40*time.Minute-time.Second), 30*time.Minute), true,
			qemuAt(40*time.Minute - time.Second), true, false},
		{"30 min timeout, 40 min", launchedWith(qemuAt(40*time.Minute), 30*time.Minute), true,
			qemuAt(40 * time.Minute), false, true},
		{"60 min timeout, 69 min", launchedWith(qemuAt(69*time.Minute), time.Hour), true, qemuAt(69 * time.Minute),
			true, false},
		{"60 min timeout, 2 h", launchedWith(qemuAt(2*time.Hour), time.Hour), true, qemuAt(2 * time.Hour), false, true},
		{"1 min timeout, 14 min", launchedWith(qemuAt(14*time.Minute), time.Minute), true, qemuAt(14 * time.Minute),
			true, false},
		// valid_until wins over SetupMarkerMaxAge both ways: a short one is not stretched to 15 minutes either
		{"valid_until before 15 min", func() setupMarker {
			mk := markerOf(qemuAt(10 * time.Minute))
			mk.ValidUntil = now.Add(-time.Minute).Unix()
			return mk
		}(), true, qemuAt(10 * time.Minute), false, true},
		{"30 min timeout, another boot", launchedWith(qemuAt(time.Hour), 30*time.Minute), true, young, false, false},
		{"no marker", setupMarker{}, false, young, false, false},
		{"unreadable marker", markerOf(young), false, young, false, false},
	}
	for _, c := range cases {
		use, stale := setupMarkerUse(c.mk, c.ok, c.q, now)
		if use != c.use || stale != c.stale {
			t.Errorf("%s: setupMarkerUse = %v %v, want %v %v", c.name, use, stale, c.use, c.stale)
		}
	}
}

// TestSetupMarkerDeadline: valid_until when set, else SetupMarkerMaxAge after qemu started, else unknown.
func TestSetupMarkerDeadline(t *testing.T) {
	started := time.Date(2026, 10, 28, 17, 0, 0, 0, time.UTC)
	q := win.Process{PID: 4000, Created: filetimeOf(started)}
	mk := setupMarker{QemuPID: 4000, QemuCreated: q.Created}
	if got := mk.deadline(q); !got.Equal(started.Add(SetupMarkerMaxAge)) {
		t.Errorf("without valid_until: %v", got)
	}
	if !mk.until().IsZero() {
		t.Errorf("until without valid_until: %v", mk.until())
	}
	mk.ValidUntil = started.Add(40 * time.Minute).Unix()
	if got := mk.deadline(q); !got.Equal(started.Add(40 * time.Minute)) {
		t.Errorf("with valid_until: %v", got)
	}
	if got := (setupMarker{}).deadline(win.Process{PID: 4000}); !got.IsZero() {
		t.Errorf("nothing known: %v", got)
	}
}

// TestClockText: local HH:MM, the date only when it is another day; seconds are cut, never rounded up.
func TestClockText(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.Local)
	cases := []struct {
		t    time.Time
		want string
	}{
		{time.Date(2026, 10, 28, 18, 15, 59, 0, time.Local), "18:15"},
		{time.Date(2026, 10, 28, 9, 5, 0, 0, time.Local), "09:05"},
		{time.Date(2026, 10, 29, 0, 5, 0, 0, time.Local), "10 月 29 日 00:05"},
		{time.Date(2027, 1, 2, 3, 4, 0, 0, time.Local), "1 月 2 日 03:04"},
		{time.Time{}, ""},
	}
	for _, c := range cases {
		if got := clockText(c.t, now); got != c.want {
			t.Errorf("clockText(%v) = %q, want %q", c.t, got, c.want)
		}
	}
	// the zone of t does not matter: it is shown as local time
	if got := clockText(time.Date(2026, 10, 28, 18, 15, 0, 0, time.Local).UTC(), now); got != "18:15" {
		t.Errorf("UTC input = %q", got)
	}
}

// TestBootSetupZero: a mode left unset is the safe one (an ordinary adopt: the Vulkan fix is only checked).
func TestBootSetupZero(t *testing.T) {
	var unset bootSetup
	if unset != setupAdopted || setupLaunched == setupAdopted || setupOwed == setupAdopted {
		t.Errorf("bootSetup values: %d %d %d", setupAdopted, setupLaunched, setupOwed)
	}
}

func markerTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(r.AvdHome(), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Manager{Root: r, Log: logx.Nop(), ADB: adb.New(r, logx.Nop())}
}

func TestSetupMarkerFile(t *testing.T) {
	m := markerTestManager(t)
	path := m.setupMarkerPath(3)
	if filepath.Dir(path) != m.Root.AvdHome() || filepath.Base(path) != ".mas_3.setup" {
		t.Fatalf("marker path = %s", path)
	}
	if _, ok := m.readSetupMarker(3); ok {
		t.Fatal("a missing marker was read")
	}

	mk := setupMarker{EmulatorPID: 3000, EmulatorCreated: 1_000_000, LogFile: `D:\r\logs\mas_3-20261028-150405.log`,
		ValidUntil: time.Date(2026, 10, 28, 18, 15, 0, 0, time.UTC).Unix()}
	m.writeSetupMarker(3, mk)
	got, ok := m.readSetupMarker(3)
	if !ok || got != mk {
		t.Fatalf("read back %+v %v, want %+v", got, ok, mk)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), `"valid_until":`) {
		t.Errorf("marker file = %s", b)
	}
	mk.QemuPID, mk.QemuCreated = 4000, 1_000_500
	mk.ValidUntil += 2
	m.writeSetupMarker(3, mk)
	if got, ok = m.readSetupMarker(3); !ok || got != mk {
		t.Fatalf("after the qemu update %+v %v, want %+v", got, ok, mk)
	}
	// a marker of an older modavd has no valid_until
	if err := os.WriteFile(path, []byte(`{"qemu_pid":4000,"qemu_created":1000500}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := m.readSetupMarker(3); !ok || got.ValidUntil != 0 || got.QemuPID != 4000 {
		t.Errorf("older marker read as %+v %v", got, ok)
	}
	entries, _ := os.ReadDir(m.Root.AvdHome())
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
	if _, ok := m.readSetupMarker(4); ok {
		t.Error("another instance's marker was read")
	}

	// a torn or foreign file counts as no marker
	for _, text := range []string{`{"qemu_pid": 40`, `{}`, `[1, 2]`, ``} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		if got, ok := m.readSetupMarker(3); ok {
			t.Errorf("%q read as %+v", text, got)
		}
	}

	m.removeSetupMarker(3)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("marker still there: %v", err)
	}
	m.removeSetupMarker(3) // already gone: nothing to warn about

	// removeLockFiles (after Delete) takes the marker along
	m.writeSetupMarker(3, mk)
	m.removeLockFiles(3)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("removeLockFiles left the marker: %v", err)
	}
}

func TestWithSetupAdvice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := ctxErr(ctx)
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.Local)
	advice := setupAdvice(now.Add(25*time.Minute), now)
	err := withSetupAdvice(canceled, advice)
	e := errs.As(err)
	if e.Code != errs.Canceled || e.Advice != advice || !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %+v (cause kept: %v)", e, errors.Is(err, context.Canceled))
	}
	if !strings.Contains(err.Error(), "再启动一次这个实例") {
		t.Errorf("message = %q", err.Error())
	}
	if errs.As(canceled).Advice != "" {
		t.Error("the original error was changed")
	}

	timeout := errs.New(errs.Timeout, "启动超时").WithAdvice("先看日志")
	if got := errs.As(withSetupAdvice(timeout, advice)); got.Code != errs.Timeout || got.Advice != "先看日志；"+advice {
		t.Errorf("timeout: %+v", got)
	}
	// the emulator is gone: nothing to finish
	gone := errs.New(errs.LaunchFailed, "退出了")
	if got := withSetupAdvice(gone, advice); got != error(gone) || gone.Advice != "" {
		t.Errorf("launch failed: %v", got)
	}
	if withSetupAdvice(nil, advice) != nil {
		t.Error("nil error")
	}
}

// TestSetupAdvice: the advice names the marker's actual deadline as local time (a long timeout_s gives a later one),
// says that after it the fix waits for the next boot, and that the zygote restart waits for an empty foreground and
// no install.
func TestSetupAdvice(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.Local)
	const rules = "这个补丁要重启 zygote，只在前台没有应用、也没有安装正在进行时才挂"
	cases := []struct {
		name  string
		until time.Time
		has   []string
		not   []string
	}{
		{"default timeout", now.Add(12 * time.Minute), []string{"模拟器没有关：在 18:12 前再启动一次这个实例，就会接着做完这次开机后的设置",
			"过了这个时间客体 Vulkan 驱动补丁就等下次开机再挂", rules}, []string{"15 分钟"}},
		// a start with timeout_s 1800 that timed out: qemu start + 30 + 10 min
		{"30 min timeout", now.Add(10*time.Minute + 30*time.Second), []string{"在 18:10 前再启动一次这个实例", rules}, nil},
		{"past midnight", time.Date(2026, 10, 29, 0, 20, 0, 0, time.Local), []string{"在 10 月 29 日 00:20 前再启动一次这个实例"}, nil},
		{"deadline passed", now.Add(-time.Minute), []string{"模拟器没有关；这次开机接着做设置的期限（17:59）已过，客体 Vulkan 驱动补丁等下次开机再挂"},
			[]string{"前再启动一次"}},
		{"deadline unknown", time.Time{}, []string{"再启动一次这个实例，就会接着做完这次开机后的设置", rules}, []string{"前再启动一次"}},
	}
	for _, c := range cases {
		got := setupAdvice(c.until, now)
		for _, s := range c.has {
			if !strings.Contains(got, s) {
				t.Errorf("%s: advice lacks %q: %s", c.name, s, got)
			}
		}
		for _, s := range c.not {
			if strings.Contains(got, s) {
				t.Errorf("%s: advice has %q: %s", c.name, s, got)
			}
		}
	}
}

// TestQemuAfterSetup: a process the scan no longer finds is gone only when its own PID + creation time check says so
// (a failed snapshot is no exit).
func TestQemuAfterSetup(t *testing.T) {
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok || self.Created == 0 {
		t.Fatal("cannot read our own process")
	}
	other := win.Process{PID: 99, Created: 1}
	if p, gone := qemuAfterSetup(other, self, true); gone || p != self {
		t.Errorf("found now: %+v %v", p, gone)
	}
	if p, gone := qemuAfterSetup(self, win.Process{}, false); gone || p != self {
		t.Errorf("snapshot missed a live one: %+v %v", p, gone)
	}
	reused := self
	reused.Created++ // our PID with another creation time: that process has exited
	if p, gone := qemuAfterSetup(reused, win.Process{}, false); !gone || p != reused {
		t.Errorf("exited: %+v %v", p, gone)
	}
	if _, gone := qemuAfterSetup(win.Process{}, win.Process{}, false); !gone {
		t.Error("no process at all is not gone")
	}
}

// TestProgressor: MCP requires progress to grow strictly, whatever the steps report; only "done" reaches the total.
func TestProgressor(t *testing.T) {
	var got []Progress
	p := &progressor{fn: func(pr Progress) { got = append(got, pr) }}
	p.emit("launch", "a", progressLaunch)
	p.emit("boot", "b", bootProgressFrom)
	p.emit("boot", "c", bootProgressFrom) // same value again
	p.emit("tune", "d", 10)               // lower than before
	p.emit("vulkan_fix", "e", progressTotal+5)
	p.emit("done", "f", progressTotal)
	want := []int{progressLaunch, bootProgressFrom, bootProgressFrom + 1, bootProgressFrom + 2, progressTotal - 1,
		progressTotal}
	if len(got) != len(want) {
		t.Fatalf("got %d notifications, want %d", len(got), len(want))
	}
	for k, pr := range got {
		if pr.Done != want[k] || pr.Total != progressTotal {
			t.Errorf("#%d %s: %d/%d, want %d/%d", k, pr.Step, pr.Done, pr.Total, want[k], progressTotal)
		}
	}
	if got[0].Step != "launch" || got[0].Message != "a" || got[5].Step != "done" {
		t.Errorf("steps / messages: %+v", got)
	}

	// no callback, no progressor: nothing happens
	(&progressor{}).emit("boot", "x", 1)
	var none *progressor
	none.emit("boot", "x", 1)
}

func TestProgressOrder(t *testing.T) {
	order := []int{progressLaunch, bootProgressFrom, bootProgressTo, progressBooted, progressTune, progressIme,
		progressFirstBoot, progressVulkanFix, progressTotal}
	for k := 1; k < len(order); k++ {
		if order[k] <= order[k-1] {
			t.Errorf("progress constants out of order at %d: %v", k, order)
		}
	}
}

func TestSleepOrDone(t *testing.T) {
	if !sleepOrDone(context.Background(), time.Millisecond) {
		t.Error("an uncanceled sleep reported done")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if sleepOrDone(ctx, time.Minute) || time.Since(start) > 5*time.Second {
		t.Error("a canceled sleep waited")
	}
	if err := ctxErr(ctx); errs.As(err) == nil || errs.As(err).Code != errs.Canceled {
		t.Errorf("ctxErr = %v", err)
	}
}

// TestPollUntil: the /sdcard wait's loop. Stops at the first true; gives up after its limit (a timeout is only a
// warning for start); a canceled start does not probe at all.
func TestPollUntil(t *testing.T) {
	ctx := context.Background()
	n := 0
	if !pollUntil(ctx, time.Second, time.Millisecond, func() bool { n++; return n == 3 }) || n != 3 {
		t.Fatalf("true on the third probe: n = %d", n)
	}
	n = 0
	t0 := time.Now()
	if pollUntil(ctx, 30*time.Millisecond, 5*time.Millisecond, func() bool { n++; return false }) {
		t.Fatal("never true, yet reported true")
	}
	if d := time.Since(t0); d < 30*time.Millisecond || d > 2*time.Second || n < 2 {
		t.Fatalf("gave up after %v and %d probes", d, n)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	n = 0
	if pollUntil(cctx, time.Second, time.Millisecond, func() bool { n++; return true }) || n != 0 {
		t.Fatalf("canceled: probed %d times", n)
	}
}
