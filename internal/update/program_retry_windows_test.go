//go:build windows

package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"modavd/internal/errs"
)

func TestScanRetriesShortRootPrograms(t *testing.T) {
	program := processUse{PID: 111, Name: "modavd.exe", RootProgram: true}
	helper := processUse{PID: 222, Name: "modavd.exe", RootProgram: true, ShutdownHelper: true}
	qemu := processUse{PID: 333, Name: "qemu-system-x86_64.exe"}
	unknown := processUse{PID: 444, Name: "emulator.exe", Unknown: true, RootProgram: true}
	for _, c := range []struct {
		name      string
		states    [][]processUse
		wantScans int
		wantPID   uint32
		errScan   int
	}{
		{name: "unused", states: [][]processUse{nil}, wantScans: 1},
		{name: "program exits after one retry", states: [][]processUse{{program}, nil}, wantScans: 2},
		{name: "program exits after two retries", states: [][]processUse{{program}, {program}, nil}, wantScans: 3},
		{name: "long program stays busy", states: [][]processUse{{program}}, wantScans: 3, wantPID: 111},
		{name: "new program shares retry budget", states: [][]processUse{{program}, {{PID: 555, RootProgram: true}}}, wantScans: 3, wantPID: 555},
		{name: "helper keeps its existing wait", states: [][]processUse{{helper}}, wantScans: 1, wantPID: 222},
		{name: "program exits beside helper", states: [][]processUse{{program, helper}, {helper}}, wantScans: 2, wantPID: 222},
		{name: "ordinary SDK use blocks immediately", states: [][]processUse{{program, qemu}}, wantScans: 1, wantPID: 111},
		{name: "unknown blocks immediately", states: [][]processUse{{unknown}}, wantScans: 1, wantPID: 444},
		{name: "new SDK occupant blocks next scan", states: [][]processUse{{program}, {qemu}}, wantScans: 2, wantPID: 333},
		{name: "initial scan error", states: [][]processUse{nil}, errScan: 1, wantScans: 1},
		{name: "retry scan error", states: [][]processUse{{program}}, errScan: 2, wantScans: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			scans, sleeps := 0, 0
			injected := errors.New("scan failed")
			uses, err := scanWithProgramRetries(true, func(programs bool) ([]processUse, error) {
				if !programs {
					t.Fatal("retry omitted root programs")
				}
				scans++
				if scans == c.errScan {
					return nil, injected
				}
				return c.states[min(scans-1, len(c.states)-1)], nil
			}, func(d time.Duration) {
				if d != 250*time.Millisecond {
					t.Fatalf("retry delay = %v", d)
				}
				sleeps++
			})
			if scans != c.wantScans || sleeps != c.wantScans-1 || (c.errScan != 0) != errors.Is(err, injected) {
				t.Fatalf("scans=%d sleeps=%d err=%v, want %d scans", scans, sleeps, err, c.wantScans)
			}
			if c.errScan != 0 {
				return
			}
			if c.wantPID == 0 && len(uses) != 0 || c.wantPID != 0 && (len(uses) == 0 || uses[0].PID != c.wantPID) {
				t.Fatalf("remaining uses = %+v, want PID %d", uses, c.wantPID)
			}
		})
	}
}

func TestInstallRetriesRootProgramAtFinalScan(t *testing.T) {
	for _, stays := range []bool{false, true} {
		t.Run(map[bool]string{false: "short program exits", true: "long program stays busy"}[stays], func(t *testing.T) {
			u, image := imageUpdate(t)
			cli := u.l.at("tools/modavd.exe")
			if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cli, image, 0o600); err != nil {
				t.Fatal(err)
			}
			var stop func()
			scans := 0
			u.scan = func(programs bool) ([]processUse, error) {
				scans++
				if scans == 1 {
					return nil, nil
				}
				assertStartLocks(t, u.Root, true)
				assertImageBlocked(t, u.Root.EmulatorExe())
				if scans == 2 {
					child := occupancyChild(t, cli)
					stop = func() { child.Process.Kill(); child.Wait() }
				} else if !stays {
					stop()
				}
				uses, err := ownScan(u)(programs)
				if (scans == 2 || stays) && (len(uses) != 1 || !uses[0].RootProgram || uses[0].Unknown || uses[0].ShutdownHelper) {
					t.Fatalf("root program was not identified: %+v, %v", uses, err)
				}
				return uses, err
			}
			st, err := u.Install(context.Background())
			if stays {
				if errs.CodeOf(err) != errs.Busy || st.Updated || st.Phase != phaseStaged || scans != 4 {
					t.Fatalf("long program allowed swap: scans=%d status=%+v err=%v", scans, st, err)
				}
			} else if err != nil || !st.Updated || st.Phase != phaseIdle || scans != 3 {
				t.Fatalf("install after short program: scans=%d status=%+v err=%v", scans, st, err)
			}
			assertStartLocks(t, u.Root, false)
		})
	}
}

func TestSDKScanDoesNotRetryRootPrograms(t *testing.T) {
	scans := 0
	uses, err := scanWithProgramRetries(false, func(programs bool) ([]processUse, error) {
		if programs {
			t.Fatal("SDK-only scan unexpectedly included programs")
		}
		scans++
		return []processUse{{PID: 111, RootProgram: true}}, nil
	}, func(time.Duration) { t.Fatal("SDK-only scan slept") })
	if err != nil || scans != 1 || len(uses) != 1 {
		t.Fatalf("SDK-only scan: scans=%d uses=%+v err=%v", scans, uses, err)
	}
}

func TestNextUpdateCleansExitedProgramTrash(t *testing.T) {
	for _, action := range []string{"prepare", "install"} {
		t.Run(action, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			r := writeRoot(t, 36, nil)
			cleanupOccupancyRoot(t, r.Dir)
			self, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cli := filepath.Join(r.Dir, "tools/modavd.exe")
			if err := copyFile(self, cli); err != nil {
				t.Fatal(err)
			}
			child := occupancyChild(t, cli)
			f := newSources(t, 36, zipOf(t, 36, pkgFiles(36, nil), false))
			u := f.updater(r)
			if err := u.l.toTrash(cli); err != nil {
				t.Fatal(err)
			}
			u.l.purgeTrash()
			entries, err := os.ReadDir(u.l.trash())
			if err != nil || len(entries) != 1 {
				t.Fatalf("running image did not remain in trash: %v, %v", entries, err)
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = child.Wait()
			_ = u.Status()
			if !lexists(u.l.trash()) {
				t.Fatal("read-only status unexpectedly cleaned trash")
			}
			var st *Status
			if action == "prepare" {
				st, err = u.Prepare(context.Background())
			} else {
				st, err = u.Install(context.Background())
			}
			if err != nil || st.Updated || st.Phase != phaseIdle || lexists(u.l.trash()) {
				t.Fatalf("next %s did not clean trash: status=%+v err=%v", action, st, err)
			}
		})
	}
}
