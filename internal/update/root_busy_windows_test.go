//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/testutil"
)

func TestRootBusyReadOnlyLoadedDLL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loaded.dll")
	free := loadDLL(t, path)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o666) })
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, access := range []uint32{syscall.GENERIC_WRITE, 0x00010000} {
		h, err := syscall.CreateFile(ptr, access, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, 0, 0)
		t.Logf("loaded read-only DLL access=%#x: %v", access, err)
		if err == nil {
			syscall.CloseHandle(h)
		}
	}
	if !fileInUse(path) {
		t.Error("loaded read-only DLL was not detected")
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("read-only attribute changed: %v, %v", st, err)
	}
	free()
	if fileInUse(path) {
		t.Error("unloaded read-only DLL is busy")
	}
}

func TestRootBusyHandleSharing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		share uint32
		busy  bool
	}{
		{"no sharing", 0, true},
		{"read and write only", syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE, true},
		{"all sharing", syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "file.txt")
			if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
				t.Fatal(err)
			}
			ptr, err := syscall.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal(err)
			}
			h, err := syscall.CreateFile(ptr, syscall.GENERIC_READ, tc.share, nil, syscall.OPEN_EXISTING, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got := fileInUse(path); got != tc.busy {
				t.Errorf("busy = %v, want %v", got, tc.busy)
			}
			if err := syscall.CloseHandle(h); err != nil {
				t.Fatal(err)
			}
			if fileInUse(path) {
				t.Error("closed handle is still busy")
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != "unchanged" {
				t.Fatalf("probe changed file: %q %v", got, err)
			}
		})
	}
	t.Run("os.Open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "open.txt")
		if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if !fileInUse(path) {
			t.Error("os.Open handle was not detected")
		}
	})
}

func TestRootBusyAccessDeniedIsNotUse(t *testing.T) {
	dir := t.TempDir()
	if fileInUse(dir) {
		t.Error("directory access denial counts as busy")
	}
	path := filepath.Join(dir, "readonly.txt")
	if err := os.WriteFile(path, []byte("unchanged"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o666) })
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		if err == nil {
			f.Close()
		}
		t.Fatalf("read-only fixture did not deny writing: %v", err)
	}
	if fileInUse(path) {
		t.Error("read-only file without a handle counts as busy")
	}
}

// Sharing allows the probes but denies a backup reader. If a busy file is known,
// reaching backupRootFiles would fail here before the updater could report Busy.
func rootBusyBackupGuard(t *testing.T, path string) func() {
	t.Helper()
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(ptr, syscall.GENERIC_READ, syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	open := true
	close := func() {
		if open {
			open = false
			syscall.CloseHandle(h)
		}
	}
	t.Cleanup(close)
	return close
}

func TestRootBusyInstallChecksBeforeBackup(t *testing.T) {
	const target = "extras/occupied.dll"
	for _, mode := range []string{"loaded DLL", "read-only loaded DLL", "no delete sharing", "deleted file"} {
		t.Run(mode, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			oldFiles := map[string]string{"0-backup.txt": "old backup", target: "old DLL"}
			newFiles := map[string]string{"0-backup.txt": "new backup", target: "new DLL"}
			if mode == "deleted file" {
				delete(newFiles, target)
			}
			r := writeRoot(t, 36, oldFiles)
			f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, newFiles), false))
			u := f.updater(r)
			u.scan = func(bool) ([]processUse, error) { return nil, nil }
			if _, err := u.Prepare(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := u.l.at(target)
			var release func()
			if mode == "no delete sharing" || mode == "deleted file" {
				opened, err := os.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				release = func() { opened.Close() }
				t.Cleanup(release)
			} else {
				release = loadDLL(t, path)
				if mode == "read-only loaded DLL" {
					if err := os.Chmod(path, 0o444); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { os.Chmod(path, 0o666) })
				}
			}
			oldDLL, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := os.ReadFile(u.l.at("mas-avd.json"))
			if err != nil {
				t.Fatal(err)
			}
			closeGuard := rootBusyBackupGuard(t, u.l.at("0-backup.txt"))
			if _, err := u.Install(context.Background()); errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), filepath.FromSlash(target)) {
				t.Fatalf("backup ran before the busy check, or occupant missed: %v", err)
			}
			if st := u.Status(); st.Phase != phaseStaged || st.PreparedVersion != "v0.37.0" {
				t.Fatalf("stage lost: %+v", st)
			}
			if err := u.l.checkStaged(u.l.readState()); err != nil {
				t.Fatalf("stage no longer valid: %v", err)
			}
			if lexists(u.l.previous()) {
				t.Error("busy install left a backup")
			}
			if got := read(t, r, "sdk/emulator/emulator.exe"); got != "emulator mas-36" {
				t.Fatalf("old SDK changed: %q", got)
			}
			if got, err := os.ReadFile(path); err != nil || string(got) != string(oldDLL) {
				t.Fatalf("occupied file changed: %v", err)
			}
			if got, err := os.ReadFile(u.l.at("mas-avd.json")); err != nil || string(got) != string(metadata) {
				t.Fatalf("metadata changed: %v", err)
			}
			closeGuard()
			release()
			if st, err := u.Install(context.Background()); err != nil || !st.Updated {
				t.Fatalf("retry: %+v %v", st, err)
			}
			if mode == "deleted file" {
				if lexists(path) {
					t.Error("obsolete file survived retry")
				}
			} else if got := read(t, r, target); got != "new DLL" {
				t.Fatalf("retry installed %q", got)
			}
		})
	}
}

