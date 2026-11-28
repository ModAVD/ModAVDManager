package emu

// When an instance is "online". Android having booted (sys.boot_completed) is not enough: the after-boot setup of
// that boot (afterBoot: Vulkan fix, host scheduling, host input method, first-boot setup, writing to /sdcard ...) goes
// on for seconds after it, and a tool that starts working then meets a guest that is not set up. So status / list /
// info report starting (the only value they have for it; Busy says starting too) until the setup of this boot is done,
// or the cap passed: bootSetupCap after the start began it.
//
// The start that does the setup (start.go) keeps a small record, <root>\avd\.mas_<i>.ready, that any other process
// reads: the qemu process it is for (PID + creation time, as the setup marker and the boot record do, so a record of
// an earlier boot never names the qemu of this one), the process that wrote it, the guest's boot_id, the deadline,
// the step it is at and how far it is. It is not the boot record: that one means "set up", and the boot watch
// registers the instance with the default adb server (5037) only once it exists, i.e. after the setup; this one
// exists while the setup runs. A start deletes the record of an earlier boot before it writes its own, so an old
// "done" never reads as this round's.
//
// Time is the machine's millisecond counter (host.MonotonicMS), not the wall clock: the deadline is fixed once when
// the setup begins (now + bootSetupCap) and the readers and the cap's log line use that one number, so a clock set back
// or forward (the time service, a user) cannot lengthen the wait, release it early or turn an "online" back into
// "starting". On Windows the counter keeps running while the computer sleeps: a setup that was still going when it
// went to sleep is released on waking up, which is better than holding the instance back. Elsewhere it is the wall
// clock, with those effects.
//
// Nothing is waited for that nobody does:
//   - a qemu that no modavd start works on (started by another tool, or by a start that was cancelled or killed: the
//     start lock is released with the process) is online as soon as Android has booted, as before;
//   - the record names the process that wrote it, and a record whose writer is gone is not waited for either;
//   - a start that works on it for longer than bootSetupCap stops holding it back (the start goes on, and logs which
//     step it was at);
//   - a record of another qemu is no record; so is one of another guest boot (the guest restarted in the middle of
//     the setup), which is asked of the guest only while a record that is not done would otherwise hold the instance;
//   - between Android's boot and the start's record (a few seconds) the setup marker of the launch holds it, with the
//     marker's heartbeat (setupMarker.AliveMS, written during the boot wait) bounding that to bootSetupCap too;
//   - a record that cannot be written is not waited for at all.
//
// A guest that reboots by itself under the same qemu reads like a new boot: the boot watch (bootwatch.go) runs Start
// again, which finds the boot_id changed, treats the boot as owed (setupOwed) and writes a new record. The seconds
// between the reboot and the watch's next look still read online, since nobody has started setting the new boot up.
// A plain adopt of a boot that is up and set up (setupAdopted) writes no record, since it only checks and a user may
// be in the guest; it starts one only when it is about to do real work: the first-boot setup (the instance is not
// initialized), or installing the host input method.

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"modavd/internal/host"
	"modavd/internal/root"
)

// bootSetupCap is how long after a start began the setup of a boot status goes on reporting starting for it, at most.
// A variable for tests.
var bootSetupCap = 60 * time.Second

// readyClockMS is the counter the deadlines are on (a variable so tests move it).
var readyClockMS = host.MonotonicMS

// Steps of the setup (InstanceInfo.SetupStep): the progress steps of start.go (progressor.emit) plus "boot" for what
// comes before the first of them and "sdcard" for the wait for /sdcard after them. The percentage of afterBoot's
// steps counts what is done, up to readyAfterBootMax; the wait for /sdcard is the rest. After the steps with a name
// of their own the step goes back to "tune", the general setup.
const (
	readyStepBoot   = "boot"
	readyStepTune   = "tune"
	readyStepSDCard = "sdcard"

	readyAfterBootMax = 90
	readySDCardFrom   = 95

	// readyHeartbeatEvery: how often the boot wait refreshes the marker's AliveMS.
	readyHeartbeatEvery = 5 * time.Second
)

