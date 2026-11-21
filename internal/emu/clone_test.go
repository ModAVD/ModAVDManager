//go:build windows

package emu

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/avd"
	"modavd/internal/bosskey"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/win"
)

// fakeCloneQemuImg stands in for qemu-img (nothing is started): info answers from backing (by file base name),
// convert writes the target, rebase fails when rebaseCode is not 0. calls records every run.
type fakeCloneQemuImg struct {
	mu         sync.Mutex
	backing    map[string]string // base name -> backing-filename
	snapshots  map[string]bool
	rebaseCode int
	calls      [][]string
}

func (f *fakeCloneQemuImg) run(_ *Manager, _ context.Context, _ time.Duration, args ...string) ([]byte, []byte, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "info":
		img := args[len(args)-1]
		b := f.backing[filepath.Base(img)]
		snaps := "[]"
		if f.snapshots[filepath.Base(img)] {
			snaps = `[{"id":"1"}]`
		}
		js := fmt.Sprintf(`{"format":"qcow2","virtual-size":1024,"backing-filename":%q,"backing-filename-format":"qcow2","snapshots":%s}`,
			b, snaps)
		return []byte(js), nil, 0, nil
	case "convert":
		if err := os.WriteFile(args[len(args)-1], []byte("flattened"), 0o644); err != nil {
			return nil, nil, 0, err
		}
		return nil, nil, 0, nil
	case "rebase":
		return nil, []byte("rebase says no"), f.rebaseCode, nil
	}
	return nil, []byte("unexpected"), 1, nil
}

func (f *fakeCloneQemuImg) ran(verb string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == verb {
			out = append(out, c)
		}
	}
	return out
}

