//go:build windows

package emu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/win"
)

func lockStart(t *testing.T, m *Manager, i int) (unlock func()) {
	t.Helper()
	l, ok, err := win.TryLock(m.lockPath(i, "start"))
	if err != nil || !ok {
		t.Fatalf("start lock: %v %v", ok, err)
	}
	released := false
	unlock = func() {
		if !released {
			released = true
			l.Unlock()
		}
	}
	t.Cleanup(unlock)
	return unlock
}

// infoOf is Info of instance i with the qemu scan finding q and the guest reporting Android booted, as status does.
func infoOf(t *testing.T, m *Manager, i int, q *win.Process) InstanceInfo {
	t.Helper()
	savedScan, savedBooted := qemuScan, statusBooted
	t.Cleanup(func() { qemuScan, statusBooted = savedScan, savedBooted })
	qemuScan = func([]int) map[int]win.Process {
		if q == nil {
			return map[int]win.Process{}
		}
		return map[int]win.Process{i: *q}
	}
	statusBooted = func(*Manager, context.Context, int) (bool, bool) { return true, true }
	info, err := m.Info(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	return *info
}

func setCap(t *testing.T, d time.Duration) {
	t.Helper()
	saved := bootSetupCap
	bootSetupCap = d
	t.Cleanup(func() { bootSetupCap = saved })
}

// fakeClock replaces the counter the deadlines are on; the returned pointer is its value in milliseconds.
func fakeClock(t *testing.T, start int64) *atomic.Int64 {
	t.Helper()
	var now atomic.Int64
	now.Store(start)
	saved := readyClockMS
	readyClockMS = now.Load
	t.Cleanup(func() { readyClockMS = saved })
	return &now
}

// ownerOfTests is this process as the record names its writer.
func ownerOfTests(t *testing.T) (uint32, uint64) {
	t.Helper()
	me, ok := host.FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("cannot read self")
	}
	return me.PID, me.Created
}

