//go:build windows

package avd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
)

var defaultOpts = CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60}

func mustCreate(t *testing.T, r *root.Root, i int, opt CreateOptions) {
	t.Helper()
	if _, err := Create(r, i, opt); err != nil {
		t.Fatal(err)
	}
}

// An update reads the config only once it holds the instance lock, so a change made by the holder meanwhile stays.
func TestUpdateWaitsForConfigLock(t *testing.T) {
	r := newRoot(t)
	mustCreate(t, r, 0, defaultOpts)
	l, err := r.LockInstanceConfig(0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Update(r, 0, UpdateOptions{CPU: ip(2)})
		done <- err
	}()
	select {
	case err := <-done:
		l.Unlock()
		t.Fatalf("update did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// The holder's own read-modify-write (another modavd changing the frame rate).
	cfg, err := ReadIni(ConfigPath(r, 0))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Set("hw.lcd.vsync", "30")
	if err := WriteIni(ConfigPath(r, 0), cfg); err != nil {
		t.Fatal(err)
	}
	l.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	in, err := Get(r, 0)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := in.CPU(); c != 2 || in.FPS() != 30 {
		t.Fatalf("lost update: cpu %d fps %d", c, in.FPS())
	}

	// Create waits too (an update or delete of the same index must see the whole instance).
	l, err = r.LockInstanceConfig(3)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, err := Create(r, 3, defaultOpts)
		done <- err
	}()
	select {
	case err := <-done:
		l.Unlock()
		t.Fatalf("create did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	l.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := Get(r, 3); err != nil {
		t.Fatal(err)
	}
}

// A writer that finds the instance directory gone (deleted by AUTO-MAS, which takes no lock of ours) fails with
// NotFound instead of creating mas_<i>.avd\config.ini again.
func TestWritesNeverRecreateDeletedInstance(t *testing.T) {
	r := newRoot(t)
	mustCreate(t, r, 0, defaultOpts)
	if err := os.RemoveAll(Dir(r, 0)); err != nil {
		t.Fatal(err)
	}
	_, err := Update(r, 0, UpdateOptions{CPU: ip(2)})
	wantCode(t, err, errs.NotFound)
	// The same after a read that still found it (the delete came between read and write).
	wantCode(t, writeInstanceConfig(r, 0, [][2]string{{"hw.cpu.ncore", "2"}}), errs.NotFound)
	_, err = ApplyDisplay(r, 0)
	wantCode(t, err, errs.NotFound)
	wantCode(t, WriteIni(ConfigPath(r, 0), NewIni()), errs.NotFound)
	if _, err := os.Lstat(Dir(r, 0)); !os.IsNotExist(err) {
		t.Fatal("instance directory recreated")
	}
	if n, _ := r.InstanceMeta(0).Get("memoryMb").Int(); n != 6144 {
		t.Fatalf("metadata changed: %d", n)
	}
}

// Delete takes whatever is left of an index (AUTO-MAS delete_instance_files), and the config lock file with it.
func TestDeleteLeftovers(t *testing.T) {
	r := newRoot(t)
	exists := func(p string) bool { _, err := os.Lstat(p); return err == nil }
	check := func(label string, i int) {
		t.Helper()
		if err := Delete(r, i); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		for _, p := range []string{IniPath(r, i), Dir(r, i), r.ConfigLockPath(i)} {
			if exists(p) {
				t.Errorf("%s: %s left", label, p)
			}
		}
		if r.HasInstanceMeta(i) {
			t.Errorf("%s: instances.%d left", label, i)
		}
	}

	// A failed delete: config.ini gone, the data disk and mas_0.ini left.
	mustCreate(t, r, 0, defaultOpts)
	writeFile(t, filepath.Join(Dir(r, 0), "userdata-qemu.img"), []byte("disk"))
	os.Remove(ConfigPath(r, 0))
	if _, err := Get(r, 0); errs.CodeOf(err) != errs.NotFound {
		t.Fatalf("not a listed instance any more: %v", err)
	}
	check("directory without config.ini", 0)

	// The .ini alone.
	mustCreate(t, r, 1, defaultOpts)
	os.RemoveAll(Dir(r, 1))
	check(".ini only", 1)

	// A create killed before the .ini: directory and config.ini, no .ini, no metadata.
	writeFile(t, ConfigPath(r, 2), []byte("avd.name=mas_2\n"))
	check("directory without .ini", 2)

	// Only instances.<i>.
	if err := r.UpdateInstanceMeta(4, nil); err != nil {
		t.Fatal(err)
	}
	check("metadata only", 4)

	// No avd directory at all: the metadata goes, nothing is created.
	r2 := newRoot(t)
	writeFile(t, r2.MetadataPath(), []byte(`{"instances": {"6": {"memoryMb": 4096}}}`))
	if err := Delete(r2, 6); err != nil {
		t.Fatal(err)
	}
	if r2.HasInstanceMeta(6) || exists(r2.AvdHome()) {
		t.Fatal("no avd directory")
	}
}

// ReadIni shares delete: it never stands in the way of AUTO-MAS deleting the instance.
func TestReadIniSharesDelete(t *testing.T) {
	r := newRoot(t)
	mustCreate(t, r, 0, CreateOptions{Title: "方舟", MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60})
	p, _ := syscall.UTF16PtrFromString(ConfigPath(r, 0))
	const deleteAccess = 0x00010000
	h, err := syscall.CreateFile(p, deleteAccess,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if _, err := os.ReadFile(ConfigPath(r, 0)); err == nil {
		t.Fatal("os.ReadFile shares delete on this Go version: the test no longer shows anything")
	}
	cfg, err := ReadIni(ConfigPath(r, 0))
	if err != nil || cfg.Value("avd.ini.displayname") != "方舟" {
		t.Fatalf("ReadIni: %v", err)
	}
	if in, err := Get(r, 0); err != nil || in.Title != "方舟" {
		t.Fatalf("Get: %+v %v", in, err)
	}
}