// cloneTestSetup is a test root (never the real one) with instance 0 holding a small fake instance directory, a fake
// qemu-img and plenty of fake free space.
func cloneTestSetup(t *testing.T) (*Manager, *fakeCloneQemuImg, *avd.Instance) {
	t.Helper()
	m, _ := opsTestManager(t)
	zero := 0
	if _, err := m.Create(context.Background(), CreateRequest{filesOnly: true, Index: &zero, Title: "源实例"}); err != nil {
		opsSkipIfNotImplemented(t, err)
		t.Fatal(err)
	}
	in, err := m.instance(0)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"userdata-qemu.img":        "base",
		"userdata-qemu.img.qcow2":  strings.Repeat("game data ", 1000),
		"cache.img.qcow2":          "cache",
		"system.img.qcow2":         "system overlay",
		"encryptionkey.img":        "key",
		"hardware-qemu.ini":        `disk.dataPartition.path = C:\elsewhere\mas_0.avd\userdata-qemu.img`,
		"emu-launch-params.txt":    "mas_0",
		"multiinstance.lock":       "",
		"config.ini.modavd-99.tmp": "dead writer",
		filepath.Join("snapshots", "default_boot", "ram.img"): "ram",
		filepath.Join("data", "misc", "pstore", "pstore.bin"): "pstore",
	} {
		writeTestFile(t, filepath.Join(in.Dir, name), content)
	}
	if err := m.Root.EditInstanceMeta(0, func(o *jsonord.Obj) error {
		o.Set("sound", jsonord.NewBool(true))
		o.Set("bridge", jsonord.NewBool(true))
		o.Set(bosskey.MetaKey, jsonord.NewString("Ctrl+Alt+0"))
		o.Set("diagnostics", jsonord.NewBool(false))
		o.Set("initialized", jsonord.NewBool(true))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCloneQemuImg{backing: map[string]string{"userdata-qemu.img.qcow2": "userdata-qemu.img"}}
	savedRun, savedFree := cloneRunQemuImg, cloneDiskFree
	cloneRunQemuImg = fake.run
	cloneDiskFree = func(string) (uint64, error) { return 1 << 40, nil }
	t.Cleanup(func() { cloneRunQemuImg, cloneDiskFree = savedRun, savedFree })
	return m, fake, in
}

// nothingOf1: no trace of instance 1 (the clone's index) is left: files, metadata, lock files.
func nothingOf1(t *testing.T, m *Manager, label string) {
	t.Helper()
	if left := instanceLeftovers(m.Root, 1); len(left) != 0 {
		t.Errorf("%s: left %v", label, left)
	}
	for _, kind := range []string{"create", "stop"} {
		if fileOrDirExists(m.lockPath(1, kind)) {
			t.Errorf("%s: lock file %s left", label, kind)
		}
	}
}

func TestCloneCopiesInstance(t *testing.T) {
	m, fake, src := cloneTestSetup(t)
	var steps []Progress
	res, err := m.Clone(context.Background(), 0, CloneRequest{Title: "副本", Progress: func(p Progress) { steps = append(steps, p) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != 0 || res.Instance.Index != 1 || res.Instance.Title != "副本" || res.Instance.Status != StatusOffline {
		t.Fatalf("result %+v", res)
	}
	dst := avd.Dir(m.Root, 1)
	for _, name := range []string{"userdata-qemu.img", "userdata-qemu.img.qcow2", "cache.img.qcow2", "system.img.qcow2",
		"encryptionkey.img", filepath.Join("data", "misc", "pstore", "pstore.bin")} {
		a, err := os.ReadFile(filepath.Join(src.Dir, name))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dst, name))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s not copied: %v", name, err)
		}
	}
	for _, name := range []string{"hardware-qemu.ini", "emu-launch-params.txt", "multiinstance.lock", "snapshots",
		"config.ini.modavd-99.tmp"} {
		if fileOrDirExists(filepath.Join(dst, name)) {
			t.Errorf("%s copied", name)
		}
	}
	// what is in the copy, config.ini as copied (RegisterCopy rewrites it afterwards) and the bridge marker included
	var copiedFiles uint64
	filepathWalkSizes(t, dst, &copiedFiles)
	cfgSize := func(dir string) int64 { fi, _ := os.Stat(filepath.Join(dir, "config.ini")); return fi.Size() }
	want := int64(copiedFiles) - cfgSize(dst) + cfgSize(src.Dir)
	if res.CopiedBytes != want {
		t.Errorf("copied %d bytes, want %d", res.CopiedBytes, want)
	}
	// the system overlay is the shared image's (not asked), the relative data disk chain needs nothing
	for _, c := range fake.ran("info") {
		if strings.EqualFold(filepath.Base(c[len(c)-1]), "system.img.qcow2") {
			t.Errorf("system overlay inspected: %v", c)
		}
	}
	if len(fake.ran("rebase"))+len(fake.ran("convert")) != 0 {
		t.Errorf("relative backing rewritten: %v", fake.calls)
	}

	cfg, err := avd.ReadIni(avd.ConfigPath(m.Root, 1))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Value("avd.name") != "mas_1" || cfg.Value("avd.id") != "mas_1" || cfg.Value("avd.ini.displayname") != "副本" ||
		cfg.Value("hw.ramSize") != src.Config.Value("hw.ramSize") || cfg.Value("hw.lcd.vsync") != src.Config.Value("hw.lcd.vsync") {
		t.Errorf("config.ini %v", cfg.Encode())
	}
	ini, err := avd.ReadIni(avd.IniPath(m.Root, 1))
	if err != nil || ini.Value("path") != dst || ini.Value("path.rel") != filepath.Join("avd", "mas_1.avd") {
		t.Errorf("mas_1.ini %v %v", ini, err)
	}
	meta := m.Root.InstanceMeta(1)
	if s, _ := meta.Get("sound").BoolVal(); !s {
		t.Error("sound not carried over")
	}
	if b, _ := meta.Get("initialized").BoolVal(); !b {
		t.Error("initialized not carried over")
	}
	for _, k := range []string{bosskey.MetaKey, "diagnostics", "creationState"} {
		if meta.Has(k) {
			t.Errorf("%s carried over", k)
		}
	}
	if c, _ := meta.Get("createdAt").Str(); c == "" {
		t.Error("no createdAt")
	}
	// the source is as it was
	if s, _ := m.Root.InstanceMeta(0).Get(bosskey.MetaKey).Str(); s != "Ctrl+Alt+0" {
		t.Error("source metadata changed")
	}
	if in0, _ := m.instance(0); in0.Config.Value("avd.name") != "mas_0" || in0.Title != "源实例" {
		t.Errorf("source config changed: %+v", in0)
	}
	if len(steps) < 2 || steps[len(steps)-1].Done != 1000 || steps[0].Step != "copy" {
		t.Errorf("progress %+v", steps)
	}
	// no title: the AVD name, like create
	res, err = m.Clone(context.Background(), 0, CloneRequest{})
	if err != nil || res.Instance.Index != 2 || res.Instance.Title != "mas_2" {
		t.Fatalf("second clone: %+v %v", res, err)
	}
}

func TestCloneRefusesBusySource(t *testing.T) {
	m, _, _ := cloneTestSetup(t)
	ctx := context.Background()

	saved := qemuScan
	qemuScan = func([]int) map[int]win.Process { return map[int]win.Process{0: {PID: 4242}} }
	_, err := m.Clone(ctx, 0, CloneRequest{})
	qemuScan = saved
	if errs.CodeOf(err) != errs.Busy {
		t.Fatalf("running: %v", err)
	}
	wantErrorDetails(t, err, map[string]string{errs.Reason: "running"})
	nothingOf1(t, m, "running")

	for _, kind := range []string{"start", "stop", "bridge", "create", "disk"} {
		l, ok, err := win.TryLock(m.lockPath(0, kind))
		if err != nil || !ok {
			t.Fatalf("TryLock %s: %v %v", kind, ok, err)
		}
		_, err = m.Clone(ctx, 0, CloneRequest{})
		l.Unlock()
		if errs.CodeOf(err) != errs.Busy {
			t.Fatalf("%s lock held: %v", kind, err)
		}
		wantErrorDetails(t, err, map[string]string{errs.Reason: "locked", errs.Lock: kind})
		nothingOf1(t, m, kind)
	}

	if _, err := m.Clone(ctx, 3, CloneRequest{}); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("missing source: %v", err)
	}
	if _, err := m.Clone(ctx, 0, CloneRequest{Title: "a\nb"}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("bad title: %v", err)
	}
	nothingOf1(t, m, "bad title")
	// a source whose setup failed is refused
	if err := m.setCreation(0, creationFailed, "boom", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Clone(ctx, 0, CloneRequest{}); errs.CodeOf(err) != errs.NotReady {
		t.Fatalf("create_failed source: %v", err)
	}
	nothingOf1(t, m, "create_failed")
}

func TestCloneRefusesWithoutRoom(t *testing.T) {
	m, _, src := cloneTestSetup(t)
	_, n, err := m.clonePlan(context.Background(), src.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var all uint64
	filepathWalkSizes(t, src.Dir, &all)
	need := uint64(n)
	if need == 0 || need >= all {
		t.Fatalf("plan %d bytes of %d: the skipped files must not count", need, all)
	}
	cloneDiskFree = func(string) (uint64, error) { return need + cloneHeadroom - 1, nil }
	_, err = m.Clone(context.Background(), 0, CloneRequest{})
	if errs.CodeOf(err) != errs.PrecheckFailed {
		t.Fatalf("no room: %v", err)
	}
	wantErrorDetails(t, err, map[string]string{errs.Reason: "disk_space"})
	nothingOf1(t, m, "no room")
	// exactly enough: it goes through
	cloneDiskFree = func(string) (uint64, error) { return need + cloneHeadroom, nil }
	if _, err := m.Clone(context.Background(), 0, CloneRequest{}); err != nil {
		t.Fatalf("just enough room: %v", err)
	}
}

// filepathWalkSizes adds up every file below dir (more than the clone copies: the skipped files count too).
func filepathWalkSizes(t *testing.T, dir string, total *uint64) {
	t.Helper()
	err := filepath.Walk(dir, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			*total += uint64(fi.Size())
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestCloneRollsBack(t *testing.T) {
	m, fake, src := cloneTestSetup(t)

	// a failing step half way (the data disk's absolute backing inside the source cannot be renamed)
	fake.backing["userdata-qemu.img.qcow2"] = filepath.Join(src.Dir, "userdata-qemu.img")
	fake.rebaseCode = 1
	_, err := m.Clone(context.Background(), 0, CloneRequest{})
	if err == nil || !strings.Contains(err.Error(), "rebase") {
		t.Fatalf("rebase failure: %v", err)
	}
	nothingOf1(t, m, "rebase failed")
	if r := fake.ran("rebase"); len(r) != 1 || r[0][2] != "-b" || r[0][3] != "userdata-qemu.img" ||
		filepath.Dir(r[0][len(r[0])-1]) != avd.Dir(m.Root, 1) {
		t.Fatalf("rebase %v", r)
	}

	// canceled half way
	fake.rebaseCode = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err = m.Clone(ctx, 0, CloneRequest{Progress: func(Progress) {
		if calls++; calls == 1 {
			cancel()
		}
	}})
	if errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("canceled: %v", err)
	}
	nothingOf1(t, m, "canceled")

}

func TestCloneBackings(t *testing.T) {
	m, fake, src := cloneTestSetup(t)
	outside := filepath.Join(t.TempDir(), "base.img")
	writeTestFile(t, outside, strings.Repeat("x", 4096))
	fake.backing["userdata-qemu.img.qcow2"] = strings.ToUpper(filepath.Join(src.Dir, "userdata-qemu.img")) // any case
	fake.backing["cache.img.qcow2"] = outside
	res, err := m.Clone(context.Background(), 0, CloneRequest{})
	if err != nil {
		t.Fatal(err)
	}
	dst := res.Instance.Path
	if r := fake.ran("rebase"); len(r) != 1 || !strings.EqualFold(r[0][3], "userdata-qemu.img") || r[0][len(r[0])-1] != filepath.Join(dst, "userdata-qemu.img.qcow2") {
		t.Errorf("rebase %v", r)
	}
	c := fake.ran("convert")
	if len(c) != 1 || c[0][len(c[0])-2] != filepath.Join(src.Dir, "cache.img.qcow2") || c[0][len(c[0])-1] != filepath.Join(dst, "cache.img.qcow2") {
		t.Errorf("convert %v", c)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "cache.img.qcow2")); string(b) != "flattened" {
		t.Errorf("flattened copy %q", b)
	}

	// internal snapshots in a disk: refused before anything is written
	fake.snapshots = map[string]bool{"userdata-qemu.img.qcow2": true}
	if _, err := m.Clone(context.Background(), 0, CloneRequest{}); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("snapshots: %v", err)
	}
	if left := instanceLeftovers(m.Root, 2); len(left) != 0 {
		t.Errorf("snapshots: left %v", left)
	}
}