// putRecord writes a record naming q, written by this process, with the deadline cap from the counter now unless mut
// changes it.
func putRecord(t *testing.T, m *Manager, i int, q win.Process, mut func(*readyRecord)) readyRecord {
	t.Helper()
	pid, created := ownerOfTests(t)
	rec := readyRecord{QemuPID: q.PID, QemuCreated: q.Created, OwnerPID: pid, OwnerCreated: created,
		DeadlineMS: readyClockMS() + bootSetupCap.Milliseconds(), Step: "tune", Percent: 40}
	if mut != nil {
		mut(&rec)
	}
	tr := &readyTracker{m: m, i: i, rec: rec}
	tr.mu.Lock()
	err := tr.write()
	tr.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// TestBootSetupOf: what the readers (status, list, info) make of the record, the start lock and the setup marker.
func TestBootSetupOf(t *testing.T) {
	setCap(t, time.Minute)
	clock := fakeClock(t, 5_000_000)
	m := markerTestManager(t)
	const i = 3
	q := win.Process{PID: 4242, PPID: 3000, Created: 1_000_500}
	other := win.Process{PID: 4243, Created: 1_000_900}
	now := time.Now()
	deadAt := func() win.Process { // a process that is gone
		dead, kill := startSleeper(t)
		kill()
		return dead
	}
	marker := func(until time.Time, alive int64) {
		t.Helper()
		m.writeSetupMarker(i, setupMarker{QemuPID: q.PID, QemuCreated: q.Created, ValidUntil: until.Unix(), AliveMS: alive})
	}
	reset := func() {
		m.removeReady(i)
		m.removeSetupMarker(i)
		clock.Store(5_000_000)
	}
	left := func(d time.Duration) func(*readyRecord) {
		return func(r *readyRecord) { r.DeadlineMS = clock.Load() + d.Milliseconds() }
	}
	var idReads int
	sameBoot := func() string { idReads++; return testBootA }
	otherBoot := func() string { idReads++; return testBootB }

	for _, c := range []struct {
		name  string
		setup func()
		held  bool
		boot  func() string
		want  bootSetupState
	}{
		{"qemu no modavd start works on: nothing to wait for", func() {}, false, nil, bootSetupState{Ready: true}},
		{"adopt of a qemu from another tool: no marker, no record", func() {}, true, nil, bootSetupState{Ready: true}},
		{"launched, record not written yet: marker, heartbeat and the start running", func() { marker(now.Add(time.Hour), clock.Load()-3000) },
			true, nil, bootSetupState{Step: readyStepBoot}},
		{"launched, the heartbeat is older than the cap: not waited for (not the marker's own deadline)",
			func() { marker(now.Add(time.Hour), clock.Load()-60_000) }, true, nil, bootSetupState{Ready: true}},
		{"launched, marker without a heartbeat", func() { marker(now.Add(time.Hour), 0) }, true, nil, bootSetupState{Ready: true}},
		{"launched, but the start is gone: nobody finishes it", func() { marker(now.Add(time.Hour), clock.Load()) }, false, nil,
			bootSetupState{Ready: true}},
		{"marker past its deadline", func() { marker(now.Add(-time.Minute), clock.Load()) }, true, nil, bootSetupState{Ready: true}},
		{"setup running", func() { putRecord(t, m, i, q, nil) }, true, sameBoot, bootSetupState{Step: "tune", Progress: 40}},
		{"setup running, the start was cancelled or killed", func() { putRecord(t, m, i, q, nil) }, false, sameBoot,
			bootSetupState{Ready: true}},
		{"setup running, the process that wrote the record is gone", func() {
			dead := deadAt()
			putRecord(t, m, i, q, func(r *readyRecord) { r.OwnerPID, r.OwnerCreated = dead.PID, dead.Created })
		}, true, sameBoot, bootSetupState{Ready: true}},
		{"setup running past the cap", func() { putRecord(t, m, i, q, left(-time.Second)) }, true, sameBoot,
			bootSetupState{Ready: true, Unfinished: "tune"}},
		{"setup running, the deadline is on the dot", func() { putRecord(t, m, i, q, left(0)) }, true, sameBoot,
			bootSetupState{Ready: true, Unfinished: "tune"}},
		{"setup done", func() { putRecord(t, m, i, q, func(r *readyRecord) { r.Done = true }) }, true, otherBoot, bootSetupState{Ready: true}},
		{"record of another qemu", func() { putRecord(t, m, i, other, nil) }, true, nil, bootSetupState{Ready: true}},
		{"record of another qemu, marker of this one", func() {
			putRecord(t, m, i, other, nil)
			marker(now.Add(time.Hour), clock.Load())
		}, true, nil, bootSetupState{Step: readyStepBoot}},
		{"record of this qemu's earlier PID reuse", func() {
			putRecord(t, m, i, win.Process{PID: q.PID, Created: q.Created + 1}, nil)
		}, true, nil, bootSetupState{Ready: true}},
		{"the guest restarted during the setup: the record is of the boot before", func() {
			putRecord(t, m, i, q, func(r *readyRecord) { r.BootID = testBootA })
		}, true, otherBoot, bootSetupState{Ready: true}},
		{"same boot_id", func() { putRecord(t, m, i, q, func(r *readyRecord) { r.BootID = testBootA }) }, true, sameBoot,
			bootSetupState{Step: "tune", Progress: 40}},
		{"boot_id unreadable: the record holds", func() { putRecord(t, m, i, q, func(r *readyRecord) { r.BootID = testBootA }) }, true,
			func() string { return "" }, bootSetupState{Step: "tune", Progress: 40}},
	} {
		reset()
		c.setup()
		idReads = 0
		got := m.bootSetupOf(i, q, c.held, now, c.boot, "")
		if got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
		if strings.Contains(c.name, "setup done") || c.name == "qemu no modavd start works on: nothing to wait for" {
			if idReads != 0 {
				t.Errorf("%s: the guest was asked for its boot_id", c.name)
			}
		}
	}
	reset()
	if err := os.WriteFile(m.readyPath(i), []byte(`{"qemu_pid":4242,"qemu_cre`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.bootSetupOf(i, q, true, now, nil, ""); !got.Ready {
		t.Errorf("a torn record held the instance back: %+v", got)
	}
}

// The deadline is on the machine's counter, set once: a wall clock set back or forward (the counter is not the wall
// clock) cannot lengthen the wait, release it early or turn an "online" back into "starting"; a counter that is not
// the one the deadline was set on (restarted machine, damaged record) is no wait either.
func TestReadyDeadlineIsOnTheCounter(t *testing.T) {
	setCap(t, time.Minute)
	clock := fakeClock(t, 9_000_000)
	m := markerTestManager(t)
	const i = 3
	q := win.Process{PID: 4242, Created: 1_000_500}
	tr := m.beginReady(i, q, testBootA, "")
	defer tr.close()
	rec, ok := m.readReady(i)
	if !ok || rec.DeadlineMS != 9_000_000+60_000 {
		t.Fatalf("deadline %d, want the counter + the cap", rec.DeadlineMS)
	}
	state := func() bootSetupState { return m.bootSetupOf(i, q, true, time.Now(), nil, "") }
	if s := state(); s.Ready || s.Step != readyStepBoot {
		t.Fatalf("at the start: %+v", s)
	}
	clock.Store(9_000_000 + 59_999)
	if s := state(); s.Ready {
		t.Fatalf("1 ms before the deadline: %+v", s)
	}
	// the wall clock changes do not touch the counter: nothing to do here but read the same number
	clock.Store(9_000_000 + 60_000)
	if s := state(); !s.Ready || s.Unfinished != readyStepBoot {
		t.Fatalf("at the deadline: %+v", s)
	}
	// a reading that is not on this record's timeline (the machine restarted: the counter is small again; a damaged
	// record) does not bring "starting" back
	for _, at := range []int64{9_000_000 - 3_600_000, 1_000, 9_000_000 + 3_600_000} {
		clock.Store(at)
		if s := state(); !s.Ready {
			t.Errorf("counter at %d: %+v, want released", at, s)
		}
	}
}

// The cap's log line goes by the same deadline: a timer that fires before the counter reached it waits again.
func TestReadyCapLogFollowsTheDeadline(t *testing.T) {
	setCap(t, 120*time.Millisecond)
	clock := fakeClock(t, 700_000)
	logs := &logx.Collector{}
	m := markerTestManager(t)
	m.Log = logx.New(logs.Sink())
	q := win.Process{PID: 4242, Created: 1_000_500}
	tr := m.beginReady(3, q, "", "")
	defer tr.close()
	tr.step("ime")
	time.Sleep(400 * time.Millisecond) // the real timer has fired; the counter has not moved
	if msgs := logs.Messages(); len(msgs) != 0 {
		t.Fatalf("logged before the counter reached the deadline: %q", msgs)
	}
	clock.Store(700_000 + 120)
	deadline := time.Now().Add(5 * time.Second)
	for len(logs.Messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if msgs := logs.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "宿主输入法") {
		t.Fatalf("after the deadline: %q", msgs)
	}
}

// TestReadyTrackerFile: the record names the qemu and its writer, only moves forward, ends Done, and a tracker that is
// closed without finishing leaves the setup not done. A new tracker deletes the earlier boot's record first.
func TestReadyTrackerFile(t *testing.T) {
	setCap(t, time.Hour)
	m := markerTestManager(t)
	const i = 3
	q := win.Process{PID: 4242, Created: 1_000_500}
	pid, created := ownerOfTests(t)
	tr := m.beginReady(i, q, testBootA, "")
	rec, ok := m.readReady(i)
	if !ok || !rec.names(q) || rec.BootID != testBootA || rec.Step != readyStepBoot || rec.Percent != 0 || rec.Done ||
		rec.OwnerPID != pid || rec.OwnerCreated != created || rec.ownerGone() {
		t.Fatalf("after begin: %+v", rec)
	}
	tr.step("vulkan_fix")
	tr.percent(10, 40)
	tr.percent(5, 40)
	tr.step("")
	rec, _ = m.readReady(i)
	if rec.Step != "vulkan_fix" || rec.Percent != 10*readyAfterBootMax/40 {
		t.Fatalf("after steps: %+v", rec)
	}
	tr.step(readyStepTune)
	if rec, _ = m.readReady(i); rec.Step != readyStepTune {
		t.Fatalf("back to tune: %+v", rec)
	}
	tr.percent(40, 40)
	tr.sdcard()
	rec, _ = m.readReady(i)
	if rec.Step != readyStepSDCard || rec.Percent != readySDCardFrom || rec.Done {
		t.Fatalf("sdcard: %+v", rec)
	}
	tr.finish()
	tr.step("tune")
	tr.percent(1, 40)
	rec, _ = m.readReady(i)
	if !rec.Done || rec.Percent != 100 || rec.Step != readyStepSDCard {
		t.Fatalf("after finish: %+v", rec)
	}

	// the next round: the earlier "done" is gone before the new record is written, and reads as this round's activity
	tr2 := m.beginReady(i, q, testBootB, "")
	if rec, _ = m.readReady(i); rec.Done || rec.BootID != testBootB || rec.Step != readyStepBoot {
		t.Fatalf("new round: %+v", rec)
	}
	if s := m.bootSetupOf(i, q, true, time.Now(), nil, ""); s.Ready {
		t.Fatalf("an old done record overruled the new round: %+v", s)
	}
	tr2.step("tune")
	tr2.close()
	tr2.step("ime")
	tr2.finish()
	if rec, _ = m.readReady(i); rec.Done || rec.Step != "tune" || rec.BootID != testBootB {
		t.Fatalf("closed without finish: %+v", rec)
	}
	var nilTracker *readyTracker
	nilTracker.step("tune")
	nilTracker.percent(1, 2)
	nilTracker.sdcard()
	nilTracker.finish()
	nilTracker.close()
	var nilProg *progressor
	nilProg.needReady()
	nilProg.readyStep("tune")
	nilProg.readyPercent(1, 2)
}

// A record that cannot be written is warned about, nothing is tracked, and the marker's heartbeat stops: status does
// not wait for a setup it cannot see.
func TestBeginReadyWriteFailure(t *testing.T) {
	logs := &logx.Collector{}
	m := markerTestManager(t)
	m.Log = logx.New(logs.Sink())
	const i = 3
	q := win.Process{PID: 4242, Created: 1_000_500}
	m.writeSetupMarker(i, setupMarker{QemuPID: q.PID, QemuCreated: q.Created, ValidUntil: time.Now().Add(time.Hour).Unix(),
		AliveMS: readyClockMS()})
	// a folder with a file in it where the record goes: it can be neither deleted nor replaced
	if err := os.MkdirAll(m.readyPath(i), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.readyPath(i), "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if tr := m.beginReady(i, q, testBootA, ""); tr != nil {
		t.Fatal("a tracker for a record that cannot be written")
	}
	var warned bool
	for _, msg := range logs.Messages() {
		warned = warned || strings.Contains(msg, "记不下开机调优进度") && strings.Contains(msg, "安卓一开完就报在线")
	}
	if !warned {
		t.Errorf("no warning in %q", logs.Messages())
	}
	if mk, ok := m.readSetupMarker(i); !ok || mk.AliveMS != 0 {
		t.Errorf("marker after the failure: %+v %v", mk, ok)
	}
	if s := m.bootSetupOf(i, q, true, time.Now(), nil, ""); !s.Ready {
		t.Errorf("held back with no record: %+v", s)
	}
}

// The take-over's one real step: the host input method is installed or replaced (not when it is up to date), and only
// then does the record start.
func TestProgressorNeedReadyIsLazyAndOnce(t *testing.T) {
	var made int
	var prog *progressor
	prog = &progressor{lazyReady: func() *readyTracker { made++; return &readyTracker{m: markerTestManager(t), i: 3} }}
	prog.emit("ime", "m", 700)
	if prog.ready != nil || made != 0 {
		t.Fatal("tracking began before there was real work")
	}
	prog.needReady()
	prog.needReady()
	if prog.ready == nil || made != 1 || prog.lazyReady != nil {
		t.Fatalf("tracker after needReady: %v made %d", prog.ready, made)
	}
	if prog.ready.rec.Step != "ime" {
		t.Errorf("the step already emitted was lost: %q", prog.ready.rec.Step)
	}
}

// Concurrent queries do not take each other for a start: with no start running, any number of them at once never see
// the instance held back or busy.
func TestConcurrentInfoNeverSeesAStart(t *testing.T) {
	setCap(t, time.Hour)
	m, i := opsTestInstance(t)
	q := win.Process{PID: 4242, Created: 1_000_500}
	putRecord(t, m, i, q, nil) // not done: only a start running would hold the instance back
	savedScan, savedBooted := qemuScan, statusBooted
	t.Cleanup(func() { qemuScan, statusBooted = savedScan, savedBooted })
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{i: q} }
	statusBooted = func(*Manager, context.Context, int) (bool, bool) { return true, true }
	var wg sync.WaitGroup
	var bad atomic.Int32
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 25; k++ {
				in, err := m.Info(context.Background(), i)
				if err != nil || in.Status != StatusOnline || in.Busy != "" {
					bad.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if n := bad.Load(); n != 0 {
		t.Fatalf("%d of 200 concurrent queries saw a start that was not there", n)
	}
	// and a real one is seen by all of them
	unlock := lockStart(t, m, i)
	defer unlock()
	for g := 0; g < 4; g++ {
		if in, err := m.Info(context.Background(), i); err != nil || in.Status != StatusStarting || in.Busy != "starting" {
			t.Fatalf("with the start lock held: %+v %v", in, err)
		}
	}
}

// A creating or failed creation shows no setup fields, whatever the boots of that setup left.
func TestCreationClearsSetupFields(t *testing.T) {
	setCap(t, time.Hour)
	m, i := opsTestInstance(t)
	q := win.Process{PID: 4242, Created: 1_000_500}
	putRecord(t, m, i, q, nil)
	unlock := lockStart(t, m, i)
	defer unlock()
	if in := infoOf(t, m, i, &q); in.Status != StatusStarting || in.SetupStep != "tune" {
		t.Fatalf("before: %s %q", in.Status, in.SetupStep)
	}
	if err := m.setCreation(i, creationFailed, "boom", ""); err != nil {
		t.Fatal(err)
	}
	if in := infoOf(t, m, i, &q); in.Status != StatusCreateFailed || in.SetupStep != "" || in.SetupProgress != 0 || in.SetupUnfinished != "" {
		t.Errorf("failed creation: %s %q %d %q", in.Status, in.SetupStep, in.SetupProgress, in.SetupUnfinished)
	}
	if err := m.setCreation(i, creationCreating, "", ""); err != nil {
		t.Fatal(err)
	}
	cl, ok, err := win.TryLock(m.lockPath(i, "create"))
	if err != nil || !ok {
		t.Fatalf("create lock: %v %v", ok, err)
	}
	defer cl.Unlock()
	if in := infoOf(t, m, i, &q); in.Status != StatusCreating || in.SetupStep != "" || in.SetupProgress != 0 || in.SetupUnfinished != "" {
		t.Errorf("creating: %s %q %d %q", in.Status, in.SetupStep, in.SetupProgress, in.SetupUnfinished)
	}
}

// The four ways status must not stay "starting" for good, through Info (what status, list and the manager read), with a
// start lock held by this process standing for the start that works on the boot.
func TestInfoNeverStuckStarting(t *testing.T) {
	setCap(t, time.Minute)
	clock := fakeClock(t, 8_000_000)
	m, i := opsTestInstance(t)
	q := win.Process{PID: 4242, Created: 1_000_500}
	now := time.Now()
	check := func(label string, held bool, want Status, wantStep string) InstanceInfo {
		t.Helper()
		var unlock func()
		if held {
			unlock = lockStart(t, m, i)
		}
		in := infoOf(t, m, i, &q)
		if unlock != nil {
			unlock()
		}
		if in.Status != want || in.SetupStep != wantStep || !in.Booted || !in.Running {
			t.Errorf("%s: status %s step %q booted %v, want %s %q", label, in.Status, in.SetupStep, in.Booted, want, wantStep)
		}
		return in
	}

	// 1. not started by modavd: no record, no marker; also while an ordinary adopt runs on it
	check("foreign qemu", false, StatusOnline, "")
	check("foreign qemu, adopt running", true, StatusOnline, "")

	// 2. the start that was doing the setup is gone: marker and record are still there, the lock is not
	m.writeSetupMarker(i, setupMarker{QemuPID: q.PID, QemuCreated: q.Created, ValidUntil: now.Add(time.Hour).Unix(), AliveMS: clock.Load()})
	putRecord(t, m, i, q, func(r *readyRecord) { r.Step, r.Percent = "tune", 35 })
	in := check("setup being done", true, StatusStarting, "tune")
	if in.SetupProgress != 35 || in.Busy != "starting" || in.SetupUnfinished != "" {
		t.Errorf("progress %d busy %q unfinished %q", in.SetupProgress, in.Busy, in.SetupUnfinished)
	}
	in = check("the start was killed", false, StatusOnline, "")
	if in.Busy != "" {
		t.Errorf("busy %q with no start", in.Busy)
	}

	// 3. the cap
	clock.Add(bootSetupCap.Milliseconds())
	putRecord(t, m, i, q, func(r *readyRecord) { r.Step, r.Percent, r.DeadlineMS = "ime", 60, clock.Load()-1 })
	in = check("past the cap", true, StatusOnline, "")
	if in.SetupUnfinished != "ime" || in.Busy != "starting" {
		t.Errorf("unfinished %q busy %q", in.SetupUnfinished, in.Busy)
	}

	// 4. the guest restarted by itself: the boot watch's start sets the new boot up (record of that boot, lock held),
	// then it is done; and a retune whose start died is not waited for either
	m.writeBootRecord(i, bootRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: testBootA})
	putRecord(t, m, i, q, func(r *readyRecord) { r.Step, r.Percent, r.Done = "sdcard", 100, true })
	check("before the guest restarted", true, StatusOnline, "")
	putRecord(t, m, i, q, func(r *readyRecord) { r.Step, r.Percent = readyStepBoot, 0 })
	check("guest restarted, retune running", true, StatusStarting, readyStepBoot)
	putRecord(t, m, i, q, func(r *readyRecord) { r.Step, r.Percent = "first_boot", 50 })
	check("guest restarted, retune further", true, StatusStarting, "first_boot")
	check("guest restarted, retune start died", false, StatusOnline, "")
	putRecord(t, m, i, q, func(r *readyRecord) { r.Step, r.Percent, r.Done = "sdcard", 100, true })
	check("retune done", true, StatusOnline, "")

	// Android not booted is starting whatever the record says, and a qemu that is gone is offline
	savedScan, savedBooted := qemuScan, statusBooted
	t.Cleanup(func() { qemuScan, statusBooted = savedScan, savedBooted })
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{i: q} }
	statusBooted = func(*Manager, context.Context, int) (bool, bool) { return false, true }
	if info, err := m.Info(context.Background(), i); err != nil || info.Status != StatusStarting || info.Booted || info.SetupStep != "" {
		t.Errorf("not booted: %+v %v", info, err)
	}
	if off := infoOf(t, m, i, nil); off.Status != StatusOffline || off.SetupStep != "" {
		t.Errorf("no qemu: %+v", off)
	}
}

// The guest restarting again during the setup: the new boot does not inherit the step and the deadline of the one
// before. The guest is asked for its boot_id only while a record that is not done would hold the instance.
func TestInfoGuestRestartedDuringSetup(t *testing.T) {
	setCap(t, time.Minute)
	fakeClock(t, 3_000_000)
	m, i := opsTestInstance(t)
	q := win.Process{PID: 4242, Created: 1_000_500}
	var reads atomic.Int32
	id := testBootA
	saved := guestBootRead
	t.Cleanup(func() { guestBootRead = saved })
	guestBootRead = func(int, bool) (bool, string, bool) { reads.Add(1); return true, id, false }

	putRecord(t, m, i, q, func(r *readyRecord) { r.BootID = testBootA })
	unlock := lockStart(t, m, i)
	defer unlock()
	if in := infoOf(t, m, i, &q); in.Status != StatusStarting || in.SetupStep != "tune" || reads.Load() == 0 {
		t.Fatalf("same boot: %s %q (boot_id read %d times)", in.Status, in.SetupStep, reads.Load())
	}
	id = testBootB
	if in := infoOf(t, m, i, &q); in.Status != StatusOnline || in.SetupStep != "" || in.SetupProgress != 0 || in.SetupUnfinished != "" {
		t.Errorf("the guest restarted: %s %q %d %q", in.Status, in.SetupStep, in.SetupProgress, in.SetupUnfinished)
	}
	// not asked when there is nothing to decide: done, or no record
	putRecord(t, m, i, q, func(r *readyRecord) { r.BootID, r.Done = testBootA, true })
	reads.Store(0)
	if in := infoOf(t, m, i, &q); in.Status != StatusOnline || reads.Load() != 0 {
		t.Errorf("done record: %s, boot_id read %d times", in.Status, reads.Load())
	}
	m.removeReady(i)
	if in := infoOf(t, m, i, &q); in.Status != StatusOnline || reads.Load() != 0 {
		t.Errorf("no record: %s, boot_id read %d times", in.Status, reads.Load())
	}
}

// The start of an instance on a root with the fake adb: its status while the setup runs, what is on disk, and what the
// cases of "stuck at starting" look like through the real Start.
type startRun struct {
	t    *testing.T
	m    *Manager
	i    int
	q    win.Process
	logs *logx.Collector
}

func newStartRun(t *testing.T) *startRun {
	t.Helper()
	r, _ := portPlanRoot(t)
	logs := &logx.Collector{}
	m := New(r, logx.New(logs.Sink()))
	const i = 6
	six := i
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &six}); err != nil {
		t.Fatal(err)
	}
	if err := r.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set("initialized", jsonord.NewBool(true))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	q := fakeQemu(t, filepath.Join(r.Dir, "fakeq"), i, 21060, false, "")
	run := &startRun{t: t, m: m, i: i, q: q, logs: logs}

	savedFind, savedWaitFind, savedScan, savedGuest, savedArgv :=
		startFindQemu, waitBootFindQemu, qemuScan, guestBootRead, bootWatchArgv
	t.Cleanup(func() {
		startFindQemu, waitBootFindQemu, qemuScan, guestBootRead, bootWatchArgv =
			savedFind, savedWaitFind, savedScan, savedGuest, savedArgv
	})
	startFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
	waitBootFindQemu = func(*Manager, int) (host.Process, bool, error) { return q, true, nil }
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{i: q} }
	bootWatchArgv = func(string, int, host.Process) ([]string, error) {
		return nil, errors.New("no boot watch in this test")
	}
	run.guestBoot(testBootA)
	return run
}

