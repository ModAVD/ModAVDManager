//go:build windows

// Copyright (C) 2026 jinghero

package emu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"modavd/internal/host"
)

// A minute's growth must be visible before the ten-minute heartbeat.
func TestBootWatchMemoryDeltaRegression(t *testing.T) {
	savedP, savedS := processMemory, systemMemory
	t.Cleanup(func() { processMemory, systemMemory = savedP, savedS })
	const mb = 1 << 20
	commit := uint64(1000 * mb)
	processMemory = func(host.Process) (host.ProcessMemory, error) {
		return host.ProcessMemory{Commit: commit, PeakCommit: commit, WorkingSet: 500 * mb}, nil
	}
	systemMemory = func() (host.SystemMemory, error) { return host.SystemMemory{}, nil }
	m, lines := recordingManager()
	w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 1}}
	t0 := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)
	w.logMemory(t0)
	commit += 257 * mb
	w.logMemory(t0.Add(time.Minute))
	if len(*lines) != 2 {
		t.Fatalf("257 MiB growth after a minute: %v, want two lines", *lines)
	}
}

func TestBootWatchMemoryPolicy(t *testing.T) {
	const mb = 1 << 20
	type sample struct {
		minute int
		commit uint64
		lines  int
	}
	for _, tc := range []struct {
		name    string
		samples []sample
	}{
		{"stable heartbeat", []sample{{0, 1000 * mb, 1}, {1, 1000 * mb, 1}, {9, 1000 * mb, 1}, {10, 1000 * mb, 2}}},
		{"cumulative strict threshold", []sample{{0, 1000 * mb, 1}, {1, 1128 * mb, 1}, {2, 1256 * mb, 1}, {3, 1256*mb + 1, 2}}},
		{"falling strict threshold", []sample{{0, 1000 * mb, 1}, {1, 744 * mb, 1}, {2, 744*mb - 1, 2}}},
		{"heartbeat resets on write", []sample{{0, 1000 * mb, 1}, {3, 1300 * mb, 2}, {10, 1300 * mb, 2}, {12, 1300 * mb, 2}, {13, 1300 * mb, 3}}},
		{"unsigned large difference", []sample{{0, 1, 1}, {1, 1 << 63, 2}, {2, 1, 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savedP, savedS := processMemory, systemMemory
			t.Cleanup(func() { processMemory, systemMemory = savedP, savedS })
			var commit uint64
			reads, systemReads := 0, 0
			processMemory = func(p host.Process) (host.ProcessMemory, error) {
				if p.PID != 4242 || p.Created != 17 {
					t.Fatalf("lost process identity: %+v", p)
				}
				reads++
				return host.ProcessMemory{Commit: commit, PeakCommit: commit, WorkingSet: commit}, nil
			}
			systemMemory = func() (host.SystemMemory, error) {
				systemReads++
				return host.SystemMemory{}, nil
			}
			m, lines := recordingManager()
			w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 17}}
			t0 := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)
			for i, s := range tc.samples {
				commit = s.commit
				w.logMemory(t0.Add(time.Duration(s.minute) * time.Minute))
				if len(*lines) != s.lines || systemReads != s.lines || reads != i+1 {
					t.Fatalf("minute %d: logs %v, process reads %d, system reads %d", s.minute, *lines, reads, systemReads)
				}
			}
		})
	}
}

func TestBootWatchMemorySummary(t *testing.T) {
	savedP, savedS := processMemory, systemMemory
	t.Cleanup(func() { processMemory, systemMemory = savedP, savedS })
	const mb = 1 << 20
	reads, systemReads := 0, 0
	var pm host.ProcessMemory
	var readErr error
	processMemory = func(host.Process) (host.ProcessMemory, error) { reads++; return pm, readErr }
	systemMemory = func() (host.SystemMemory, error) { systemReads++; return host.SystemMemory{}, nil }
	m, lines := recordingManager()
	w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 1}}
	t0 := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)
	pm = host.ProcessMemory{Commit: 1000 * mb, PeakCommit: 2000 * mb, WorkingSet: 200 * mb}
	w.logMemory(t0)
	pm = host.ProcessMemory{Commit: 1100 * mb, PeakCommit: 2200 * mb, WorkingSet: 350 * mb}
	w.logMemory(t0.Add(time.Minute)) // sampled but not written
	pm = host.ProcessMemory{Commit: 900 * mb, PeakCommit: 2300 * mb, WorkingSet: 300 * mb}
	w.logMemory(t0.Add(2 * time.Minute))
	readErr = errors.New("denied")
	pm = host.ProcessMemory{Commit: 99999 * mb} // partial result with error must never become history
	w.logMemory(t0.Add(3 * time.Minute))
	w.logMemory(t0.Add(4 * time.Minute))
	w.logMemorySummary()
	want := "实例 3 模拟器内存退出汇总（qemu pid 4242）：成功采样 3 次，读取失败 2 次；最后成功采样 2026-11-12T20:02:00Z：提交 900 MB，OS 提交峰值 2300 MB，工作集 300 MB；采样最大提交 1100 MB，工作集 350 MB；最近读取失败：denied；精确退出峰值未知"
	if len(*lines) != 3 || (*lines)[2].msg != want || reads != 5 || systemReads != 1 {
		t.Fatalf("summary = %v, process reads %d, system reads %d", *lines, reads, systemReads)
	}
}