// readyRecord is the content of readyPath.
type readyRecord struct {
	QemuPID     uint32 `json:"qemu_pid"`
	QemuCreated uint64 `json:"qemu_created"`
	// OwnerPID and OwnerCreated: the process whose start wrote the record (0: unknown).
	OwnerPID     uint32 `json:"owner_pid,omitempty"`
	OwnerCreated uint64 `json:"owner_created,omitempty"`
	BootID       string `json:"boot_id,omitempty"`
	// Round: the start attempt that wrote the record (overlay.go, "The round"); "" in records of an older modavd.
	Round string `json:"round,omitempty"`
	// DeadlineMS (host.MonotonicMS): when the wait for this setup ends, fixed when it began.
	DeadlineMS int64  `json:"deadline_ms"`
	Step       string `json:"step"`
	Percent    int    `json:"percent"`
	Done       bool   `json:"done"`
}

// names: q is the qemu process this record was written for.
func (r readyRecord) names(q host.Process) bool {
	return r.QemuPID != 0 && r.QemuCreated != 0 && r.QemuPID == q.PID && r.QemuCreated == q.Created
}

// ownerGone: the process that wrote the record has exited.
func (r readyRecord) ownerGone() bool {
	return r.OwnerPID != 0 && r.OwnerCreated != 0 && !(host.Process{PID: r.OwnerPID, Created: r.OwnerCreated}).Alive()
}

// expired: now (readyClockMS) is at or past the deadline, or so far before it that the counter cannot be the one the
// deadline was set on (the computer restarted, or the record is damaged): the wait is over.
func (r readyRecord) expired(now int64) bool {
	left := r.DeadlineMS - now
	return left <= 0 || left > bootSetupCap.Milliseconds()
}

// readyPath is <root>\avd\.mas_<i>.ready.
func (m *Manager) readyPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".ready")
}

// readReady: ok=false when there is none or it cannot be parsed (a torn write counts as none).
func (m *Manager) readReady(i int) (readyRecord, bool) {
	var r readyRecord
	b, err := os.ReadFile(m.readyPath(i))
	if err != nil || json.Unmarshal(b, &r) != nil {
		return readyRecord{}, false
	}
	return r, r.QemuPID != 0
}

