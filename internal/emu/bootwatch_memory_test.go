//go:build windows

package emu

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"modavd/internal/host"
	"modavd/internal/logx"
)

// logMemory writes a line at the first look and then once per bootWatchMemoryInterval, with qemu's and the computer's
// numbers in MB; nothing while qemu's counters cannot be read, and only qemu's part without the computer's.
func TestBootWatchLogMemory(t *testing.T) {
	savedP, savedS := processMemory, systemMemory
	t.Cleanup(func() { processMemory, systemMemory = savedP, savedS })
	const mb = 1 << 20
	var perr, serr error
	processMemory = func(p host.Process) (host.ProcessMemory, error) {
		if p.PID != 4242 {
			t.Errorf("asked for pid %d", p.PID)
		}
		return host.ProcessMemory{Commit: 6100 * mb, PeakCommit: 6200 * mb, WorkingSet: 4300 * mb}, perr
	}
	systemMemory = func() (host.SystemMemory, error) {
		return host.SystemMemory{TotalPhys: 15728 * mb, AvailPhys: 4400 * mb, CommitLimit: 41574 * mb, Committed: 38600 * mb}, serr
	}
	m, lines := recordingManager()
	w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 1}}
	t0 := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)

	perr = errors.New("gone")
	w.logMemory(t0)
	if len(*lines) != 0 {
		t.Fatalf("logged without counters: %+v", *lines)
	}
	perr = nil
	w.logMemory(t0)
	w.logMemory(t0.Add(bootWatchMemoryInterval - time.Second))
	if len(*lines) != 1 {
		t.Fatalf("lines = %+v, want one", *lines)
	}
	want := "实例 3 模拟器内存（qemu pid 4242）：提交 6100 MB（最高 6200 MB），工作集 4300 MB；电脑：物理内存可用 4400 / 15728 MB，提交 38600 / 41574 MB"
	if got := (*lines)[0]; got.level != logx.Info || got.msg != want {
		t.Errorf("line = %v %q\nwant %q", got.level, got.msg, want)
	}
	serr = errors.New("no")
	w.logMemory(t0.Add(bootWatchMemoryInterval))
	if len(*lines) != 2 || !strings.HasSuffix((*lines)[1].msg, "工作集 4300 MB") {
		t.Errorf("without the computer's numbers: %+v", *lines)
	}
}

// The real counters read this test process and the computer.
func TestMemoryCounters(t *testing.T) {
	p, ok := host.FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("own process not found")
	}
	pm, err := host.GetProcessMemory(p)
	if err != nil || pm.Commit == 0 || pm.WorkingSet == 0 || pm.PeakCommit < pm.Commit {
		t.Errorf("process memory = %+v, %v", pm, err)
	}
	sm, err := host.GetSystemMemory()
	if err != nil || sm.TotalPhys == 0 || sm.AvailPhys > sm.TotalPhys || sm.CommitLimit < sm.TotalPhys ||
		sm.Committed == 0 || sm.Committed > sm.CommitLimit {
		t.Errorf("system memory = %+v, %v", sm, err)
	}
	if _, err := host.GetProcessMemory(host.Process{PID: p.PID, Created: p.Created + 1}); err == nil {
		t.Error("a reused PID read as the process")
	}
}
