//go:build windows

package emu

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/host"
	"modavd/internal/logx"
)

// Successful reads resume after failure; only emitted samples query the system counters.
func TestBootWatchLogMemory(t *testing.T) {
	savedP, savedS := processMemory, systemMemory
	t.Cleanup(func() { processMemory, systemMemory = savedP, savedS })
	const mb = 1 << 20
	var perr, serr error
	systemCalls := 0
	processMemory = func(p host.Process) (host.ProcessMemory, error) {
		if p.PID != 4242 {
			t.Errorf("asked for pid %d", p.PID)
		}
		return host.ProcessMemory{Commit: 6100 * mb, PeakCommit: 6200 * mb, WorkingSet: 4300 * mb}, perr
	}
	systemMemory = func() (host.SystemMemory, error) {
		systemCalls++
		return host.SystemMemory{TotalPhys: 15728 * mb, AvailPhys: 4400 * mb, CommitLimit: 41574 * mb, Committed: 38600 * mb}, serr
	}
	m, lines := recordingManager()
	w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 1}}
	t0 := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)

	perr = errors.New("gone")
	w.logMemory(t0)
	if len(*lines) != 1 || (*lines)[0].level != logx.Warn || systemCalls != 0 {
		t.Fatalf("failed read: %+v, system calls %d", *lines, systemCalls)
	}
	perr = nil
	w.logMemory(t0.Add(time.Minute))
	w.logMemory(t0.Add(bootWatchMemoryInterval))
	if len(*lines) != 2 || systemCalls != 1 {
		t.Fatalf("lines = %+v, system calls %d, want recovery only", *lines, systemCalls)
	}
	want := "实例 3 模拟器内存（qemu pid 4242）：提交 6100 MB（OS 提交峰值 6200 MB），工作集 4300 MB；电脑：物理内存可用 4400 / 15728 MB，提交 38600 / 41574 MB；内存采样已恢复"
	if got := (*lines)[1]; got.level != logx.Info || got.msg != want {
		t.Errorf("line = %v %q\nwant %q", got.level, got.msg, want)
	}
	serr = errors.New("no")
	w.logMemory(t0.Add(time.Minute + bootWatchMemoryInterval))
	if len(*lines) != 3 || !strings.HasSuffix((*lines)[2].msg, "工作集 4300 MB") {
		t.Errorf("without the computer's numbers: %+v", *lines)
	}
}

// Retaining the process object does not make post-exit memory a valid live snapshot. Windows can return success
// with most allocations already torn down, so the memory boundary must check exit as well as creation identity.
func TestMemoryRejectsRetainedExitedProcess(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestBootWatchHelperSleeper$")
	cmd.Env = append(os.Environ(), "MODAVD_TEST_BOOTWATCH_SLEEPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	p, ok := host.FindProcess(uint32(cmd.Process.Pid))
	if !ok {
		t.Fatal("child not found")
	}
	h, err := syscall.OpenProcess(0x1000, false, p.PID) // PROCESS_QUERY_LIMITED_INFORMATION, retained through Wait
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if _, err := host.GetProcessMemory(p); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if pm, err := host.GetProcessMemory(p); err == nil {
		t.Fatalf("accepted torn-down counters of exited process: %+v", pm)
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
