//go:build windows

package emu

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/win"
)

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Leftovers of a delete or create that stopped half-way are deleted like AUTO-MAS's delete_instance does (it lets
// NOT_FOUND through to delete_instance_files), and an explicit create on such an index says how to clear it.
func TestDeleteHalfInstances(t *testing.T) {
	m, logs := opsTestManager(t)
	ctx := context.Background()
	create := func(i int) {
		t.Helper()
		_, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &i})
		opsSkipIfNotImplemented(t, err)
		if err != nil {
			t.Fatal(err)
		}
	}
	deleted := func(label string, i int) {
		t.Helper()
		del, err := m.Delete(ctx, i)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if del.Index != i || del.Name != root.AvdName(i) || del.Path != avd.Dir(m.Root, i) {
			t.Fatalf("%s: result %+v", label, del)
		}
		if left := instanceLeftovers(m.Root, i); len(left) != 0 {
			t.Fatalf("%s: left %v", label, left)
		}
		for _, p := range []string{m.lockPath(i, "start"), m.lockPath(i, "stop"), m.Root.ConfigLockPath(i)} {
			if fileOrDirExists(p) {
				t.Errorf("%s: %s left", label, p)
			}
		}
	}

	// A delete that failed half-way (a held data disk): config.ini went, the disk and mas_0.ini stayed.
	create(0)
	writeTestFile(t, filepath.Join(avd.Dir(m.Root, 0), "userdata-qemu.img"), "disk")
	if err := os.Remove(avd.ConfigPath(m.Root, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Info(ctx, 0); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("not an instance any more: %v", err)
	}
	zero := 0
	_, err := m.Create(ctx, CreateRequest{filesOnly: true, Index: &zero})
	if errs.CodeOf(err) != errs.AlreadyExists || !strings.Contains(err.Error(), "不完整") ||
		!strings.Contains(err.Error(), "modavd delete 0 --yes") {
		t.Fatalf("explicit create on leftovers: %v", err)
	}
	deleted("directory without config.ini", 0)
	if !strings.Contains(strings.Join(logs.Messages(), "\n"), "魔改 AVD 实例 0 不完整（只剩：") {
		t.Errorf("leftovers not reported: %q", logs.Messages())
	}
	create(0) // the index is usable again

	// A create killed before mas_1.ini: the directory with config.ini.
	writeTestFile(t, avd.ConfigPath(m.Root, 1), "avd.name=mas_1\n")
	deleted("directory without .ini", 1)

	// mas_2.ini alone.
	writeTestFile(t, avd.IniPath(m.Root, 2), "path.rel=avd\\mas_2.avd\n")
	deleted(".ini only", 2)

	// instances.3 alone (the metadata write of a delete failed).
	if err := m.Root.UpdateInstanceMeta(3, map[string]*jsonord.Value{"memoryMb": jsonord.NewInt(4096)}); err != nil {
		t.Fatal(err)
	}
	deleted("metadata only", 3)

	// Nothing at all.
	if _, err := m.Delete(ctx, 4); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("nothing there: %v", err)
	}

	// Leftovers are still refused while their qemu runs or a start holds the lock.
	writeTestFile(t, avd.IniPath(m.Root, 6), "")
	saved := qemuScan
	qemuScan = func(indexes []int) map[int]win.Process { return map[int]win.Process{6: {PID: 4242}} }
	_, err = m.Delete(ctx, 6)
	qemuScan = saved
	if errs.CodeOf(err) != errs.Busy || !fileOrDirExists(avd.IniPath(m.Root, 6)) {
		t.Fatalf("running: %v", err)
	}
	l, ok, err := win.TryLock(m.lockPath(6, "start"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	_, err = m.Delete(ctx, 6)
	l.Unlock()
	if errs.CodeOf(err) != errs.Busy || !strings.Contains(err.Error(), "正在启动") {
		t.Fatalf("starting: %v", err)
	}
	deleted(".ini only, after all", 6)

	// A junction as the leftover directory is not followed; what it points to stays.
	target := filepath.Join(t.TempDir(), "elsewhere")
	writeTestFile(t, filepath.Join(target, "userdata-qemu.img"), "someone else's disk")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", avd.Dir(m.Root, 7), target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	defer os.Remove(avd.Dir(m.Root, 7))
	if _, err := m.Delete(ctx, 7); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("junction: %v", err)
	}
	if !fileOrDirExists(filepath.Join(target, "userdata-qemu.img")) {
		t.Fatal("junction target touched")
	}
}
