//go:build windows

package emu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestBootSetupOf: what the readers (status, list, info) make of the record, the start lock and the setup marker.
func TestBootSetupOf(t *testing.T) {
	m := markerTestManager(t)
	const i = 3
	q := win.Process{PID: 4242, PPID: 3000, Created: 1_000_500}
	other := win.Process{PID: 4243, Created: 1_000_900}
	now := time.Now()
	rec := func(p win.Process, started time.Time, step string, pct int, done bool) {
		t.Helper()
		b := &readyTracker{m: m, i: i, rec: readyRecord{QemuPID: p.PID, QemuCreated: p.Created, StartedAtMS: started.UnixMilli(),
			Step: step, Percent: pct, Done: done}}
		b.mu.Lock()
		b.write()
		b.mu.Unlock()
	}
	marker := func(until time.Time) {
		t.Helper()
		m.writeSetupMarker(i, setupMarker{QemuPID: q.PID, QemuCreated: q.Created, ValidUntil: until.Unix()})
	}
	reset := func() {
		m.removeReady(i)
		m.removeSetupMarker(i)
	}

	for _, c := range []struct {
		name  string
		setup func()
		held  bool
		want  bootSetupState
	}{
		{"qemu no modavd start works on: nothing to wait for", func() {}, false, bootSetupState{Ready: true}},
		{"adopt of a qemu from another tool: no marker, no record", func() {}, true, bootSetupState{Ready: true}},
		{"launched, record not written yet: marker and the start running", func() { marker(now.Add(time.Hour)) }, true,
			bootSetupState{Step: readyStepBoot}},
		{"launched, but the start is gone: nobody finishes it", func() { marker(now.Add(time.Hour)) }, false,
			bootSetupState{Ready: true}},
		{"marker past its deadline", func() { marker(now.Add(-time.Minute)) }, true, bootSetupState{Ready: true}},
		{"setup running", func() { rec(q, now.Add(-5*time.Second), "tune", 40, false) }, true,
			bootSetupState{Step: "tune", Progress: 40}},
		{"setup running, the start was cancelled or killed", func() { rec(q, now.Add(-5*time.Second), "tune", 40, false) }, false,
			bootSetupState{Ready: true}},
		{"setup running past the cap", func() { rec(q, now.Add(-bootSetupCap-time.Second), "ime", 60, false) }, true,
			bootSetupState{Ready: true, Unfinished: "ime"}},
		{"setup done", func() { rec(q, now.Add(-5*time.Second), "sdcard", 100, true) }, true, bootSetupState{Ready: true}},
		{"record of another qemu", func() { rec(other, now, "tune", 40, false) }, true, bootSetupState{Ready: true}},
		{"record of another qemu, marker of this one", func() {
			rec(other, now, "tune", 40, false)
			marker(now.Add(time.Hour))
		}, true, bootSetupState{Step: readyStepBoot}},
		{"record of this qemu's earlier PID reuse", func() {
			rec(win.Process{PID: q.PID, Created: q.Created + 1}, now, "tune", 40, false)
		}, true, bootSetupState{Ready: true}},
	} {
		reset()
		c.setup()
		if got := m.bootSetupOf(i, q, c.held, now); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	reset()
	if err := os.WriteFile(m.readyPath(i), []byte(`{"qemu_pid":4242,"qemu_cre`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.bootSetupOf(i, q, true, now); !got.Ready {
		t.Errorf("a torn record held the instance back: %+v", got)
	}
}

// TestReadyTrackerFile: the record names the qemu, only moves forward, ends Done, and a tracker that is closed without
// finishing leaves the setup not done.
func TestReadyTrackerFile(t *testing.T) {
	setCap(t, time.Hour)
	m := markerTestManager(t)
	const i = 3
	q := win.Process{PID: 4242, Created: 1_000_500}
	tr := m.beginReady(i, q, testBootA)
	rec, ok := m.readReady(i)
	if !ok || !rec.names(q) || rec.BootID != testBootA || rec.Step != readyStepBoot || rec.Percent != 0 || rec.Done ||
		time.Since(time.UnixMilli(rec.StartedAtMS)) > time.Minute {
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

	tr2 := m.beginReady(i, q, testBootB)
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
}

// TestReadyCapLogsTheStep: a setup that runs on past the cap says which step it was at; one that is done, or whose
// start returned, says nothing.
func TestReadyCapLogsTheStep(t *testing.T) {
	setCap(t, 80*time.Millisecond)
	logs := &logx.Collector{}
	m := markerTestManager(t)
	m.Log = logx.New(logs.Sink())
	q := win.Process{PID: 4242, Created: 1_000_500}

	slow := m.beginReady(3, q, "")
	slow.step("ime")
	deadline := time.Now().Add(5 * time.Second)
	for len(logs.Messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if msgs := logs.Messages(); len(msgs) != 1 || !strings.Contains(msgs[0], "实例 3 开机后调优超过") || !strings.Contains(msgs[0], "宿主输入法") {
		t.Fatalf("after the cap: %q", msgs)
	}
	slow.close()

	n := len(logs.Messages())
	done := m.beginReady(3, q, "")
	done.finish()
	closed := m.beginReady(3, q, "")
	closed.close()
	time.Sleep(300 * time.Millisecond)
	if got := len(logs.Messages()); got != n {
		t.Fatalf("finished or closed setups logged: %q", logs.Messages()[n:])
	}
}

// The four ways status must not stay "starting" for good, through Info (what status, list and the manager read), with a
// start lock held by this process standing for the start that works on the boot.
func TestInfoNeverStuckStarting(t *testing.T) {
	m, i := opsTestInstance(t)
	q := win.Process{PID: 4242, Created: 1_000_500}
	now := time.Now()
	writeRec := func(started time.Time, step string, pct int, done bool) {
		tr := &readyTracker{m: m, i: i, rec: readyRecord{QemuPID: q.PID, QemuCreated: q.Created,
			StartedAtMS: started.UnixMilli(), Step: step, Percent: pct, Done: done}}
		tr.mu.Lock()
		tr.write()
		tr.mu.Unlock()
	}
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
	m.writeSetupMarker(i, setupMarker{QemuPID: q.PID, QemuCreated: q.Created, ValidUntil: now.Add(time.Hour).Unix()})
	writeRec(now.Add(-2*time.Second), "tune", 35, false)
	in := check("setup being done", true, StatusStarting, "tune")
	if in.SetupProgress != 35 || in.Busy != "starting" || in.SetupUnfinished != "" {
		t.Errorf("progress %d busy %q unfinished %q", in.SetupProgress, in.Busy, in.SetupUnfinished)
	}
	in = check("the start was killed", false, StatusOnline, "")
	if in.Busy != "" {
		t.Errorf("busy %q with no start", in.Busy)
	}

	// 3. the cap
	writeRec(now.Add(-bootSetupCap-time.Second), "ime", 60, false)
	in = check("past the cap", true, StatusOnline, "")
	if in.SetupUnfinished != "ime" || in.Busy != "starting" {
		t.Errorf("unfinished %q busy %q", in.SetupUnfinished, in.Busy)
	}

	// 4. the guest restarted by itself: the boot watch's start sets the new boot up (record of that boot, lock held),
	// then it is done; and a retune whose start died is not waited for either
	m.writeBootRecord(i, bootRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: testBootA})
	writeRec(now.Add(-time.Minute), "sdcard", 100, true)
	check("before the guest restarted", true, StatusOnline, "")
	writeRec(time.Now(), readyStepBoot, 0, false)
	check("guest restarted, retune running", true, StatusStarting, readyStepBoot)
	writeRec(time.Now(), "first_boot", 50, false)
	check("guest restarted, retune further", true, StatusStarting, "first_boot")
	check("guest restarted, retune start died", false, StatusOnline, "")
	writeRec(time.Now(), "sdcard", 100, true)
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

// The start of an instance on a root with the fake adb: its status while the setup runs, what is on disk, and what the
// four cases of "stuck at starting" look like through the real Start.
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
// instance with the default adb server (5037) is only written by then.
func TestStartOwedBootIsStartingUntilSetupDone(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.m.writeSetupMarker(s.i, setupMarker{QemuPID: s.q.PID, QemuCreated: s.q.Created, ValidUntil: time.Now().Add(time.Hour).Unix()})
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

// An ordinary adopt of a boot that is up and was set up (no marker, record of the same boot) only checks: nothing is
// held back, no record is written; likewise for a qemu modavd never set up.
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
	s.m.writeSetupMarker(s.i, setupMarker{QemuPID: s.q.PID, QemuCreated: s.q.Created, ValidUntil: time.Now().Add(time.Hour).Unix()})
	seen := map[string]InstanceInfo{}
	if _, err := s.start(context.Background(), func(step string, in InstanceInfo) {
		seen[step] = in
		if step == "tune" {
			// the setup takes longer than the cap: wait until it has passed (counted from the record's start)
			rec, _ := s.m.readReady(s.i)
			time.Sleep(time.Until(time.UnixMilli(rec.StartedAtMS).Add(capFor + 200*time.Millisecond)))
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

// A start that is cancelled in the middle of the setup leaves a record that is not done and releases the lock: status
// does not wait for a setup nobody does, and the next start of that boot sets it up again.
func TestStartCancelledMidSetupDoesNotStickAtStarting(t *testing.T) {
	s := newStartRun(t)
	setCap(t, time.Hour)
	s.m.writeSetupMarker(s.i, setupMarker{QemuPID: s.q.PID, QemuCreated: s.q.Created, ValidUntil: time.Now().Add(time.Hour).Unix()})
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
