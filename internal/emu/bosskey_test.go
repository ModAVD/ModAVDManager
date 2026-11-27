//go:build windows

package emu

import (
	"context"
	"slices"
	"sync"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/win"
)

// fakeWindows stands for the emulator windows of the boss key tests: qemu processes by instance (pid 1000+i), which of
// them run with a window, and whether that window shows now. Setting visibility goes to the window state.
type fakeWindows struct {
	mu       sync.Mutex
	running  map[int]bool // instance -> windowed
	shown    map[uint32]bool
	setCalls []string
}

func installFakeWindows(t *testing.T, running map[int]bool, shown map[int]bool) *fakeWindows {
	t.Helper()
	f := &fakeWindows{running: running, shown: map[uint32]bool{}}
	for i, s := range shown {
		f.shown[uint32(1000+i)] = s
	}
	scan, windowed, isShown, set := qemuScan, qemuWindowedOf, emulatorWindowsShown, setEmulatorWindowsVisible
	t.Cleanup(func() {
		qemuScan, qemuWindowedOf, emulatorWindowsShown, setEmulatorWindowsVisible = scan, windowed, isShown, set
	})
	qemuScan = func(indexes []int) map[int]win.Process {
		out := map[int]win.Process{}
		for _, i := range indexes {
			if _, ok := f.running[i]; ok {
				out[i] = win.Process{PID: uint32(1000 + i)}
			}
		}
		return out
	}
	qemuWindowedOf = func(p host.Process) (bool, bool) { return f.running[int(p.PID)-1000], true }
	emulatorWindowsShown = func(pid uint32) (bool, int, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.running[int(pid)-1000] {
			return false, 0, nil
		}
		return f.shown[pid], 2, nil
	}
	setEmulatorWindowsVisible = func(pid uint32, visible bool) (int, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if !f.running[int(pid)-1000] {
			return 0, nil
		}
		f.shown[pid] = visible
		f.setCalls = append(f.setCalls, map[bool]string{true: "show", false: "hide"}[visible]+" "+string(rune('0'+pid-1000)))
		return 2, nil
	}
	return f
}

func (f *fakeWindows) isShown(i int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.shown[uint32(1000+i)]
}

func setKey(t *testing.T, m *Manager, i int, key string) {
	t.Helper()
	if _, err := m.Update(context.Background(), i, UpdateRequest{BossKey: &key}); err != nil {
		t.Fatalf("set boss key %d %q: %v", i, key, err)
	}
}

// Default keys, the key stored and read back, cleared, and a bad key refused without writing anything.
func TestBossKeySetting(t *testing.T) {
	m := New(managerTestRoot(t, 1, 3), nil)
	installFakeWindows(t, map[int]bool{}, nil)
	info := func(i int) *InstanceInfo {
		t.Helper()
		in, err := m.Info(context.Background(), i)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	if k := info(1).BossKey; k != "Ctrl+Alt+Shift+1" {
		t.Fatalf("default of 1: %q", k)
	}
	key := "alt+shift+F5"
	res, err := m.Update(context.Background(), 1, UpdateRequest{BossKey: &key})
	if err != nil {
		t.Fatal(err)
	}
	if res.Applied["boss_key"] != "Alt+Shift+F5" || res.Instance.BossKey != "Alt+Shift+F5" || res.AppliesNextBoot {
		t.Fatalf("update result %+v", res)
	}
	if v, _ := m.Root.InstanceMeta(1).Get("bossKey").Str(); v != "Alt+Shift+F5" {
		t.Fatalf("stored %q", v)
	}
	setKey(t, m, 1, "none")
	if k := info(1).BossKey; k != "" {
		t.Fatalf("cleared: %q", k)
	}
	if v, ok := m.Root.InstanceMeta(1).Get("bossKey").Str(); !ok || v != "" {
		t.Fatalf("cleared is stored as \"\": %q %v", v, ok)
	}
	// a bad key changes nothing, not even the other settings of the same request
	bad, title := "Win+3", "改了"
	if _, err := m.Update(context.Background(), 3, UpdateRequest{BossKey: &bad, Title: &title}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("Win+3: %v", err)
	}
	if in := info(3); in.BossKey != "Ctrl+Alt+Shift+3" || in.Title == title {
		t.Fatalf("after a refused key: %+v", in)
	}
	// the delete takes the key with instances.<i>
	if _, err := m.Delete(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if m.Root.InstanceMeta(1).Has("bossKey") {
		t.Fatal("bossKey left behind by delete")
	}
}

// One press: any shown window among the key's windowed instances hides them all, else all come back; headless and
// stopped instances and other keys are left alone, whoever hid a window.
func TestToggleBossKey(t *testing.T) {
	r := managerTestRoot(t, 0, 1, 2, 3, 4)
	m := New(r, nil)
	// 1, 2 and 3 share a key; 3 runs headless, 4 has its own key, 0 is stopped
	f := installFakeWindows(t, map[int]bool{1: true, 2: true, 3: false, 4: true}, map[int]bool{1: true, 2: false, 4: true})
	const shared = "Ctrl+Alt+Shift+9"
	for _, i := range []int{0, 1, 2, 3} {
		setKey(t, m, i, shared)
	}
	press := func(want bool, wantInstances ...int) {
		t.Helper()
		res, err := m.ToggleBossKey(context.Background(), shared)
		if err != nil {
			t.Fatal(err)
		}
		if res.Visible != want || !slices.Equal(res.Instances, wantInstances) {
			t.Fatalf("press: %+v, want visible %v on %v", res, want, wantInstances)
		}
		for _, i := range wantInstances {
			if f.isShown(i) != want {
				t.Fatalf("instance %d shown %v, want %v", i, f.isShown(i), want)
			}
		}
		if !f.isShown(4) {
			t.Fatal("another key's instance was touched")
		}
	}
	press(false, 1, 2) // 1 shows: hide both
	press(true, 1, 2)  // all hidden: show both
	press(false, 1, 2)
	// hidden by someone else (AUTO-MAS, modavd window): the key brings it back
	f.shown[1001], f.shown[1002] = false, false
	press(true, 1, 2)

	// a key of one instance just flips it
	res, err := m.ToggleBossKey(context.Background(), "Ctrl+Alt+Shift+4")
	if err != nil || !slices.Equal(res.Instances, []int{4}) || res.Visible || f.isShown(4) {
		t.Fatalf("own key: %+v %v", res, err)
	}
	// a key nobody uses, or no key: nothing
	for _, k := range []string{"Ctrl+Alt+Shift+7", ""} {
		if res, err := m.ToggleBossKey(context.Background(), k); err != nil || len(res.Instances) != 0 {
			t.Fatalf("key %q: %+v %v", k, res, err)
		}
	}

	// the tray icon / exit: every hidden windowed instance comes back, the shown ones are left as they are
	f.setCalls = nil
	f.shown[1001] = false
	shown, err := m.ShowHiddenWindows(context.Background())
	if err != nil || !slices.Equal(shown, []int{1, 4}) || !f.isShown(1) || !f.isShown(4) {
		t.Fatalf("show hidden: %v %v (calls %v)", shown, err, f.setCalls)
	}
	if !slices.Equal(f.setCalls, []string{"show 1", "show 4"}) {
		t.Fatalf("calls %v", f.setCalls)
	}

	targets, err := m.BossKeyTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []BossKeyTarget{{0, shared, false, 0}, {1, shared, true, 1001}, {2, shared, true, 1002}, {3, shared, false, 1003},
		{4, "Ctrl+Alt+Shift+4", true, 1004}}
	if !slices.Equal(targets, want) {
		t.Fatalf("targets %+v", targets)
	}
}
