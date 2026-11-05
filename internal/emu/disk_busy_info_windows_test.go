//go:build windows

package emu

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

// The same test binary acts as a real separate lock owner. Stdin gates the race
// and graceful release; killing this process tests kernel cleanup without defer.
func TestDiskBusyChild(t *testing.T) {
	dir := os.Getenv("MODAVD_DISK_BUSY_ROOT")
	if dir == "" {
		return
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(os.Stdin)
	if os.Getenv("MODAVD_DISK_BUSY_RACE") == "1" {
		fmt.Println("ready")
		reader.ReadString('\n')
	}
	unlock, err := New(r, nil).lockInstanceForCompact(3)
	if err != nil {
		if errs.CodeOf(err) != errs.Busy {
			t.Fatal(err)
		}
		fmt.Println("busy")
		return
	}
	defer unlock()
	fmt.Println("held")
	reader.ReadString('\n')
}

func diskBusyChild(t *testing.T, r *root.Root, race bool) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDiskBusyChild$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), "MODAVD_DISK_BUSY_ROOT="+r.Dir)
	if race {
		cmd.Env = append(cmd.Env, "MODAVD_DISK_BUSY_RACE=1")
	}
	win.HideConsoleCmd(cmd)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { in.Close(); cmd.Process.Kill(); cmd.Wait() })
	reader := bufio.NewReader(out)
	want := "held"
	if race {
		want = "ready"
	}
	if got, _ := reader.ReadString('\n'); strings.TrimSpace(got) != want {
		t.Fatalf("child: %q, want %s", got, want)
	}
	return cmd, in, reader
}

func diskBusyHeader(t *testing.T, r *root.Root, i int) string {
	t.Helper()
	b := make([]byte, 65536)
	binary.BigEndian.PutUint32(b, 0x514649fb)
	binary.BigEndian.PutUint32(b[20:], 16)
	binary.BigEndian.PutUint64(b[24:], 8<<30)
	p := filepath.Join(avd.Dir(r, i), userdataOverlay)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func assertDiskBusy(t *testing.T, f func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	begin := time.Now()
	err := f(ctx)
	if errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在整理磁盘") || time.Since(begin) >= time.Second {
		t.Fatalf("wanted prompt compact busy, elapsed=%v err=%v", time.Since(begin), err)
	}
}

func TestDiskBusyCrossProcessOperationsAndRecovery(t *testing.T) {
	for _, killed := range []bool{false, true} {
		t.Run(fmt.Sprintf("killed=%v", killed), func(t *testing.T) {
			r := managerTestRoot(t, 3, 4)
			m := New(r, nil)
			diskBusyHeader(t, r, 3)
			cmd, in, _ := diskBusyChild(t, r, false)
			assertDiskBusy(t, func(ctx context.Context) error { _, err := m.Start(ctx, 3, StartOptions{}); return err })
			assertDiskBusy(t, func(ctx context.Context) error { _, err := m.Delete(ctx, 3); return err })
			assertDiskBusy(t, func(ctx context.Context) error { _, err := m.Bridge(ctx, 3, "install"); return err })
			info, err := m.DiskInfo(context.Background(), 3)
			if err != nil || info.LogicalBytes != 8<<30 || info.OnDiskBytes == nil || info.AllocatedBytes != nil || info.ReclaimableBytes != nil || info.NeedsShutdown || !strings.Contains(strings.Join(info.Warnings, " "), "正在整理磁盘") {
				t.Fatalf("info during compact: %+v, %v", info, err)
			}
			if _, err := m.Delete(context.Background(), 4); err != nil {
				t.Fatalf("another instance blocked: %v", err)
			}
			other := New(managerTestRoot(t, 3), nil)
			unlock, err := other.lockInstanceForCompact(3)
			if err != nil {
				t.Fatalf("another root blocked: %v", err)
			}
			unlock()
			if killed {
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			} else {
				fmt.Fprintln(in, "release")
			}
			err = cmd.Wait()
			if !killed && err != nil {
				t.Fatal(err)
			}
			// The persistent lock files remain; their existence must never block retry.
			unlock, err = m.lockInstanceForCompact(3)
			if err != nil {
				t.Fatalf("retry after exit: %v", err)
			}
			unlock()
			if _, err := m.Delete(context.Background(), 3); err != nil {
				t.Fatalf("delete after exit: %v", err)
			}
		})
	}
}