// removeReady deletes the record of instance i (after Delete, and before a launch: the boot it named is over); best
// effort.
func (m *Manager) removeReady(i int) {
	if err := os.Remove(m.readyPath(i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.Log.Debugf("实例 %d 删不掉开机调优记录: %v", i, err)
	}
}

// bootSetupState is what the readers make of the record: ready (online may be reported), and while not, the step and
// percentage; Unfinished is the step still running when ready only because the cap passed.
type bootSetupState struct {
	Ready      bool
	Step       string
	Progress   int
	Unfinished string
}

// bootSetupOf is the state of the setup of the boot of qemu q of instance i, at now. startHeld: some modavd process
// holds the instance's start lock right now (a start is running). guestBootID reads the guest's boot_id; it is called
// only when a record that is not done would hold the instance, and may be nil (the overlay passes a cached read: it
// must not wait for adb). Android has booted already when this is asked.
//
// round: only a record of that round counts ("" for status: the round of the start that holds the lock, as its setup
// marker names it, when there is one; any record otherwise). A record of another round, which is what an earlier boot
// of the same qemu left, is no record: it never reads as this round's done.
func (m *Manager) bootSetupOf(i int, q host.Process, startHeld bool, now time.Time, guestBootID func() string, round string) bootSetupState {
	clock := readyClockMS()
	var mk setupMarker
	var mkOK, mkUse bool
	if startHeld {
		mk, mkOK = m.readSetupMarker(i)
		mkUse, _ = setupMarkerUse(mk, mkOK, q, now)
	}
	want := round
	if want == "" && mkUse {
		want = mk.Round
	}
	if rec, ok := m.readReady(i); ok && rec.names(q) && (want == "" || rec.Round == want) {
		if rec.Done || !startHeld || rec.ownerGone() {
			// done, or the start that was doing it is gone (cancelled, killed, failed): nobody will finish it
			return bootSetupState{Ready: true}
		}
		if rec.expired(clock) {
			return bootSetupState{Ready: true, Unfinished: rec.Step}
		}
		same := true
		if rec.BootID != "" && guestBootID != nil {
			if id := guestBootID(); id != "" && id != rec.BootID {
				same = false // the guest restarted since: the record is of the boot before
			}
		}
		if same {
			return bootSetupState{Step: rec.Step, Progress: rec.Percent}
		}
	}
	// No record of this boot yet. A start that launched it (or took over an unfinished boot) left the setup marker,
	// refreshes its AliveMS during the boot wait and writes the record within seconds of Android's boot; any other
	// qemu has no setup to wait for, and a marker nobody refreshed for bootSetupCap is not waited for either.
	if startHeld && mkUse && mk.AliveMS != 0 && (round == "" || mk.Round == round) {
		if age := clock - mk.AliveMS; age >= 0 && age < bootSetupCap.Milliseconds() {
			return bootSetupState{Step: readyStepBoot}
		}
	}
	return bootSetupState{Ready: true}
}

// readyTracker is the writer of the record, owned by the start that does a boot's setup. A nil tracker does nothing.
type readyTracker struct {
	m *Manager
	i int

	mu       sync.Mutex
	rec      readyRecord
	deadline time.Time // for the timer only; the record's DeadlineMS is the number that counts
	timer    *time.Timer
	closed   bool
}

// beginReady deletes the record of the earlier boot, writes the record of the setup that begins now for qemu q
// (bootID: the guest's boot_id, "" if unknown; round: this start attempt's) and arms the cap's log line. nil when the
// record cannot be written: that is warned about, the setup is then not tracked and status does not wait for it (the
// setup marker's heartbeat is stopped too, which would otherwise hold it for a while).
func (m *Manager) beginReady(i int, q host.Process, bootID, round string) *readyTracker {
	m.removeReady(i)
	t := &readyTracker{m: m, i: i, rec: readyRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: bootID, Round: round,
		DeadlineMS: readyClockMS() + bootSetupCap.Milliseconds(), Step: readyStepBoot}}
	if me, ok := host.FindProcess(uint32(os.Getpid())); ok {
		t.rec.OwnerPID, t.rec.OwnerCreated = me.PID, me.Created
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.write(); err != nil {
		m.Log.Warnf("实例 %d 记不下开机调优进度（%v）：状态不再等调优，安卓一开完就报在线", i, err)
		m.removeReady(i)
		m.releaseSetupMarker(i)
		return nil
	}
	t.arm()
	return t
}

// readyWrite stores a record (a variable: the tests make it fail).
var readyWrite = func(path string, data []byte, tag string) error { return writeFileReplace(path, data, tag) }

// write stores the record (t.mu held).
func (t *readyTracker) write() error {
	b, err := json.Marshal(t.rec)
	if err == nil {
		err = readyWrite(t.m.readyPath(t.i), b, strconv.Itoa(os.Getpid()))
	}
	if err != nil {
		t.m.Log.Debugf("实例 %d 记不下开机调优进度: %v", t.i, err)
	}
	return err
}

// arm sets the timer for the record's deadline (t.mu held); capPassed looks at the counter again when it fires.
func (t *readyTracker) arm() {
	left := time.Duration(t.rec.DeadlineMS-readyClockMS()) * time.Millisecond
	t.timer = time.AfterFunc(max(left, 0), t.capPassed)
}

// capPassed is the timer: the setup is still going on when status stopped waiting for it. It goes by the same deadline
// as the readers (a timer that fires before the counter reaches it, because the counter was set back, waits again).
func (t *readyTracker) capPassed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.rec.Done {
		return
	}
	if left := t.rec.DeadlineMS - readyClockMS(); left > 0 && left <= bootSetupCap.Milliseconds() {
		t.timer = time.AfterFunc(time.Duration(left)*time.Millisecond, t.capPassed)
		return
	}
	t.m.Log.Warnf("实例 %d 开机后调优超过 %.0f 秒还没做完（正在做：%s，约 %d%%）：状态已经先报在线，调优接着做", t.i,
		bootSetupCap.Seconds(), readyStepText(t.rec.Step), t.rec.Percent)
}

