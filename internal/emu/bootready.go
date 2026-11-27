package emu

// When an instance is "online". Android having booted (sys.boot_completed) is not enough: the after-boot setup of
// that boot (afterBoot: Vulkan fix, host scheduling, host input method, first-boot setup, writing to /sdcard ...) goes
// on for seconds after it, and a tool that starts working then meets a guest that is not set up. So status / list /
// info report starting (the only value they have for it; Busy says starting too) until the setup of this boot is done,
// or the cap passed: bootSetupCap after the start began it.
//
// The start that does the setup (start.go) keeps a small record, <root>\avd\.mas_<i>.ready, that any other process
// reads: the qemu process it is for (PID + creation time, as the setup marker and the boot record do, so a record of
// an earlier boot never names the qemu of this one), when the setup began, the step it is at and how far it is. It is
// not the boot record: that one means "set up", and the boot watch registers the instance with the default adb server
// (5037) only once it exists, i.e. after the setup; this one exists while the setup runs.
//
// Nothing is waited for that nobody does:
//   - a qemu that no modavd start works on (started by another tool, or by a start that was cancelled or killed: the
//     start lock is released with the process) is online as soon as Android has booted, as before;
//   - a start that works on it for longer than bootSetupCap stops holding it back (the start goes on, and logs which
//     step it was at);
//   - a record of another qemu is no record.
//
// A guest that reboots by itself under the same qemu reads like a new boot: the boot watch (bootwatch.go) runs Start
// again, which finds the boot_id changed, treats the boot as owed (setupOwed) and writes a new record. The seconds
// between the reboot and the watch's next look still read online, since nobody has started setting the new boot up.
// A plain adopt of a boot that is up and set up (setupAdopted) writes no record: it only checks, a user may be in the
// guest, and the instance stays online.

import (
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

// Steps of the setup (InstanceInfo.SetupStep): the progress steps of start.go (progressor.emit) plus "boot" for what
// comes before the first of them and "sdcard" for the wait for /sdcard after them. The percentage of afterBoot's
// steps counts what is done, up to readyAfterBootMax; the wait for /sdcard is the rest.
const (
	readyStepBoot   = "boot"
	readyStepSDCard = "sdcard"

	readyAfterBootMax = 90
	readySDCardFrom   = 95
)

// readyRecord is the content of readyPath.
type readyRecord struct {
	QemuPID     uint32 `json:"qemu_pid"`
	QemuCreated uint64 `json:"qemu_created"`
	BootID      string `json:"boot_id,omitempty"`
	// StartedAtMS (Unix milliseconds): when the start began this boot's setup, and so what the cap counts from.
	StartedAtMS int64  `json:"started_at_ms"`
	Step        string `json:"step"`
	Percent     int    `json:"percent"`
	Done        bool   `json:"done"`
}

// names: q is the qemu process this record was written for.
func (r readyRecord) names(q host.Process) bool {
	return r.QemuPID != 0 && r.QemuCreated != 0 && r.QemuPID == q.PID && r.QemuCreated == q.Created
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
// holds the instance's start lock right now (a start is running). Android has booted already when this is asked.
func (m *Manager) bootSetupOf(i int, q host.Process, startHeld bool, now time.Time) bootSetupState {
	if rec, ok := m.readReady(i); ok && rec.names(q) {
		if rec.Done || !startHeld {
			// done, or the start that was doing it is gone (cancelled, killed, failed): nobody will finish it
			return bootSetupState{Ready: true}
		}
		if started := time.UnixMilli(rec.StartedAtMS); !now.Before(started.Add(bootSetupCap)) {
			return bootSetupState{Ready: true, Unfinished: rec.Step}
		}
		return bootSetupState{Step: rec.Step, Progress: rec.Percent}
	}
	// No record of this qemu yet. A start that launched it (or took over an unfinished boot) left the setup marker
	// and writes the record within seconds of Android's boot; any other qemu has no setup to wait for.
	if startHeld {
		mk, ok := m.readSetupMarker(i)
		if use, _ := setupMarkerUse(mk, ok, q, now); use {
			return bootSetupState{Step: readyStepBoot}
		}
	}
	return bootSetupState{Ready: true}
}

// readyTracker is the writer of the record, owned by the start that does a boot's setup. A nil tracker does nothing.
type readyTracker struct {
	m *Manager
	i int

	mu     sync.Mutex
	rec    readyRecord
	timer  *time.Timer
	closed bool
}

// beginReady writes the record of the setup that begins now for qemu q (bootID: the guest's boot_id, "" if unknown)
// and arms the cap's log line.
func (m *Manager) beginReady(i int, q host.Process, bootID string) *readyTracker {
	t := &readyTracker{m: m, i: i, rec: readyRecord{QemuPID: q.PID, QemuCreated: q.Created, BootID: bootID,
		StartedAtMS: time.Now().UnixMilli(), Step: readyStepBoot}}
	t.mu.Lock()
	t.write()
	t.timer = time.AfterFunc(bootSetupCap, t.capPassed)
	t.mu.Unlock()
	return t
}

// write stores the record (t.mu held). A failure is only noted: the readers then see no record and treat the boot as
// one nobody sets up.
func (t *readyTracker) write() {
	b, err := json.Marshal(t.rec)
	if err == nil {
		err = writeFileReplace(t.m.readyPath(t.i), b, strconv.Itoa(os.Getpid()))
	}
	if err != nil {
		t.m.Log.Debugf("实例 %d 记不下开机调优进度: %v", t.i, err)
	}
}

// capPassed is the timer: the setup is still going on when status stopped waiting for it.
func (t *readyTracker) capPassed() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.rec.Done {
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
	t.write()
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
	case "tune":
		return "开机后调优"
	case "ime":
		return "宿主输入法"
	case "first_boot":
		return "首次开机初始化"
	case readyStepSDCard:
		return "等 /sdcard 可写"
	}
	return "开机收尾"
}
