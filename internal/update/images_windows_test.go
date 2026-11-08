//go:build windows

package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

func imageUpdate(t *testing.T) (*Updater, []byte) {
	t.Helper()
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, nil)
	cleanupOccupancyRoot(t, r.Dir)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.EmulatorExe(), image, 0o600); err != nil {
		t.Fatal(err)
	}
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, map[string]string{"sdk/emulator/emulator.exe": string(image)}), false))
	return f.updater(r), image
}

// Use os.StartProcess directly so an image with a non-.exe name is tested too.
func startImageChild(t *testing.T, path string) (*os.Process, string, error) {
	t.Helper()
	ready := filepath.Join(t.TempDir(), "ready")
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	p, err := os.StartProcess(path, []string{path, "-test.run=^TestUpdateOccupancyChild$"}, &os.ProcAttr{
		Env:   append(os.Environ(), "MODAVD_UPDATE_TEST_READY="+ready),
		Files: []*os.File{null, null, null},
		Sys:   &syscall.SysProcAttr{HideWindow: true},
	})
	if err == nil {
		t.Cleanup(func() { p.Kill(); p.Wait() })
	}
	return p, ready, err
}

func assertImageBlocked(t *testing.T, path string) {
	t.Helper()
	_, _, err := startImageChild(t, path)
	if !errors.Is(err, syscall.Errno(32)) {
		t.Fatalf("image start was not blocked by sharing: %s: %v", path, err)
	}
}