// step records the step the setup is at (from the progress events); "" keeps the one it had.
func (t *readyTracker) step(step string) {
	if t == nil || step == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.rec.Done || t.rec.Step == step {
		return
	}
	t.rec.Step = step
	t.write()
}

// percent records that done of total steps of afterBoot are done; only ever up.
func (t *readyTracker) percent(done, total int) {
	if t == nil || total <= 0 {
		return
	}
	t.setPercent(min(done, total) * readyAfterBootMax / total)
}

// sdcard records that the wait for /sdcard begins.
func (t *readyTracker) sdcard() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.rec.Step = readyStepSDCard
	t.mu.Unlock()
	t.setPercent(readySDCardFrom)
}

func (t *readyTracker) setPercent(p int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.rec.Done || p <= t.rec.Percent {
		return
	}
	t.rec.Percent = min(p, 99)
	t.write()
}

// finish records the setup as done: from now on the readers report online.
func (t *readyTracker) finish() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.rec.Done {
		return
	}
	t.rec.Done, t.rec.Percent = true, 100
	if err := t.write(); err != nil {
		// "done" cannot be written, so the readers would go on reading the step before it until the start is gone. Take
		// the record away and stop the marker's heartbeat: with neither, readers (status, the overlay) read the boot as
		// set up now. Whatever fails here, the start returning (its lock released) is the release all the same.
		t.m.Log.Warnf("实例 %d 记不下「调优做完」（%v）：状态直接按做完处理", t.i, err)
		t.m.removeReady(t.i)
		t.m.releaseSetupMarker(t.i)
	}
	if t.timer != nil {
		t.timer.Stop()
	}
}

// close ends the tracker (the start returns): a setup that is not done stays as it is, which the readers take for
// abandoned once the start lock is released.
func (t *readyTracker) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.timer != nil {
		t.timer.Stop()
	}
}

// readyStepText is a step as the log line of the cap names it.
func readyStepText(step string) string {
	switch step {
	case "vulkan_fix":
		return "客体库补丁"
	case "ime":
		return "宿主输入法"
	case "first_boot":
		return "首次开机初始化"
	case readyStepSDCard:
		return "等 /sdcard 可写"
	case readyStepBoot:
		return "开机收尾"
	}
	return "开机后调优"
}

// needReady starts the record of a take-over that finds it has real work to do (the instance is not initialized, or the
// host input method is installed or replaced), if it has none yet.
func (p *progressor) needReady() {
	if p != nil && p.ready == nil && p.lazyReady != nil {
		p.ready, p.lazyReady = p.lazyReady(), nil
		p.ready.step(p.lastStep)
	}
}

// readyStep tells the record the step the setup is at.
func (p *progressor) readyStep(step string) {
	if p != nil {
		p.ready.step(step)
	}
}

// guestBootIDOf is the guest's boot_id for the readers (one adb shell; "" when it cannot be read).
func (m *Manager) guestBootIDOf(ctx context.Context, i int) func() string {
	return func() string {
		_, id, _ := m.guestBoot(ctx, i, false)
		return id
	}
}

// releaseSetupMarker stops the heartbeat of the instance's setup marker (the readers then do not wait for a record
// that will not come); best effort.
func (m *Manager) releaseSetupMarker(i int) {
	if mk, ok := m.readSetupMarker(i); ok && mk.AliveMS != 0 {
		mk.AliveMS = 0
		m.writeSetupMarker(i, mk)
	}
}

// heartbeatDue: the boot wait refreshes the setup marker's AliveMS (last: when it did, now: the counter), at most this\n// often.
func heartbeatDue(last, now int64) bool {
	return now-last >= readyHeartbeatEvery.Milliseconds() || now < last
}