func TestRootBusySelfThroughDirectoryLink(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"check", "apply", "restore"} {
		t.Run(action, func(t *testing.T) {
			dir := testutil.TempDir(t)
			real := filepath.Join(dir, "root")
			cli := filepath.Join(real, "tools", "modavd.exe")
			if err := copyFile(self, cli); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(dir, "alias")
			// The target exists, so this is a directory link on Windows too.
			if err := os.Symlink(real, alias); err != nil {
				t.Skipf("directory symlink unavailable: %v", err)
			}
			if st, err := os.Stat(filepath.Join(alias, "tools")); err != nil || !st.IsDir() {
				t.Fatalf("invalid directory symlink: %v", err)
			}
			cmd := exec.Command(cli, "-test.run=^TestRootBusySelfAliasChild$", "-test.v")
			cmd.Env = append(os.Environ(), "MODAVD_ROOT_BUSY_SELF="+alias, "MODAVD_ROOT_BUSY_ACTION="+action)
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("self %s through link: %v\n%s", action, err, out)
			}
		})
	}
}

func TestRootBusySelfAliasChild(t *testing.T) {
	dir := os.Getenv("MODAVD_ROOT_BUSY_SELF")
	if dir == "" {
		t.Skip("self-update fixture only")
	}
	l := layout{root: dir}
	const rel = "tools/modavd.exe"
	plan := rootPlan{replace: []string{rel}}
	switch action := os.Getenv("MODAVD_ROOT_BUSY_ACTION"); action {
	case "check":
		if busy := l.rootFileInUse(plan); busy != "" {
			t.Fatalf("self treated as occupant: %s", busy)
		}
	case "apply", "restore":
		source := l.pkgAt(rel)
		if action == "restore" {
			source = l.prevAt(rel)
		}
		if err := os.MkdirAll(filepath.Dir(source), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(source, []byte(action), 0o600); err != nil {
			t.Fatal(err)
		}
		var err error
		if action == "apply" {
			err = l.applyRootFiles(plan)
		} else {
			err = l.restoreRootFiles(plan)
		}
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		if got, err := os.ReadFile(l.at(rel)); err != nil || string(got) != action {
			t.Fatalf("wrong replacement: %q %v", got, err)
		}
	default:
		t.Fatalf("unknown action: %s", action)
	}
}

func TestRootBusyDeniedACLIsNotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "denied.txt")
	if err := os.WriteFile(path, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("icacls.exe", append([]string{path}, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("icacls: %v\n%s", err, out)
		}
	}
	run("/deny", "*S-1-1-0:(WD,AD,DE)")
	t.Cleanup(func() { run("/remove:d", "*S-1-1-0") })
	if f, err := os.OpenFile(path, os.O_WRONLY, 0); !errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		if err == nil {
			f.Close()
		}
		t.Fatalf("ACL fixture did not deny writing: %v", err)
	}
	if fileInUse(path) {
		t.Error("ACL access denial counts as occupied")
	}
}

func TestRootBusyDeleteSharedReadOnlyFile(t *testing.T) {
	const obsolete = "extras/obsolete.txt"
	withSettings(t, SourceGitHub, "")
	r := writeRoot(t, 36, map[string]string{obsolete: "obsolete"})
	f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
	u := f.updater(r)
	u.scan = func(bool) ([]processUse, error) { return nil, nil }
	path := u.l.at(obsolete)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o666) })
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(ptr, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if st, err := u.Install(context.Background()); err != nil || !st.Updated {
		t.Fatalf("shared reader blocked obsolete file deletion: %+v %v", st, err)
	}
	if lexists(path) {
		t.Error("obsolete file was not removed")
	}
}

func TestRootBusyExternalReadOnlyDLL(t *testing.T) {
	for _, deep := range []bool{false, true} {
		t.Run(fmt.Sprint("deep=", deep), func(t *testing.T) {
			dir := testutil.TempDir(t)
			dll := filepath.Join(dir, "external.dll")
			if deep {
				dll = filepath.Join(dir, strings.Repeat("nested-", 14), strings.Repeat("nested-", 14), "external.dll")
			}
			free := loadDLL(t, dll)
			free()
			if err := os.Chmod(dll, 0o444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dll, 0o666) })
			ready := filepath.Join(dir, "ready")
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, "-test.run=^TestRootBusyDLLChild$")
			cmd.Env = append(os.Environ(), "MODAVD_ROOT_BUSY_DLL="+dll, "MODAVD_ROOT_BUSY_READY="+ready)
			cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				input.Close()
				if !waited {
					cancel()
					cmd.Wait()
				}
			})
			deadline := time.Now().Add(5 * time.Second)
			for !lexists(ready) {
				if time.Now().After(deadline) {
					t.Fatal("DLL child never became ready")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// Query through a valid directory link too, so mapped image and
			// target filenames must resolve to the same actual file.
			alias := filepath.Join(dir, "alias")
			probe := dll
			if err := os.Symlink(filepath.Dir(dll), alias); err == nil {
				probe = filepath.Join(alias, filepath.Base(dll))
			}
			if !fileInUse(probe) {
				t.Error("another process's read-only DLL was missed")
			}
			input.Close()
			err = cmd.Wait()
			waited = true
			if err != nil {
				t.Fatalf("DLL child: %v\n%s", err, output.String())
			}
			if fileInUse(probe) {
				t.Error("exited DLL child still blocks the file")
			}
		})
	}
}

func TestRootBusyDLLChild(t *testing.T) {
	path := os.Getenv("MODAVD_ROOT_BUSY_DLL")
	if path == "" {
		t.Skip("DLL occupancy fixture only")
	}
	h, err := syscall.LoadLibrary(path)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.FreeLibrary(h)
	if err := os.WriteFile(os.Getenv("MODAVD_ROOT_BUSY_READY"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var release [1]byte
	os.Stdin.Read(release[:])
}