func (s *startRun) guestBoot(id string) {
	guestBootRead = func(int, bool) (bool, string, bool) { return true, id, false }
}

func (s *startRun) owedMarker() {
	s.m.writeSetupMarker(s.i, setupMarker{QemuPID: s.q.PID, QemuCreated: s.q.Created, ValidUntil: time.Now().Add(time.Hour).Unix(),
		AliveMS: readyClockMS()})
}

// start runs Start; at each progress step (vulkan_fix, tune, done) at, it records the status the manager would show.
func (s *startRun) start(ctx context.Context, at func(step string, in InstanceInfo)) (*StartResult, error) {
	s.t.Helper()
	return s.m.Start(ctx, s.i, StartOptions{NoBridge: true, Timeout: time.Minute, Progress: func(p Progress) {
		switch p.Step {
		case "vulkan_fix", "tune", "done":
			if at != nil {
				in, err := s.m.Info(context.Background(), s.i)
				if err != nil {
					s.t.Errorf("Info at %s: %v", p.Step, err)
					return
				}
				at(p.Step, *in)
			}
		}
	}})
}

// A start that sets a boot up (the marker of an interrupted start is there: an owed boot) keeps status at starting
// with the step and percentage until the setup is done, and the boot record that makes the boot watch register the
// instance with the default adb server (5037) is only written by then. The record of the boot before, "done", does not
// count as this round's.
func TestStartOwedBootIsStartingUntilSetupDone(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.owedMarker()
	putRecord(t, s.m, s.i, s.q, func(r *readyRecord) { r.Done, r.Step, r.Percent = true, "sdcard", 100 })
	seen := map[string]InstanceInfo{}
	var recordAtTune bool
	res, err := s.start(context.Background(), func(step string, in InstanceInfo) {
		seen[step] = in
		if step == "tune" {
			_, recordAtTune = s.m.readBootRecord(s.i)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"vulkan_fix", "tune"} {
		in, ok := seen[step]
		if !ok || in.Status != StatusStarting || in.Busy != "starting" || in.SetupStep != step || !in.Booted {
			t.Errorf("at %s: %+v", step, in)
		}
	}
	if seen["tune"].SetupProgress <= 0 || seen["tune"].SetupProgress > readyAfterBootMax {
		t.Errorf("progress at tune: %d", seen["tune"].SetupProgress)
	}
	if recordAtTune {
		t.Error("the boot record (default adb server registration) exists before the setup is done")
	}
	if in := seen["done"]; in.Status != StatusOnline || in.SetupStep != "" || in.SetupProgress != 0 || in.SetupUnfinished != "" {
		t.Errorf("at done (start lock still held): %+v", in)
	}
	if res.Instance.Status != StatusOnline || res.Instance.Busy != "" {
		t.Errorf("result: %s busy %q", res.Instance.Status, res.Instance.Busy)
	}
	if rec, ok := s.m.readReady(s.i); !ok || !rec.Done || rec.Percent != 100 || !rec.names(s.q) || rec.BootID != testBootA {
		t.Errorf("record after start: %+v %v", rec, ok)
	}
	if _, ok := s.m.readBootRecord(s.i); !ok {
		t.Error("no boot record after the start")
	}
	if in := infoOf(t, s.m, s.i, &s.q); in.Status != StatusOnline || in.Busy != "" {
		t.Errorf("after the start: %s busy %q", in.Status, in.Busy)
	}
}

// An ordinary adopt of a boot that is up and was set up (no marker, record of the same boot, initialized) only checks:
// nothing is held back, no record is written; likewise for a qemu modavd never set up.
func TestStartPlainAdoptStaysOnline(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.m.writeBootRecord(s.i, bootRecord{QemuPID: s.q.PID, QemuCreated: s.q.Created, BootID: testBootA})
	seen := map[string]InstanceInfo{}
	if _, err := s.start(context.Background(), func(step string, in InstanceInfo) { seen[step] = in }); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("no progress step seen")
	}
	for step, in := range seen {
		if in.Status != StatusOnline || in.SetupStep != "" {
			t.Errorf("at %s: %s step %q", step, in.Status, in.SetupStep)
		}
	}
	if _, ok := s.m.readReady(s.i); ok {
		t.Error("an adopt that owes nothing wrote a setup record")
	}
}

// A take-over that has real work to do is tracked: the instance is not initialized (the first-boot setup runs).
func TestStartAdoptOfUninitializedInstanceIsTracked(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	if err := s.m.Root.EditInstanceMeta(s.i, func(meta *jsonord.Obj) error {
		meta.Set("initialized", jsonord.NewBool(false))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	seen := map[string]InstanceInfo{}
	if _, err := s.m.Start(context.Background(), s.i, StartOptions{NoBridge: true, Timeout: time.Minute, Progress: func(p Progress) {
		if p.Step == "first_boot" || p.Step == "done" {
			in, err := s.m.Info(context.Background(), s.i)
			if err == nil {
				seen[p.Step] = *in
			}
		}
	}}); err != nil {
		t.Fatal(err)
	}
	if in, ok := seen["first_boot"]; !ok || in.Status != StatusStarting || in.SetupStep != "first_boot" {
		t.Errorf("at first_boot: %+v", seen["first_boot"])
	}
	if in := seen["done"]; in.Status != StatusOnline {
		t.Errorf("at done: %s", in.Status)
	}
}

// The guest restarted by itself under the same qemu: the start (the boot watch's, or any) finds another boot_id in the
// record, sets the new boot up as an owed one: starting with the step, then online, and the record names the new boot.
func TestStartAfterGuestRebootIsStartingToo(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.m.writeBootRecord(s.i, bootRecord{QemuPID: s.q.PID, QemuCreated: s.q.Created, BootID: testBootA})
	s.guestBoot(testBootB)
	seen := map[string]InstanceInfo{}
	if _, err := s.start(context.Background(), func(step string, in InstanceInfo) { seen[step] = in }); err != nil {
		t.Fatal(err)
	}
	if in := seen["tune"]; in.Status != StatusStarting || in.SetupStep != "tune" {
		t.Errorf("at tune: %s step %q", in.Status, in.SetupStep)
	}
	if in := seen["done"]; in.Status != StatusOnline {
		t.Errorf("at done: %s", in.Status)
	}
	if rec, _ := s.m.readReady(s.i); !rec.Done || rec.BootID != testBootB {
		t.Errorf("record: %+v", rec)
	}
	if rec, _ := s.m.readBootRecord(s.i); rec.BootID != testBootB {
		t.Errorf("boot record: %+v", rec)
	}
}

// A setup that runs past the cap: status is online with the step still running named, the start goes on to the end,
// and its log names the step.
func TestStartPastTheCapReportsOnline(t *testing.T) {
	s := newStartRun(t)
	const capFor = 2 * time.Second
	setCap(t, capFor)
	s.owedMarker()
	seen := map[string]InstanceInfo{}
	if _, err := s.start(context.Background(), func(step string, in InstanceInfo) {
		seen[step] = in
		if step == "tune" {
			// the setup takes longer than the cap: wait until it has passed (counted from the record's deadline)
			rec, _ := s.m.readReady(s.i)
			time.Sleep(time.Duration(rec.DeadlineMS-readyClockMS())*time.Millisecond + 200*time.Millisecond)
			late, err := s.m.Info(context.Background(), s.i)
			if err != nil {
				t.Error(err)
				return
			}
			seen["tune+"] = *late
		}
	}); err != nil {
		t.Fatal(err)
	}
	if in := seen["vulkan_fix"]; in.Status != StatusStarting {
		t.Errorf("within the cap: %s", in.Status)
	}
	if in := seen["tune+"]; in.Status != StatusOnline || in.SetupUnfinished != "tune" || in.SetupStep != "" || in.Busy != "starting" {
		t.Errorf("past the cap: %s unfinished %q step %q busy %q", in.Status, in.SetupUnfinished, in.SetupStep, in.Busy)
	}
	var warned bool
	for _, msg := range s.logs.Messages() {
		warned = warned || strings.Contains(msg, "开机后调优超过") && strings.Contains(msg, "开机后调优") && strings.Contains(msg, "状态已经先报在线")
	}
	if !warned {
		t.Errorf("no cap warning in %q", s.logs.Messages())
	}
}

// A record that cannot be written: the start warns, goes on, and status never waits for it.
func TestStartWithoutARecordIsNotWaitedFor(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.owedMarker()
	if err := os.MkdirAll(s.m.readyPath(s.i), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.m.readyPath(s.i), "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	seen := map[string]InstanceInfo{}
	if _, err := s.start(context.Background(), func(step string, in InstanceInfo) { seen[step] = in }); err != nil {
		t.Fatal(err)
	}
	for step, in := range seen {
		if in.Status != StatusOnline || in.SetupStep != "" {
			t.Errorf("at %s: %s step %q", step, in.Status, in.SetupStep)
		}
	}
	var warned bool
	for _, msg := range s.logs.Messages() {
		warned = warned || strings.Contains(msg, "记不下开机调优进度")
	}
	if !warned {
		t.Errorf("no warning in %q", s.logs.Messages())
	}
}

// A start that is cancelled in the middle of the setup leaves a record that is not done and releases the lock: status
// does not wait for a setup nobody does, and the next start of that boot sets it up again.
func TestStartCancelledMidSetupDoesNotStickAtStarting(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.owedMarker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var during InstanceInfo
	if _, err := s.start(ctx, func(step string, in InstanceInfo) {
		if step == "tune" {
			during = in
			cancel()
		}
	}); err == nil {
		t.Fatal("the cancelled start returned no error")
	}
	if during.Status != StatusStarting {
		t.Errorf("during: %s", during.Status)
	}
	if rec, ok := s.m.readReady(s.i); !ok || rec.Done {
		t.Errorf("record after the cancel: %+v %v", rec, ok)
	}
	if in := infoOf(t, s.m, s.i, &s.q); in.Status != StatusOnline || in.Busy != "" || in.SetupStep != "" {
		t.Errorf("after the cancel: %s busy %q step %q", in.Status, in.Busy, in.SetupStep)
	}
	// the next start finishes it
	if _, err := s.start(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if rec, _ := s.m.readReady(s.i); !rec.Done {
		t.Errorf("record after the second start: %+v", rec)
	}
}

// Cancelled during the wait for /sdcard (the last part of the setup): that is no completed setup: no "done" record, no
// boot record (so the default adb server is not told), and Start returns the cancellation.
func TestStartCancelledDuringSdcardWaitIsNotDone(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.owedMarker()
	t.Setenv(fakeAdbSdcardFailEnv, "1") // /sdcard never accepts a file: the wait runs until cancelled
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var cancelAt atomic.Int64
	go func() {
		for ctx.Err() == nil {
			if rec, ok := s.m.readReady(s.i); ok && rec.Step == readyStepSDCard {
				time.AfterFunc(700*time.Millisecond, cancel)
				cancelAt.Store(time.Now().UnixMilli())
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	_, err := s.start(ctx, nil)
	if errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("Start returned %v (%s), want a cancellation", err, errs.CodeOf(err))
	}
	if cancelAt.Load() == 0 {
		t.Fatal("never reached the /sdcard wait")
	}
	if rec, ok := s.m.readReady(s.i); !ok || rec.Done || rec.Step != readyStepSDCard {
		t.Errorf("record: %+v %v", rec, ok)
	}
	if _, ok := s.m.readBootRecord(s.i); ok {
		t.Error("a boot record was written for a setup that was cancelled")
	}
	if in := infoOf(t, s.m, s.i, &s.q); in.Status != StatusOnline || in.Busy != "" {
		t.Errorf("after: %s busy %q", in.Status, in.Busy)
	}
}