func TestBootWatchMemoryNoSuccessfulSample(t *testing.T) {
	savedP, savedS := processMemory, systemMemory
	t.Cleanup(func() { processMemory, systemMemory = savedP, savedS })
	processMemory = func(host.Process) (host.ProcessMemory, error) {
		return host.ProcessMemory{}, errors.New("unreadable")
	}
	systemMemory = func() (host.SystemMemory, error) {
		t.Fatal("queried system after failed sample")
		return host.SystemMemory{}, nil
	}
	m, lines := recordingManager()
	w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 1}}
	w.logMemory(time.Now())
	w.logMemory(time.Now().Add(time.Minute))
	w.logMemorySummary()
	if len(*lines) != 2 || !strings.Contains((*lines)[1].msg, "成功采样 0 次，读取失败 2 次；没有成功样本") ||
		!strings.HasSuffix((*lines)[1].msg, "精确退出峰值未知") || strings.Contains((*lines)[1].msg, "0 MB") {
		t.Fatalf("unknown counters presented as numbers: %v", *lines)
	}
}

func TestBootWatchMemoryFailureRecovery(t *testing.T) {
	savedP, savedS := processMemory, systemMemory
	t.Cleanup(func() { processMemory, systemMemory = savedP, savedS })
	var readErr error
	processMemory = func(host.Process) (host.ProcessMemory, error) {
		return host.ProcessMemory{Commit: 1 << 30}, readErr
	}
	systemMemory = func() (host.SystemMemory, error) { return host.SystemMemory{}, nil }
	m, lines := recordingManager()
	w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 1}}
	t0 := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)
	w.logMemory(t0)
	readErr = errors.New("denied")
	w.logMemory(t0.Add(10 * time.Minute))
	w.logMemory(t0.Add(11 * time.Minute))
	if w.memory.logged.at != t0 || w.memory.last.at != t0 {
		t.Fatal("failure moved the successful/logged baseline")
	}
	readErr = nil
	w.logMemory(t0.Add(12 * time.Minute))
	w.logMemory(t0.Add(21 * time.Minute))
	w.logMemory(t0.Add(22 * time.Minute))
	w.logMemorySummary()
	if len(*lines) != 5 || !strings.HasSuffix((*lines)[2].msg, "内存采样已恢复") ||
		strings.Contains((*lines)[4].msg, "最近读取失败") || !strings.Contains((*lines)[4].msg, "读取失败 2 次") {
		t.Fatalf("recovery/heartbeat = %v", *lines)
	}
}

// A guest read can block the reboot/stop path, but cannot block memory sampling or its exit summary.
func TestBootWatchMemoryIndependentOfGuest(t *testing.T) {
	savedP, savedS, savedGuest := processMemory, systemMemory, guestBootRead
	t.Cleanup(func() { processMemory, systemMemory, guestBootRead = savedP, savedS, savedGuest })
	rootManager, _ := opsTestManager(t)
	i := 3
	if _, err := rootManager.Create(context.Background(), CreateRequest{filesOnly: true, Index: &i}); err != nil {
		t.Fatal(err)
	}
	m, lines := recordingManager()
	m.Root = rootManager.Root
	w := &bootWatch{m: m, i: 3, q: host.Process{PID: 4242, Created: 1}}
	m.writeBootRecord(3, bootRecord{QemuPID: w.q.PID, QemuCreated: w.q.Created, BootID: testBootA})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	guestBootRead = func(int, bool) (bool, string, bool) {
		close(entered)
		<-ctx.Done()
		return false, "", false
	}
	guestDone := make(chan struct{})
	go func() { defer close(guestDone); w.tick(ctx, time.Now()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("guest read never started")
	}
	read := make(chan struct{}, 2)
	processMemory = func(host.Process) (host.ProcessMemory, error) {
		read <- struct{}{}
		return host.ProcessMemory{Commit: 1 << 30}, nil
	}
	systemMemory = func() (host.SystemMemory, error) { return host.SystemMemory{}, nil }
	ticks := make(chan time.Time)
	done := make(chan struct{})
	w.logMemory(time.Now())
	go func() { defer close(done); w.watchMemory(ctx, ticks) }()
	awaitRead := func() {
		t.Helper()
		select {
		case <-read:
		case <-time.After(5 * time.Second):
			t.Fatal("memory sample blocked by guest read")
		}
	}
	awaitRead()
	ticks <- time.Now().Add(time.Minute)
	awaitRead()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("memory worker did not stop")
	}
	<-guestDone
	if w.memory.samples != 2 || len(read) != 0 || len(*lines) != 2 || !strings.Contains((*lines)[1].msg, "内存退出汇总") {
		t.Fatalf("sampling/exit read count = %d, extra reads %d, logs %v", w.memory.samples, len(read), *lines)
	}
}
