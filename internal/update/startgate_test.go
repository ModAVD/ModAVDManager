//go:build windows

package update

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/avd"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

// A real Windows image occupying the SDK, without emulator, guest or port access.
func TestUpdateOccupancyChild(t *testing.T) {
	ready := os.Getenv("MODAVD_UPDATE_TEST_READY")
	if ready == "" {
		t.Skip("occupancy fixture only")
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Second)
}

func occupancyChild(t *testing.T, image string) *exec.Cmd {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(image, "-test.run=^TestUpdateOccupancyChild$")
	cmd.Env = append(os.Environ(), "MODAVD_UPDATE_TEST_READY="+ready)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatal("occupancy child did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Removing a Windows fixture tree can transiently fail after its child exits.
// Retry only transient errors before TempDir's own final cleanup.
func cleanupOccupancyRoot(t *testing.T, dir string) {
	t.Helper()
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			transient := errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32)) || errors.Is(err, syscall.Errno(145))
			if !transient || time.Now().After(deadline) {
				t.Errorf("cleanup occupancy fixture: %v", err)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	})
}

func startLockPath(r *root.Root, i int) string {
	return filepath.Join(r.AvdHome(), "."+root.AvdName(i)+".start.lock")
}

func assertStartLocks(t *testing.T, r *root.Root, held bool) {
	t.Helper()
	for i := 0; i <= root.MaxIndex; i++ {
		lock, free, err := win.TryLock(startLockPath(r, i))
		lock.Unlock()
		wantFree := !held || !root.ValidIndex(i)
		if err != nil || free != wantFree {
			t.Fatalf("start lock %d: free=%v err=%v, held=%v", i, free, err, held)
		}
	}
}

// The first native scan finishes before a managed start acquires its lock and
// launches QEMU. Windows permits renaming its directory while the image runs.
func TestInstallRejectsStartAfterOccupancyCheck(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	cleanupOccupancyRoot(t, r.Dir)
	image := filepath.Join(r.Dir, "sdk/emulator/qemu/windows-x86_64/qemu-system-x86_64.exe")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyFile(self, image); err != nil {
		t.Fatal(err)
	}
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	var child *exec.Cmd
	scans := 0
	var scan func(bool) ([]processUse, error)
	scan = func(programs bool) ([]processUse, error) {
		u.scan = nil
		uses, err := u.processesUsing(programs)
		u.scan = scan
		if err != nil {
			return nil, err
		}
		scans++
		if scans == 1 {
			if len(uses) != 0 {
				t.Fatalf("initial occupants: %+v", uses)
			}
			lock, free, err := win.TryLock(startLockPath(r, 6))
			if err != nil || !free {
				t.Fatalf("concurrent start lock: %v %v", free, err)
			}
			defer lock.Unlock()
			child = occupancyChild(t, image)
		}
		return uses, nil
	}
	u.scan = scan
	st, err := u.Install(context.Background())
	if errs.CodeOf(err) != errs.Busy || st.Updated || st.Phase != phaseStaged || scans < 1 {
		t.Fatalf("new occupant did not stop the swap: scans=%d status=%+v err=%v", scans, st, err)
	}
	p, found := win.FindProcess(uint32(child.Process.Pid))
	if !found || !p.Alive() || !strings.Contains(err.Error(), "qemu-system-x86_64.exe") || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-36" {
		t.Fatalf("old package or running child changed: found=%v err=%v", found, err)
	}
	assertStartLocks(t, r, false)
}

