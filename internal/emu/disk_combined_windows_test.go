//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

const combinedEnv = "MODAVD_TEST_DISK_COMBINED"

type combinedRequest struct{ Real, Overlay, Stage string }

// Forward to the existing real qemu-img, except at a controller-owned gate.
// The gate holds the original and scratch files like an active image operation.
func init() {
	if path := os.Getenv(combinedEnv); path != "" && strings.EqualFold(filepath.Base(os.Args[0]), "qemu-img.exe") {
		os.Exit(combinedChild(path))
	}
}

func combinedChild(path string) int {
	b, err := os.ReadFile(path)
	var req combinedRequest
	if err != nil || json.Unmarshal(b, &req) != nil || len(os.Args) < 2 {
		return 90
	}
	stage := os.Args[1]
	f, err := os.OpenFile(path+".calls", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 91
	}
	fmt.Fprintln(f, stage)
	f.Close()
	if stage == req.Stage {
		p, _ := syscall.UTF16PtrFromString(req.Overlay)
		h, err := syscall.CreateFile(p, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0, 0)
		if err != nil {
			return 92
		}
		defer syscall.CloseHandle(h)
		if stage == "convert" {
			if err := os.WriteFile(os.Args[len(os.Args)-1], []byte("incomplete conversion"), 0600); err != nil {
				return 93
			}
		}
		p, _ = syscall.UTF16PtrFromString(filepath.Join(filepath.Dir(req.Overlay), userdataCompactTmp))
		scratch, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0, 0)
		if err != nil {
			return 94
		}
		defer syscall.CloseHandle(scratch)
		p, _ = syscall.UTF16PtrFromString(path + ".held")
		held, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.CREATE_ALWAYS, 0, 0)
		if err != nil {
			return 94
		}
		defer syscall.CloseHandle(held)
		if err := os.WriteFile(path+".ready.tmp", []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			return 95
		}
		if err := os.Rename(path+".ready.tmp", path+".ready"); err != nil {
			return 95
		}
		// The controller must interrupt this stage. Expiry is a test failure,
		// never a successful conversion after an ineffective cancellation.
		time.Sleep(time.Minute)
		return 97
	}
	cmd := exec.Command(req.Real, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	win.HideConsoleCmd(cmd)
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			return e.ExitCode()
		}
		return 96
	}
	return 0
}

