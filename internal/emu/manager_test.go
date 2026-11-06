//go:build windows

package emu

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// managerTestRoot is a fresh root with the files avd.List needs for each of indexes (mas_<i>.ini and
// mas_<i>.avd\config.ini). Its private port is an unused one; nothing here runs adb.
func managerTestRoot(t *testing.T, indexes ...int) *root.Root {
	t.Helper()
	dir := opsTestTempDir(t)
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), []byte(`{"adbServerPort": 20391, "scriptAdbServerPort": 20392}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range indexes {
		avdDir := filepath.Join(r.AvdHome(), root.AvdName(i)+".avd")
		if err := os.MkdirAll(avdDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(r.AvdHome(), root.AvdName(i)+".ini"), []byte("path="+avdDir+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(avdDir, "config.ini"), []byte("hw.cpu.ncore=4\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestQemuScanHookAndStopTargets(t *testing.T) {
	m := New(managerTestRoot(t, 2, 3, 4), nil)
	fake := win.Process{PID: 4242, Name: "qemu-system-x86_64-headless.exe"}
	saved := qemuScan
	var asked [][]int
	qemuScan = func(indexes []int) map[int]win.Process {
		asked = append(asked, slices.Clone(indexes))
		out := map[int]win.Process{}
		for _, i := range indexes {
			if i == 3 || i == 4 || i == 7 {
				out[i] = fake
			}
		}
		return out
	}
	t.Cleanup(func() { qemuScan = saved })

	if p, ok, err := m.qemuOf(3); err != nil || !ok || p.PID != 4242 {
		t.Errorf("m.qemuOf(3) = %v %v %v", p, ok, err)
	}
	if _, ok, err := m.qemuOf(2); err != nil || ok {
		t.Errorf("m.qemuOf(2): found=%v err=%v", ok, err)
	}
	if p, ok, err := m.qemuOf(4); err != nil || !ok || p.PID != 4242 {
		t.Errorf("m.qemuOf(4) = %v %v %v", p, ok, err)
	}
	asked = nil
	running, err := m.StopTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// 7 runs as far as the scan goes, but this root has no instance 7
	if !slices.Equal(running, []int{3, 4}) {
		t.Errorf("Running = %v, want [3 4]", running)
	}
	if len(asked) != 1 || !slices.Equal(asked[0], []int{2, 3, 4}) {
		t.Errorf("Running scanned %v, want one scan of [2 3 4]", asked)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.StopTargets(ctx); errs.CodeOf(err) != errs.Canceled {
		t.Errorf("Running canceled = %v", err)
	}
	empty, err := New(managerTestRoot(t), nil).StopTargets(context.Background())
	if err != nil || empty == nil || len(empty) != 0 {
		t.Errorf("Running without instances = %#v, %v (want an empty, non-nil list)", empty, err)
	}
}

func TestStopTargetsIncludesOnlyActiveCreationsAndQemu(t *testing.T) {
	m := New(managerTestRoot(t, 0, 1, 2, 3, 4, 6, 7), nil)
	for i, state := range map[int]string{1: creationCreating, 2: creationCreating, 3: creationFailed, 4: creationCreating, 6: creationFailed} {
		if err := m.setCreation(i, state, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// 1 is between boots; 2 is also running; 4 is a killed creator's stale state.
	// A create lock without creating metadata (7) does not publish an attempt.
	for _, i := range []int{1, 2, 7} {
		l, ok, err := win.TryLock(m.lockPath(i, "create"))
		if err != nil || !ok {
			t.Fatalf("create lock %d: %v %v", i, ok, err)
		}
		t.Cleanup(l.Unlock)
	}
	savedScan, savedBooted := qemuScan, statusBooted
	t.Cleanup(func() { qemuScan, statusBooted = savedScan, savedBooted })
	scans := 0
	qemuScan = func(indexes []int) map[int]win.Process {
		scans++
		if !slices.Equal(indexes, []int{0, 1, 2, 3, 4, 6, 7}) {
			t.Errorf("scan indexes %v", indexes)
		}
		return map[int]win.Process{2: {PID: 4242}, 3: {PID: 4243}, 9: {PID: 4249}}
	}
	statusBooted = func(*Manager, context.Context, int) bool {
		t.Fatal("stop target discovery called adb")
		return false
	}
	targets, err := m.StopTargets(context.Background())
	if err != nil || !slices.Equal(targets, []int{1, 2, 3}) || scans != 1 {
		t.Fatalf("targets %v, scans %d, error %v", targets, scans, err)
	}
}

func TestStopTargetCreationEndsBeforeStop(t *testing.T) {
	for _, state := range []string{"ready", "failed", "exited"} {
		t.Run(state, func(t *testing.T) {
			m, _ := opsTestManager(t)
			i := 3
			if _, err := m.Create(context.Background(), CreateRequest{Index: &i, filesOnly: true}); err != nil {
				t.Fatal(err)
			}
			if err := m.setCreation(i, creationCreating, "", ""); err != nil {
				t.Fatal(err)
			}
			l, ok, err := win.TryLock(m.lockPath(i, "create"))
			if err != nil || !ok {
				t.Fatalf("create lock: %v %v", ok, err)
			}
			t.Cleanup(l.Unlock)
			targets, err := m.StopTargets(context.Background())
			if err != nil || !slices.Equal(targets, []int{i}) {
				t.Fatalf("active target: %v %v", targets, err)
			}
			switch state {
			case "ready":
				err = m.finishCreation(context.Background(), i)
			case "failed":
				err = m.setCreation(i, creationFailed, "setup failed", "")
			}
			l.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			stopped, err := m.Stop(context.Background(), targets[0], StopOptions{CancelCreate: true})
			if err != nil || stopped.InterruptedCreate || stopped.Method != "already_offline" || m.createCanceled(i) {
				t.Fatalf("stale creation target: %+v %v, marker %v", stopped, err, m.createCanceled(i))
			}
		})
	}
}

// The real process scan on a fresh root finds nothing, whatever runs on this machine: a real mas_<i> of another
// root (the production D:\ModAVD, say) has the same name and ports but is not this root's. Only the process table,
// command lines and the emulator's runtime files (%LOCALAPPDATA%\Temp\avd\running) are read; no real qemu lies under
// the fresh root's SDK, so no run lock is probed.
func TestScanQemuFreshRoot(t *testing.T) {
	all := []int{}
	for i := 0; i <= root.MaxIndex; i++ {
		if root.ValidIndex(i) {
			all = append(all, i)
		}
	}
	m := New(managerTestRoot(t, all...), nil)
	if found, err := m.scanQemu(all); err != nil || len(found) != 0 {
		t.Errorf("scanQemu on a fresh root found %v, error=%v", found, err)
	}
	if _, ok, err := m.findQemu(0); err != nil || ok {
		t.Errorf("findQemu(0): found=%v, error=%v", ok, err)
	}
	saved := qemuScan
	qemuScan = nil // the real scan
	t.Cleanup(func() { qemuScan = saved })
	running, err := m.StopTargets(context.Background())
	if err != nil || len(running) != 0 {
		t.Errorf("Running on a fresh root = %v, %v", running, err)
	}
	if found, err := (&Manager{}).scanQemu(all); err != nil || len(found) != 0 {
		t.Errorf("scanQemu without a root found %v, error=%v", found, err)
	}
	if found, err := scanQemuOf(nil, all); err != nil || len(found) != 0 {
		t.Errorf("scanQemuOf without roots found %v, error=%v", found, err)
	}
}

// Every mas_<i> qemu running on this machine whose runtime file names its root is found for that root and for no
// other. Skipped when none runs. Read-only: no root here is written to, nothing is sent to any emulator.
func TestScanQemuRealInstances(t *testing.T) {
	// Looks at the emulators running on this machine (their lock / runtime files, a real adb server): opt in.
	if os.Getenv("MODAVD_TEST_LIVE") == "" {
		t.Skip("set MODAVD_TEST_LIVE=1 to run tests that look at live emulators or start a real adb server")
	}
	procs, err := win.ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	fresh := managerTestRoot(t)
	// where the emulator writes its runtime files, and adb.QemuBelongsTo reads them: %LOCALAPPDATA%\Temp\avd\running
	discovery := adb.EmulatorDiscoveryDir()
	checked := 0
	for _, p := range procs {
		if !isQemuName(p.Name) {
			continue
		}
		cmdline, err := p.CommandLine()
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(discovery, "pid_"+strconv.FormatUint(uint64(p.PID), 10)+".ini"))
		if err != nil {
			continue
		}
		var avdDir string
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "avd.dir="); ok {
				avdDir = v
			}
		}
		base := filepath.Base(avdDir)
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(base, root.AvdNamePrefix), ".avd"))
		owner := &root.Root{Dir: filepath.Dir(filepath.Dir(avdDir))} // <root>\avd\mas_<i>.avd
		if err != nil || !strings.HasPrefix(base, root.AvdNamePrefix) ||
			!cmdlineMatches(cmdline, root.AvdName(i), owner.Ports().Console(i)) {
			continue
		}
		if !adb.QemuBelongsTo(owner, p, i) {
			t.Errorf("qemu %d (%s) does not belong to its own root %s", p.PID, avdDir, owner.Dir)
		}
		found, err := scanQemuOf([]*root.Root{owner}, []int{i})
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := found[i]; !ok || got.PID != p.PID {
			// another qemu of the same root and index would be a broken machine state; report it all the same
			t.Errorf("scanQemuOf(%s, %d) = %v %v, want pid %d", owner.Dir, i, got, ok, p.PID)
		}
		if adb.QemuBelongsTo(fresh, p, i) {
			t.Errorf("qemu %d of %s counted for the fresh root %s", p.PID, owner.Dir, fresh.Dir)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no mas_<i> qemu with a runtime file runs on this machine")
	}
	t.Logf("checked %d running instance(s)", checked)
}

func TestManagerWaitAndWithLog(t *testing.T) {
	m := New(managerTestRoot(t), nil)
	start := time.Now()
	m.Wait(5 * time.Second) // nothing started: returns at once
	if d := time.Since(start); d > time.Second {
		t.Errorf("Wait without background work took %v", d)
	}
	(&Manager{}).Wait(time.Second) // no adb client: nothing to wait for, no panic

	c := &logx.Collector{}
	mm := m.withLog(c.Sink())
	if mm.ADB == m.ADB || mm.ADB.Root != m.Root || mm.ADB.Booting == nil || mm.ADB.OnReregistered == nil {
		t.Errorf("withLog's adb client: %+v", mm.ADB)
	}
	mm.ADB.Log.Warnf("x")
	if got := c.Messages(); len(got) != 1 || got[0] != "x" {
		t.Errorf("withLog's adb client does not log into the sink: %q", got)
	}
}