func TestDiskBusySimultaneousProcesses(t *testing.T) {
	// Whichever operation wins its first conflicting lock must exclude the other.
	for _, kind := range []string{"start", "stop", "bridge", "create"} {
		for round := 0; round < 4; round++ {
			t.Run(fmt.Sprintf("%s/%d", kind, round), func(t *testing.T) {
				r := managerTestRoot(t, 3)
				cmd, in, reader := diskBusyChild(t, r, true)
				fmt.Fprintln(in, "race")
				l, ok, err := New(r, nil).tryOperationLock(3, kind)
				defer l.Unlock()
				line, _ := reader.ReadString('\n')
				held := strings.TrimSpace(line) == "held"
				if held && (ok || errs.CodeOf(err) != errs.Busy) {
					t.Fatalf("both won: child=%q ok=%v err=%v", line, ok, err)
				}
				if !held && strings.TrimSpace(line) != "busy" {
					t.Fatalf("child: %q", line)
				}
				fmt.Fprintln(in, "release")
				if err := cmd.Wait(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestDiskBusyDetectedWhileWaiting(t *testing.T) {
	m := New(managerTestRoot(t, 3), nil)
	start, _, err := win.TryLock(m.lockPath(3, "start"))
	if err != nil {
		t.Fatal(err)
	}
	defer start.Unlock()
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _, err := m.Start(ctx, 3, StartOptions{}); done <- err }()
	// Introduce disk ownership after the start has entered its wait. This also
	// covers compact acquiring disk immediately before it attempts start.
	time.Sleep(150 * time.Millisecond)
	disk, _, err := win.TryLock(m.lockPath(3, "disk"))
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Unlock()
	assertDiskBusy(t, func(context.Context) error { return <-done })
}

func TestDiskBusyConcurrentProbes(t *testing.T) {
	m := New(managerTestRoot(t, 3), nil)
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			for range 20 {
				if err := m.compactBusy(3); err != nil {
					t.Errorf("probe claimed compaction when none exists: %v", err)
					return
				}
			}
		})
	}
	group.Wait()
}

func TestDiskBusyOrdinaryStartAndPartialFailure(t *testing.T) {
	for _, kind := range []string{"start", "stop", "bridge", "create"} {
		t.Run(kind, func(t *testing.T) {
			m := New(managerTestRoot(t, 3), nil)
			l, _, err := win.TryLock(m.lockPath(3, kind))
			if err != nil {
				t.Fatal(err)
			}
			defer l.Unlock()
			if _, err := m.lockInstanceForCompact(3); errs.CodeOf(err) != errs.Busy || strings.Contains(err.Error(), "正在整理磁盘") {
				t.Fatalf("conflict: %v", err)
			}
			if err := m.compactBusy(3); err != nil {
				t.Fatalf("disk leaked: %v", err)
			}
			if kind == "start" {
				_, err := m.lock(context.Background(), 3, "start", 100*time.Millisecond, "正在启动")
				if errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在启动") {
					t.Fatalf("ordinary start: %v", err)
				}
				if _, err := m.Delete(context.Background(), 3); errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在启动") {
					t.Fatalf("delete during start: %v", err)
				}
			}
			l.Unlock()
			unlock, err := m.lockInstanceForCompact(3)
			if err != nil {
				t.Fatalf("retry: %v", err)
			}
			unlock()
		})
	}
	m := New(managerTestRoot(t, 3), nil)
	diskBusyHeader(t, m.Root, 3)
	// Actual compact exits after locking because this fixture has no qemu-img.
	if _, err := m.DiskCompact(context.Background(), 3); err == nil {
		t.Fatal("compact should fail")
	}
	unlock, err := m.lockInstanceForCompact(3)
	if err != nil {
		t.Fatalf("failed compact leaked locks: %v", err)
	}
	unlock()
}

func TestDiskBusyOpenFailureReleasesEarlierLocks(t *testing.T) {
	for _, kind := range []string{"start", "stop", "bridge", "create"} {
		t.Run(kind, func(t *testing.T) {
			m := New(managerTestRoot(t, 3), nil)
			path := m.lockPath(3, kind)
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			if _, err := m.lockInstanceForCompact(3); errs.CodeOf(err) != errs.Internal {
				t.Fatalf("want open failure: %v", err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			unlock, err := m.lockInstanceForCompact(3)
			if err != nil {
				t.Fatalf("earlier locks leaked: %v", err)
			}
			unlock()
		})
	}
}

func TestDiskInfoLockFallback(t *testing.T) {
	for _, mode := range []string{"start busy", "start unreadable", "disk unreadable", "invalid header", "unreadable image"} {
		t.Run(mode, func(t *testing.T) {
			m := New(managerTestRoot(t, 3), nil)
			image := diskBusyHeader(t, m.Root, 3)
			want := "未运行 qemu-img check"
			switch mode {
			case "start unreadable", "disk unreadable":
				kind := strings.Fields(mode)[0]
				if err := os.Mkdir(m.lockPath(3, kind), 0o755); err != nil {
					t.Fatal(err)
				}
			default:
				l, _, err := win.TryLock(m.lockPath(3, "start"))
				if err != nil {
					t.Fatal(err)
				}
				defer l.Unlock()
			}
			if mode == "invalid header" {
				if err := os.WriteFile(image, []byte("broken"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "unreadable image" {
				p, _ := syscall.UTF16PtrFromString(image)
				h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, 0, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer syscall.CloseHandle(h)
			}
			res, err := m.DiskInfo(context.Background(), 3)
			if err != nil || res.AllocatedBytes != nil || res.ReclaimableBytes != nil || !strings.Contains(strings.Join(res.Warnings, " "), want) {
				t.Fatalf("fallback: %+v %v", res, err)
			}
			if mode != "invalid header" && mode != "unreadable image" && (res.LogicalBytes != 8<<30 || res.OnDiskBytes == nil) {
				t.Fatalf("lost safe data: %+v", res)
			}
			if mode == "unreadable image" && res.OnDiskBytes == nil {
				b, _ := json.Marshal(res)
				if strings.Contains(string(b), "on_disk_bytes") {
					t.Fatalf("unknown allocation serialized: %s", b)
				}
			}
		})
	}
}