func TestCombinedCompactParent(t *testing.T) {
	dir := os.Getenv("MODAVD_TEST_COMBINED_ROOT")
	if dir == "" {
		t.Skip("subprocess helper requires MODAVD_TEST_COMBINED_ROOT")
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	var ctx context.Context
	var cancel context.CancelFunc
	if os.Getenv("MODAVD_TEST_COMBINED_MODE") == "deadline" {
		ctx, cancel = context.WithTimeout(context.Background(), 20*time.Second)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	defer cancel()
	if os.Getenv("MODAVD_TEST_COMBINED_MODE") == "cancel" {
		go func() {
			for ctx.Err() == nil {
				if _, err := os.Stat(os.Getenv(combinedEnv) + ".cancel"); err == nil {
					cancel()
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	res, err := New(r, nil).DiskCompact(ctx, 2)
	if err == nil {
		t.Fatalf("expected interrupted compact, got %+v", res)
	}
	if errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("want canceled: %v", err)
	}
	if os.Getenv("MODAVD_TEST_COMBINED_MODE") == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want caller deadline: %v", err)
	}
}

func combinedFixture(t *testing.T) (*Manager, string, string, []byte) {
	t.Helper()
	sdk := os.Getenv("MODAVD_TEST_SDK_ROOT")
	if sdk == "" {
		t.Skip("requires existing qemu-img SDK")
	}
	real := filepath.Join(sdk, "emulator", "qemu-img.exe")
	m := New(managerTestRoot(t, 2), nil)
	in, err := m.instance(2)
	if err != nil {
		t.Fatal(err)
	}
	qemu := func(args ...string) {
		t.Helper()
		cmd := exec.Command(real, args...)
		cmd.Dir = in.Dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	raw := filepath.Join(m.Root.Dir, "source.raw")
	data := make([]byte, 64<<20)
	copy(data[1<<20:], bytes.Repeat([]byte{0x31}, 64<<10))
	copy(data[32<<20:], bytes.Repeat([]byte{0x72}, 64<<10))
	if err := os.WriteFile(raw, data, 0600); err != nil {
		t.Fatal(err)
	}
	qemu("convert", "-f", "raw", "-O", "qcow2", raw, userdataBacking)
	clear(data)
	copy(data[32<<20:], bytes.Repeat([]byte{0xa5}, 64<<10))
	if err := os.WriteFile(raw, data, 0600); err != nil {
		t.Fatal(err)
	}
	qemu("convert", "-f", "raw", "-O", "qcow2", raw, userdataOverlay)
	qemu("rebase", "-u", "-f", "qcow2", "-F", "qcow2", "-b", userdataBacking, userdataOverlay)
	copy(data[1<<20:], bytes.Repeat([]byte{0x31}, 64<<10))
	overlay := filepath.Join(in.Dir, userdataOverlay)
	addCompactGarbage(t, overlay)
	copyTestExe(t, filepath.Join(m.Root.RuntimeSDK(), "emulator", "qemu-img.exe"))
	return m, real, overlay, data
}

func combinedWriteRequest(t *testing.T, path string, req combinedRequest) {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCombinedCompactInterruption(t *testing.T) {
	for _, mode := range []string{"kill", "cancel", "deadline"} {
		for _, stage := range []string{"convert", "compare"} {
			t.Run(mode+"/"+stage, func(t *testing.T) {
				m, real, overlay, expected := combinedFixture(t)
				request := filepath.Join(m.Root.Dir, "request.json")
				combinedWriteRequest(t, request, combinedRequest{real, overlay, stage})
				t.Setenv(combinedEnv, request)
				before, err := os.ReadFile(overlay)
				if err != nil {
					t.Fatal(err)
				}
				id := compactFileID(t, overlay)
				backing := filepath.Join(filepath.Dir(overlay), userdataBacking)
				backingBefore, err := os.ReadFile(backing)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestCombinedCompactParent$", "-test.timeout=45s")
				cmd.Env = append(os.Environ(), "MODAVD_TEST_COMBINED_ROOT="+m.Root.Dir, "MODAVD_TEST_COMBINED_MODE="+mode)
				var output bytes.Buffer
				cmd.Stdout, cmd.Stderr = &output, &output
				win.HideConsoleCmd(cmd)
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				finished := make(chan struct{})
				var parentErr error
				go func() { parentErr = cmd.Wait(); close(finished) }()
				t.Cleanup(func() { cmd.Process.Kill(); <-finished })
				h := lifetimeAwaitChild(t, request)
				calls, err := os.ReadFile(request + ".calls")
				if err != nil {
					t.Fatal(err)
				}
				assertDiskBusy(t, func(ctx context.Context) error { _, err := m.Start(ctx, 2, StartOptions{}); return err })
				assertDiskBusy(t, func(ctx context.Context) error { _, err := m.Delete(ctx, 2); return err })
				assertDiskBusy(t, func(ctx context.Context) error { _, err := m.Bridge(ctx, 2, "install"); return err })
				assertDiskBusy(t, func(ctx context.Context) error { _, err := m.DiskCompact(ctx, 2); return err })
				start := time.Now()
				info, err := m.DiskInfo(context.Background(), 2)
				if err != nil || info.AllocatedBytes != nil || info.ReclaimableBytes != nil || info.LogicalBytes != 64<<20 || info.OnDiskBytes == nil || !strings.Contains(strings.Join(info.Warnings, " "), "正在整理磁盘") || time.Since(start) > time.Second {
					t.Fatalf("unsafe info: %+v %v", info, err)
				}
				if after, err := os.ReadFile(request + ".calls"); err != nil || !bytes.Equal(calls, after) {
					t.Fatalf("busy/info launched qemu-img: %s %v", after, err)
				}
				if mode == "kill" {
					if err := cmd.Process.Kill(); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "cancel" {
					if err := os.WriteFile(request+".cancel", nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-finished:
				case <-time.After(30 * time.Second):
					t.Fatal("parent did not finish")
				}
				if mode != "kill" && parentErr != nil {
					t.Fatalf("parent: %v\n%s", parentErr, &output)
				}
				lifetimeAssertExited(t, h, request)
				if after, err := os.ReadFile(overlay); err != nil || !bytes.Equal(before, after) || id != compactFileID(t, overlay) {
					t.Fatalf("interruption changed original: %v", err)
				}
				unlock, err := m.lockInstanceForCompact(2)
				if err != nil {
					t.Fatal(err)
				}
				unlock()
				tmp := filepath.Join(filepath.Dir(overlay), userdataCompactTmp)
				if mode != "kill" {
					if _, err := os.Stat(tmp); !os.IsNotExist(err) {
						t.Fatalf("scratch after cancel/deadline: %v", err)
					}
				}
				combinedWriteRequest(t, request, combinedRequest{real, overlay, ""})
				info, err = m.DiskInfo(context.Background(), 2)
				if err != nil || info.AllocatedBytes == nil {
					t.Fatalf("offline info did not recover: %+v %v", info, err)
				}
				res, err := m.DiskCompact(context.Background(), 2)
				if err != nil || res.Skipped || res.FreedBytes < 16<<20 || id == compactFileID(t, overlay) {
					t.Fatalf("retry: %+v %v", res, err)
				}
				if _, err := os.Stat(tmp); !os.IsNotExist(err) {
					t.Fatalf("retry left scratch: %v", err)
				}
				outRaw := filepath.Join(m.Root.Dir, "after.raw")
				if out, err := exec.Command(real, "convert", "-O", "raw", overlay, outRaw).CombinedOutput(); err != nil {
					t.Fatalf("unreadable original: %v %s", err, out)
				}
				if data, err := os.ReadFile(outRaw); err != nil || !bytes.Equal(data, expected) {
					t.Fatalf("guest content after retry: %v", err)
				}
				if b, err := os.ReadFile(backing); err != nil || !bytes.Equal(b, backingBefore) {
					t.Fatalf("backing changed: %v", err)
				}
				res, err = m.DiskCompact(context.Background(), 2)
				if err != nil || !res.Skipped {
					t.Fatalf("second retry did not skip: %+v %v", res, err)
				}
			})
		}
	}
}

func TestCombinedLinkedLowBenefit(t *testing.T) {
	for _, kind := range []string{"hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			m, real, overlay, _ := combinedFixture(t)
			request := filepath.Join(m.Root.Dir, "request.json")
			combinedWriteRequest(t, request, combinedRequest{real, overlay, ""})
			t.Setenv(combinedEnv, request)
			if res, err := m.DiskCompact(context.Background(), 2); err != nil || res.Skipped {
				t.Fatalf("initial rewrite: %+v %v", res, err)
			}
			alias := filepath.Join(filepath.Dir(overlay), "alias.qcow2")
			if kind == "hardlink" {
				if err := os.Link(overlay, alias); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Rename(overlay, alias); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(alias, overlay); err != nil {
					if errors.Is(err, syscall.ERROR_PRIVILEGE_NOT_HELD) {
						t.Skipf("account cannot create file symlinks: %v", err)
					}
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(alias)
			if err != nil {
				t.Fatal(err)
			}
			id := compactFileID(t, alias)
			tmp := filepath.Join(filepath.Dir(overlay), userdataCompactTmp)
			if err := os.WriteFile(tmp, []byte("leave this scratch"), 0600); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(request + ".calls")
			if err != nil {
				t.Fatal(err)
			}
			for _, busy := range []bool{false, true} {
				var unlock func()
				if busy {
					unlock, err = m.lockInstanceForCompact(2)
					if err != nil {
						t.Fatal(err)
					}
				}
				res, err := m.DiskCompact(context.Background(), 2)
				if busy {
					unlock()
				}
				if res != nil || errs.CodeOf(err) != errs.Unsupported {
					t.Fatalf("link bypassed by skip/lock: %+v %v", res, err)
				}
			}
			if after, err := os.ReadFile(alias); err != nil || !bytes.Equal(before, after) || id != compactFileID(t, alias) {
				t.Fatalf("linked data changed: %v", err)
			}
			if after, err := os.ReadFile(request + ".calls"); err != nil || !bytes.Equal(calls, after) {
				t.Fatalf("link invoked qemu-img: %v", err)
			}
			if b, err := os.ReadFile(tmp); err != nil || string(b) != "leave this scratch" {
				t.Fatalf("link cleaned scratch: %v", err)
			}
		})
	}
}