func assertImageStarts(t *testing.T, path string) *os.Process {
	t.Helper()
	p, ready, err := startImageChild(t, path)
	if err != nil {
		t.Fatalf("image did not start: %s: %v", path, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			return p
		}
		if time.Now().After(deadline) {
			t.Fatal("image child never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInstallBlocksRawAndSharedSDKStarts(t *testing.T) {
	for _, mode := range []string{"direct", "shared SDK", "hardlink", "renamed PE"} {
		t.Run(mode, func(t *testing.T) {
			u, image := imageUpdate(t)
			r := u.Root
			path := r.EmulatorExe()
			switch mode {
			case "shared SDK":
				other, err := root.New(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				meta := `{"sdkRoot": "` + filepath.ToSlash(filepath.Join(r.Dir, "sdk")) + `"}`
				if err := os.WriteFile(filepath.Join(other.Dir, "mas-avd.json"), []byte(meta), 0o600); err != nil {
					t.Fatal(err)
				}
				path = other.EmulatorExe()
				lock, free, err := win.TryLock(startLockPath(other, 0))
				if err != nil || !free {
					t.Fatalf("other root start lock: %v %v", free, err)
				}
				defer lock.Unlock()
			case "hardlink":
				path = filepath.Join(u.l.emulator(), "alias.exe")
				if err := os.Link(r.EmulatorExe(), path); err != nil {
					t.Fatal(err)
				}
			case "renamed PE":
				path = filepath.Join(u.l.emulator(), "native-image.bin")
				if err := os.WriteFile(path, image, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			// Establish a positive control, including for the alternate filename.
			p := assertImageStarts(t, path)
			p.Kill()
			p.Wait()
			scans := 0
			u.scan = func(programs bool) ([]processUse, error) {
				if !programs {
					t.Fatal("manager scan omitted")
				}
				scans++
				if scans == 2 {
					assertImageBlocked(t, path)
					assertImageBlocked(t, filepath.Join(u.l.stageEmulator(), "emulator.exe"))
				}
				return nil, nil
			}
			st, err := u.Install(context.Background())
			if err != nil || !st.Updated || scans < 2 {
				t.Fatalf("guarded install failed: %+v %v", st, err)
			}
			assertImageStarts(t, r.EmulatorExe())
		})
	}
}

func TestImageGatesSurviveDirectorySwapAndRecovery(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		t.Run(map[bool]string{false: "install", true: "recover"}[recovering], func(t *testing.T) {
			u, _ := imageUpdate(t)
			if _, err := u.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(filepath.Join(u.l.stageEmulator(), "emulator.exe"))
			if err != nil {
				t.Fatal(err)
			}
			if recovering {
				u.cutSwap(t, 1)
			}
			metaLock, free, err := win.TryLock(filepath.Join(u.Root.Dir, "mas-avd.json.lock"))
			if err != nil || !free {
				t.Fatalf("metadata lock: %v %v", free, err)
			}
			defer metaLock.Unlock()
			var result *Status
			var updateErr error
			done := make(chan struct{})
			go func() {
				if recovering {
					result, updateErr = u.Recover(context.Background())
				} else {
					result, updateErr = u.Install(context.Background())
				}
				close(done)
			}()
			t.Cleanup(func() {
				metaLock.Unlock()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("update worker did not finish")
				}
			})
			deadline := time.Now().Add(5 * time.Second)
			for !isDir(u.l.emulator()) || !isDir(u.l.previousEmulator()) {
				select {
				case <-done:
					t.Fatalf("update ended before metadata commit: %+v %v", result, updateErr)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("SDK directory swap did not complete")
				}
				time.Sleep(10 * time.Millisecond)
			}
			assertImageBlocked(t, u.Root.EmulatorExe())
			assertImageBlocked(t, filepath.Join(u.l.previousEmulator(), "emulator.exe"))
			metaLock.Unlock()
			select {
			case <-done:
				if updateErr != nil || result.Phase != phaseIdle {
					t.Fatalf("update: %+v %v", result, updateErr)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("update did not finish")
			}
			after, err := os.Stat(u.Root.EmulatorExe())
			if err != nil || !before.ModTime().Equal(after.ModTime()) {
				t.Fatalf("image timestamp changed without writes: %v", err)
			}
			assertImageStarts(t, u.Root.EmulatorExe())
		})
	}
}

func TestImageGateFailureReleasesEarlierImages(t *testing.T) {
	for _, mode := range []string{"running", "readonly", "reparse"} {
		t.Run(mode, func(t *testing.T) {
			u, image := imageUpdate(t)
			path := filepath.Join(u.l.emulator(), "qemu/windows-x86_64/qemu-system-x86_64.exe")
			if err := os.WriteFile(path, image, 0o600); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "running":
				assertImageStarts(t, path)
			case "readonly":
				if err := os.Chmod(path, 0o400); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Chmod(path, 0o600) })
			case "reparse":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(u.Root.EmulatorExe(), path); err != nil {
					t.Skipf("symlink creation unavailable: %v", err)
				}
			}
			st, err := u.Install(context.Background())
			if errs.CodeOf(err) != errs.Busy || st.Updated || st.Phase != phaseStaged || !isDir(u.l.emulator()) {
				t.Fatalf("image guard failed open: %+v %v", st, err)
			}
			if mode == "readonly" {
				f, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				info, err := f.Stat()
				f.Close()
				if err != nil || info.Mode().Perm()&0o200 != 0 {
					t.Fatalf("readonly attribute changed after guard failure: %v", err)
				}
			}
			assertStartLocks(t, u.Root, false)
			assertImageStarts(t, u.Root.EmulatorExe())
		})
	}
}

func TestImageGateRemainsHeldDuringImmediateRecovery(t *testing.T) {
	u, _ := imageUpdate(t)
	if _, err := u.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	unlock, err := u.lockForSwap("装")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	u.cutSwap(t, 1)
	err = u.recoverAfter(errors.New("injected failure"))
	if errs.CodeOf(err) != errs.Internal || phaseOf(u.l.readState()) != phaseIdle {
		t.Fatalf("immediate recovery failed: %v", err)
	}
	assertImageBlocked(t, u.Root.EmulatorExe())
	unlock()
	assertImageStarts(t, u.Root.EmulatorExe())
}

// Process pathname ownership cannot recognize a loaded image reached through an
// outside hardlink. The image-object gate must still refuse to replace its SDK.
func TestRunningSDKHardlinkOutsideRootBlocksUpdate(t *testing.T) {
	u, _ := imageUpdate(t)
	aliasDir := t.TempDir()
	cleanupOccupancyRoot(t, aliasDir)
	alias := filepath.Join(aliasDir, "external-image.exe")
	if err := os.Link(u.Root.EmulatorExe(), alias); err != nil {
		t.Fatal(err)
	}
	p := assertImageStarts(t, alias)
	u.scan = ownScan(u)
	st, err := u.Install(context.Background())
	if errs.CodeOf(err) != errs.Busy || st.Updated || st.Phase != phaseStaged {
		t.Fatalf("outside hardlink did not block the swap: %+v %v", st, err)
	}
	proc, found := win.FindProcess(uint32(p.Pid))
	if !found || !proc.Alive() {
		t.Fatal("update changed the running outside process")
	}
	assertStartLocks(t, u.Root, false)
	p.Kill()
	p.Wait()
	if st, err = u.Install(context.Background()); err != nil || !st.Updated {
		t.Fatalf("install after outside process exit: %+v %v", st, err)
	}
	assertImageStarts(t, u.Root.EmulatorExe())
}
