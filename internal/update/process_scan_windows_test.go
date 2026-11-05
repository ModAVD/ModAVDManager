//go:build windows

package update

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/win"
)

func TestInstallAfterProcessSnapshotExit(t *testing.T) {
	for _, c := range []struct {
		name           string
		afterImage     bool
		unknownCreated bool
	}{
		{name: "before image read"},
		{name: "after image read", afterImage: true},
		{name: "before image read without creation time", unknownCreated: true},
		{name: "after image read without creation time", afterImage: true, unknownCreated: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			r := writeRoot(t, 36, nil)
			cleanupOccupancyRoot(t, r.Dir)
			image := filepath.Join(r.Dir, "sdk/emulator/emulator.exe")
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			if err := copyFile(self, image); err != nil {
				t.Fatal(err)
			}
			child := occupancyChild(t, image)
			p, found := win.FindProcess(uint32(child.Process.Pid))
			if !found || p.Created == 0 || !p.Alive() {
				t.Fatalf("missing live fixture identity: %+v, found=%v", p, found)
			}
			if c.unknownCreated {
				p.Created = 0
			}
			f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
			u := f.updater(r)
			scans, reads := 0, 0
			u.scan = func(programs bool) ([]processUse, error) {
				scans++
				if scans == 1 {
					// Enter the helper wait, then exercise the native stale-snapshot
					// scan when the next iteration observes the helper exiting.
					return []processUse{{PID: p.PID, Name: p.Name, ShutdownHelper: true}}, nil
				}
				if scans == 3 {
					assertStartLocks(t, r, true)
				}
				return u.scanProcesses(programs, func() ([]win.Process, error) {
					// Keep the stale entry for the final scan under the SDK locks too.
					return []win.Process{p}, nil
				}, func(snapshot win.Process) (string, error) {
					reads++
					if reads > 1 {
						return snapshot.ImagePath()
					}
					path, err := snapshot.ImagePath()
					if err != nil {
						t.Fatalf("fixture path before exit: %v", err)
					}
					if err := child.Process.Kill(); err != nil {
						t.Fatal(err)
					}
					_ = child.Wait()
					if snapshot.Alive() {
						t.Fatal("fixture did not exit")
					}
					if !c.afterImage {
						path, err = snapshot.ImagePath()
						if err == nil {
							t.Fatal("expected native image-path failure after exit")
						}
					} else if _, err := snapshot.CommandLine(); err == nil {
						t.Fatal("expected native command-line failure after exit")
					}
					return path, err
				})
			}
			st, err := u.Install(context.Background())
			if err != nil || !st.Updated || st.Phase != phaseIdle || scans != 3 || reads != 2 ||
				read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-37" {
				t.Fatalf("install after snapshot exit: scans=%d reads=%d status=%+v err=%v", scans, reads, st, err)
			}
			assertStartLocks(t, r, false)
		})
	}
}

func TestProcessScanUnreadableImageIdentity(t *testing.T) {
	u, _ := imageUpdate(t)
	child := occupancyChild(t, u.Root.EmulatorExe())
	live, found := win.FindProcess(uint32(child.Process.Pid))
	if !found || live.Created == 0 || !live.Alive() {
		t.Fatalf("missing live fixture identity: %+v, found=%v", live, found)
	}
	for _, c := range []struct {
		name, imageName string
		created         uint64
		wantBusy        bool
	}{
		{"live emulator", "emulator.exe", live.Created, true},
		{"live qemu", "qemu-system-x86_64.exe", live.Created, true},
		{"live netsimd", "netsimd.exe", live.Created, true},
		{"unreadable creation time", "emulator.exe", 0, true},
		{"PID reused", "emulator.exe", live.Created + 1, false},
		{"unrelated protected process", "other.exe", live.Created, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := live
			p.Name, p.Created = c.imageName, c.created
			err := waitUntilUnused("装", func(programs bool) ([]processUse, error) {
				uses, err := u.scanProcesses(programs, func() ([]win.Process, error) {
					return []win.Process{p}, nil
				}, func(win.Process) (string, error) { return "", syscall.ERROR_ACCESS_DENIED })
				if c.wantBusy && (len(uses) != 1 || !uses[0].Unknown || uses[0].ShutdownHelper) {
					t.Fatalf("live unreadable process lost Unknown: %+v, %v", uses, err)
				}
				return uses, err
			}, time.Now, func(time.Duration) { t.Fatal("unknown process must block without waiting") })
			if got := errs.CodeOf(err); c.wantBusy && got != errs.Busy || !c.wantBusy && err != nil {
				t.Fatalf("wantBusy=%v, err=%v", c.wantBusy, err)
			}
			if !live.Alive() {
				t.Fatal("scan changed the live fixture")
			}
		})
	}
}
