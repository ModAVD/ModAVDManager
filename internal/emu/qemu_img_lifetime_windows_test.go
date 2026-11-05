//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

const lifetimeEnv = "MODAVD_TEST_QEMU_IMG_LIFETIME"

type lifetimeRequest struct {
	Hold           bool
	EchoArgs       bool
	Code           int
	Stdout, Stderr string
}

// A copy of this test executable accepts real qemu-img argument lists, without
// changing the package's shared TestMain or the disk integration test.
func init() {
	if os.Getenv(lifetimeEnv) == "1" && strings.EqualFold(filepath.Base(os.Args[0]), "qemu-img.exe") {
		os.Exit(lifetimeChild())
	}
}

func lifetimeChild() int {
	path := os.Args[len(os.Args)-1]
	b, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 90
	}
	var req lifetimeRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return 91
	}
	if req.Hold {
		p, err := syscall.UTF16PtrFromString(path + ".held")
		if err != nil {
			return 92
		}
		h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.CREATE_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 93
		}
		defer syscall.CloseHandle(h)
		if err := os.WriteFile(path+".ready", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			return 94
		}
		deadline := time.Now().Add(time.Minute)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(path + ".release"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if req.EchoArgs {
		b, _ := json.Marshal(os.Args[1:])
		fmt.Fprint(os.Stdout, string(b))
	} else {
		fmt.Fprint(os.Stdout, req.Stdout)
	}
	fmt.Fprint(os.Stderr, req.Stderr)
	return req.Code
}

func lifetimeManager(t *testing.T) *Manager {
	t.Helper()
	m := New(managerTestRoot(t, 1), nil)
	copyTestExe(t, filepath.Join(m.Root.RuntimeSDK(), "emulator", "qemu-img.exe"))
	t.Setenv(lifetimeEnv, "1")
	return m
}

func lifetimeWriteRequest(t *testing.T, dir string, req lifetimeRequest) string {
	t.Helper()
	f, err := os.CreateTemp(dir, "image with 空格-")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(req)
	if err == nil {
		_, err = f.Write(b)
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("request: %v / %v", err, closeErr)
	}
	return f.Name()
}

func lifetimeAwaitChild(t *testing.T, path string) syscall.Handle {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path + ".ready")
		if err == nil {
			pid, err := strconv.Atoi(string(b))
			if err != nil {
				t.Fatal(err)
			}
			h, err := syscall.OpenProcess(syscall.SYNCHRONIZE|syscall.PROCESS_TERMINATE|syscall.PROCESS_QUERY_INFORMATION, false, uint32(pid))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { syscall.TerminateProcess(h, 99); syscall.WaitForSingleObject(h, 5000); syscall.CloseHandle(h) })
			lifetimeAssertExclusive(t, path, false)
			t.Logf("child %d holds test file exclusively", pid)
			return h
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not acquire its test file and report ready")
	return 0
}

func lifetimeAssertExclusive(t *testing.T, path string, want bool) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path + ".held")
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err == nil {
		syscall.CloseHandle(h)
	}
	if (err == nil) != want {
		t.Fatalf("exclusive open=%v, want %v: %v", err == nil, want, err)
	}
	if !want && err != syscall.Errno(32) {
		t.Fatalf("expected sharing violation: %v", err)
	}
}

func lifetimeAssertExited(t *testing.T, h syscall.Handle, path string) {
	t.Helper()
	if event, err := syscall.WaitForSingleObject(h, 5000); err != nil || event != syscall.WAIT_OBJECT_0 {
		t.Fatalf("child survived parent/cancellation: event=%d err=%v", event, err)
	}
	lifetimeAssertExclusive(t, path, true)
}