func TestInstallHoldsAllStartLocksThroughCommit(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	if _, err := avd.Create(r, 0, avd.CreateOptions{MemoryMB: 3072, CPU: 2, DataPartitionGB: 64, FPS: 60}); err != nil {
		t.Fatal(err)
	}
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	// Hold the final metadata commit after the SDK has moved. Start must remain
	// excluded through this point, not just while the last process scan runs.
	metaLock, free, err := win.TryLock(filepath.Join(r.Dir, "mas-avd.json.lock"))
	if err != nil || !free {
		t.Fatalf("metadata lock: %v %v", free, err)
	}
	defer metaLock.Unlock()
	var installed *Status
	var installErr error
	done := make(chan struct{})
	go func() {
		installed, installErr = u.Install(context.Background())
		close(done)
	}()
	t.Cleanup(func() {
		metaLock.Unlock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("install did not finish after releasing metadata lock")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-37" {
		if time.Now().After(deadline) {
			t.Fatal("install never reached metadata commit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertStartLocks(t, r, true)
	// Exercise the real Manager.Start lock protocol. Cancellation must happen
	// while waiting for update, before any emulator precheck or launch can run.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := emu.New(r, nil).Start(ctx, 0, emu.StartOptions{NoBridge: true}); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("Manager.Start did not wait behind update: %v", err)
	}
	metaLock.Unlock()
	select {
	case <-done:
		if installErr != nil || !installed.Updated {
			t.Fatalf("install: %+v %v", installed, installErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("install did not finish")
	}
	assertStartLocks(t, r, false)
}

func TestInstallStartGateFailureReleasesLocks(t *testing.T) {
	for _, fail := range []string{"busy", "unreadable", "final scan error"} {
		t.Run(fail, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			r := writeRoot(t, 36, nil)
			f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
			u := f.updater(r)
			want := errs.Internal
			switch fail {
			case "busy":
				lock, free, err := win.TryLock(startLockPath(r, 9))
				if err != nil || !free {
					t.Fatalf("start lock: %v %v", free, err)
				}
				defer lock.Unlock()
				want = errs.Busy
			case "unreadable":
				if err := os.Mkdir(startLockPath(r, 9), 0o755); err != nil {
					t.Fatal(err)
				}
			case "final scan error":
				scans := 0
				u.scan = func(bool) ([]processUse, error) {
					scans++
					if scans == 2 {
						return nil, errors.New("scan unavailable")
					}
					return nil, nil
				}
			}
			st, err := u.Install(context.Background())
			if errs.CodeOf(err) != want || st.Updated || st.Phase != phaseStaged || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-36" {
				t.Fatalf("gate failure: %+v %v", st, err)
			}
			// Earlier slots must be released even when the last slot fails.
			last := root.MaxIndex
			if fail != "final scan error" {
				last-- // this slot is still held or unreadable in the fixture
			}
			for i := 0; i <= last; i++ {
				lock, free, err := win.TryLock(startLockPath(r, i))
				lock.Unlock()
				if err != nil || !free {
					t.Fatalf("leaked start lock %d: %v %v", i, free, err)
				}
			}
		})
	}
}

func TestInstallStartGateIsRootScoped(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	other := writeRoot(t, 36, nil)
	lock, free, err := win.TryLock(startLockPath(other, 0))
	if err != nil || !free {
		t.Fatalf("other root start lock: %v %v", free, err)
	}
	defer lock.Unlock()
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	if st, err := f.updater(r).Install(context.Background()); err != nil || !st.Updated {
		t.Fatalf("another root blocked install: %+v %v", st, err)
	}
	probe, free, err := win.TryLock(startLockPath(other, 0))
	probe.Unlock()
	if err != nil || free || read(t, other, "sdk/emulator/emulator.exe") != "emulator mas-36" {
		t.Fatalf("another root's lock or SDK changed: free=%v err=%v", free, err)
	}
	assertStartLocks(t, r, false)
}

func TestRecoveryUsesStartGateWithoutRelocking(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	u.cutSwap(t, 1)
	lock, free, err := win.TryLock(startLockPath(r, 0))
	if err != nil || !free {
		t.Fatalf("start lock: %v %v", free, err)
	}
	defer lock.Unlock()
	if _, err := u.Recover(context.Background()); errs.CodeOf(err) != errs.Busy || phaseOf(u.l.readState()) != phaseSwapping || isDir(u.l.emulator()) {
		t.Fatalf("recovery did not preserve interrupted swap: %v", err)
	}
	lock.Unlock()
	unlock, err := u.lockForSwap("装")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = u.recoverAfter(errors.New("injected swap failure"))
	if errs.CodeOf(err) != errs.Internal || strings.Contains(err.Error(), "当场恢复也失败") || phaseOf(u.l.readState()) != phaseIdle || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-37" {
		t.Fatalf("immediate recovery relocked or failed: %v", err)
	}
	assertStartLocks(t, r, true)
	unlock()
	assertStartLocks(t, r, false)
}

func TestRecoveryChecksPreviousSDKOccupants(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	cleanupOccupancyRoot(t, r.Dir)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const rel = "qemu/windows-x86_64/qemu-system-x86_64.exe"
	if err := copyFile(self, filepath.Join(r.Dir, "sdk/emulator", rel)); err != nil {
		t.Fatal(err)
	}
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	u.cutSwap(t, 1)
	child := occupancyChild(t, filepath.Join(u.l.previousEmulator(), rel))
	u.scan = nil
	if _, err := u.Recover(context.Background()); errs.CodeOf(err) != errs.Busy || phaseOf(u.l.readState()) != phaseSwapping || isDir(u.l.emulator()) {
		t.Fatalf("recovery ignored previous SDK occupant: %v", err)
	}
	child.Process.Kill()
	child.Wait()
	if st, err := u.Recover(context.Background()); err != nil || st.Phase != phaseIdle || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-37" {
		t.Fatalf("recovery after exit: %+v %v", st, err)
	}
	assertStartLocks(t, r, false)
}