func TestQemuImgLifetimeResults(t *testing.T) {
	m := lifetimeManager(t)
	t.Run("arguments", func(t *testing.T) {
		path := lifetimeWriteRequest(t, m.Root.Dir, lifetimeRequest{EchoArgs: true})
		args := []string{"convert", "a\"b", `trailing\`, "", "with 空格", path}
		so, _, code, err := m.runQemuImg(context.Background(), 15*time.Second, args...)
		var got []string
		if err != nil || code != 0 || json.Unmarshal(so, &got) != nil || !slices.Equal(got, args) {
			t.Fatalf("args=%q, want %q, code=%d error=%v", got, args, code, err)
		}
	})
	t.Run("already canceled", func(t *testing.T) {
		path := lifetimeWriteRequest(t, m.Root.Dir, lifetimeRequest{Hold: true})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, _, err := m.runQemuImg(ctx, time.Minute, "convert", path); errs.CodeOf(err) != errs.Canceled {
			t.Fatalf("run error=%v", err)
		}
		if _, _, err := m.runQemuImgCheck(ctx, path); errs.CodeOf(err) != errs.Canceled {
			t.Fatalf("check error=%v", err)
		}
		if _, err := os.Stat(path + ".ready"); !os.IsNotExist(err) {
			t.Fatalf("canceled command started a child: %v", err)
		}
	})
	for _, code := range []int{0, 1, 2, 3, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			out := strings.Repeat("stdout-空格\n", 10000)
			se := strings.Repeat("stderr\n", 10000)
			path := lifetimeWriteRequest(t, m.Root.Dir, lifetimeRequest{Code: code, Stdout: out, Stderr: se})
			so, stderr, got, err := m.runQemuImg(context.Background(), 15*time.Second, "convert", "a\"b", `trailing\`, "", path)
			if err != nil || got != code || string(so) != out || string(stderr) != se {
				t.Fatalf("run: code=%d err=%v output lengths=%d/%d", got, err, len(so), len(stderr))
			}
			so, failure, err := m.runQemuImgCheck(context.Background(), path)
			if err != nil {
				t.Fatal(err)
			}
			if code == 0 || code == 2 || code == 3 {
				if failure != "" || string(so) != out {
					t.Fatalf("check: failure=%q stdout length=%d", failure, len(so))
				}
			} else if so != nil || !strings.Contains(failure, fmt.Sprintf("退出码 %d", code)) || !strings.Contains(failure, "stderr") {
				t.Fatalf("check failure=%q", failure)
			}
		})
	}
	t.Run("launch failure", func(t *testing.T) {
		// Use a fresh, valid image. Holding it without sharing makes launch fail
		// without executing a bad image or overwriting a recently used image.
		m := lifetimeManager(t)
		exe, err := m.Root.QemuImgExe()
		if err != nil {
			t.Fatal(err)
		}
		path, err := syscall.UTF16PtrFromString(exe)
		if err != nil {
			t.Fatal(err)
		}
		h, err := syscall.CreateFile(path, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer syscall.CloseHandle(h)
		if _, _, _, err := m.runQemuImg(context.Background(), time.Second, "info"); errs.CodeOf(err) != errs.Internal {
			t.Fatalf("launch error=%v", err)
		}
		if _, _, err := m.runQemuImgCheck(context.Background(), "unused"); errs.CodeOf(err) != errs.Internal {
			t.Fatalf("check launch error=%v", err)
		}
	})
}

func TestQemuImgLifetimeCancelAndTimeout(t *testing.T) {
	m := lifetimeManager(t)
	for _, mode := range []string{"cancel convert", "cancel check", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			path := lifetimeWriteRequest(t, m.Root.Dir, lifetimeRequest{Hold: true})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if mode == "cancel check" {
					_, _, err := m.runQemuImgCheck(ctx, path)
					done <- err
					return
				}
				timeout := time.Minute
				if mode == "timeout" {
					timeout = 3 * time.Second
				}
				_, _, _, err := m.runQemuImg(ctx, timeout, "convert", path)
				done <- err
			}()
			h := lifetimeAwaitChild(t, path)
			want := errs.Timeout
			if mode != "timeout" {
				want = errs.Canceled
				cancel()
			}
			select {
			case err := <-done:
				if errs.CodeOf(err) != want {
					t.Fatalf("error=%v, want %v", err, want)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("run did not finish")
			}
			lifetimeAssertExited(t, h, path)
		})
	}
}

// Invoked only by the external controller below. It calls the production launch
// entry points, while the controller terminates this parent without tree-kill.
func TestQemuImgLifetimeParent(t *testing.T) {
	dir := os.Getenv("MODAVD_TEST_LIFETIME_ROOT")
	if dir == "" {
		t.Skip("subprocess helper requires MODAVD_TEST_LIFETIME_ROOT")
	}
	if gate := os.Getenv("MODAVD_TEST_LIFETIME_GATE"); gate != "" {
		deadline := time.Now().Add(15 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("parent was not admitted to outer job")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	path := os.Getenv("MODAVD_TEST_LIFETIME_REQUEST")
	mode := os.Getenv("MODAVD_TEST_LIFETIME_MODE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if strings.HasPrefix(mode, "cancel ") {
		go func() {
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := os.Stat(path + ".cancel"); err == nil {
						cancel()
						return
					}
				}
			}
		}()
	}
	timeout := time.Minute
	if mode == "timeout" {
		timeout = 3 * time.Second
	}
	if mode == "check" || mode == "cancel check" {
		_, _, err = m.runQemuImgCheck(ctx, path)
	} else {
		_, _, _, err = m.runQemuImg(ctx, timeout, "convert", path)
	}
	if strings.HasPrefix(mode, "cancel ") {
		if errs.CodeOf(err) != errs.Canceled {
			t.Fatalf("cancel error=%v", err)
		}
	} else if mode == "timeout" {
		if errs.CodeOf(err) != errs.Timeout {
			t.Fatalf("timeout error=%v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func TestQemuImgLifetimeParentExit(t *testing.T) {
	m := lifetimeManager(t)
	start := func(t *testing.T, path, mode string, outerJob syscall.Handle) (*exec.Cmd, <-chan error) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestQemuImgLifetimeParent$", "-test.count=1")
		cmd.Env = append(os.Environ(), "MODAVD_TEST_LIFETIME_ROOT="+m.Root.Dir, "MODAVD_TEST_LIFETIME_REQUEST="+path, "MODAVD_TEST_LIFETIME_MODE="+mode)
		gate := ""
		if outerJob != 0 {
			gate = path + ".parent-gate"
		}
		cmd.Env = append(cmd.Env, "MODAVD_TEST_LIFETIME_GATE="+gate)
		var output bytes.Buffer
		cmd.Stdout, cmd.Stderr = &output, &output
		win.HideConsoleCmd(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		finished := make(chan struct{})
		go func() {
			err := cmd.Wait()
			if err != nil {
				err = fmt.Errorf("parent: %w\n%s", err, output.String())
			}
			done <- err
			close(finished)
		}()
		t.Cleanup(func() {
			cmd.Process.Kill()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
			}
		})
		if outerJob != 0 {
			lifetimeAssignOuterJob(t, outerJob, uint32(cmd.Process.Pid))
			if err := os.WriteFile(gate, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return cmd, done
	}
	for _, mode := range []string{"convert", "check", "normal", "nested convert", "nested check", "nested normal", "nested cancel convert", "nested cancel check", "nested timeout"} {
		t.Run(mode, func(t *testing.T) {
			var outerJob syscall.Handle
			if strings.HasPrefix(mode, "nested ") {
				outerJob = lifetimeOuterJob(t)
				mode = strings.TrimPrefix(mode, "nested ")
			}
			path := lifetimeWriteRequest(t, m.Root.Dir, lifetimeRequest{Hold: true})
			other := lifetimeWriteRequest(t, m.Root.Dir, lifetimeRequest{Hold: true})
			parent, done := start(t, path, mode, outerJob)
			h := lifetimeAwaitChild(t, path)
			_, _ = start(t, other, "convert", outerJob)
			otherHandle := lifetimeAwaitChild(t, other)
			if outerJob != 0 {
				lifetimeAssertInJob(t, h, outerJob)
				lifetimeAssertInJob(t, otherHandle, outerJob)
			}
			if mode == "normal" {
				if err := os.WriteFile(path+".release", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if strings.HasPrefix(mode, "cancel ") {
				if err := os.WriteFile(path+".cancel", nil, 0o600); err != nil {
					t.Fatal(err)
				}
			} else if mode != "timeout" {
				if err := parent.Process.Kill(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				terminated := mode == "convert" || mode == "check"
				if !terminated && err != nil {
					t.Fatal(err)
				}
				if terminated && err == nil {
					t.Fatal("terminated parent reported success")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("parent did not exit")
			}
			lifetimeAssertExited(t, h, path)
			if event, err := syscall.WaitForSingleObject(otherHandle, 0); err != nil || event != syscall.WAIT_TIMEOUT {
				t.Fatalf("unrelated same-name child exited: event=%d err=%v", event, err)
			}
			lifetimeAssertExclusive(t, other, false)
			t.Log("target child exited, file reopened exclusively, same-name child in other tree remains alive")
		})
	}
}
