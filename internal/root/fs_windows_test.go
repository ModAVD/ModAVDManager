//go:build windows

package root

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"modavd/internal/errs"
)

// deleteAccess is the DELETE access right: a handle with it is what a pending rename or delete holds.
const deleteAccess = 0x00010000

// openForDelete holds path open with DELETE access (sharing everything). Every other open must share delete then,
// or it fails with a sharing violation, so a read that succeeds meanwhile proves it shares delete.
func openForDelete(t *testing.T, path string) {
	t.Helper()
	p, _ := syscall.UTF16PtrFromString(path)
	h, err := syscall.CreateFile(p, deleteAccess,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.CloseHandle(h) })
}

// TestOpenSharedAllowsDelete: AUTO-MAS deleting an instance (shutil.rmtree) while modavd reads its config.ini goes
// through with OpenShared and fails with os.Open. A replace over the open file fails either way (only logged: that is
// Windows' rename, nothing modavd can change).
func TestOpenSharedAllowsDelete(t *testing.T) {
	dir := t.TempDir()
	inst := filepath.Join(dir, "avd", "mas_3.avd")
	target := filepath.Join(inst, "config.ini")
	writeFile(t, target, []byte("old"))
	writeFile(t, filepath.Join(inst, "userdata-qemu.img"), []byte("disk"))

	plain, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(inst); err == nil {
		t.Fatal("rmtree went through a file held by os.Open: the test no longer shows anything")
	}
	plain.Close()
	writeFile(t, target, []byte("old"))

	f, err := OpenShared(target)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, target+".tmp", []byte("new"))
	if err := os.Rename(target+".tmp", target); err != nil {
		t.Logf("replace over a file open with delete sharing still fails (expected): %v", err)
	}
	if err := os.RemoveAll(inst); err != nil {
		t.Fatalf("rmtree while modavd reads: %v", err)
	}
	if _, err := os.Lstat(inst); !os.IsNotExist(err) {
		t.Fatalf("directory left: %v", err)
	}
	f.Close()

	if _, err := OpenShared(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestSharedReadsDoNotBlockDelete(t *testing.T) {
	r := newRoot(t)
	writeFile(t, r.MetadataPath(), []byte(`{"adbServerPort": 20391}`))
	props := filepath.Join(r.SDK(), "emulator", "source.properties")
	writeFile(t, props, []byte("Pkg.Revision=37.2.10\r\nPkg.BuildId=mas-35\r\n"))
	writeFile(t, r.ManifestPath(), []byte(`{"version": "v0.35.2"}`))
	exe := filepath.Join(t.TempDir(), "qemu-system-x86_64-headless.exe")
	writeFile(t, exe, []byte("xx"+SelfBuiltMark+"yy"))
	for _, p := range []string{r.MetadataPath(), props, r.ManifestPath(), exe} {
		openForDelete(t, p)
	}
	if _, err := os.ReadFile(props); err == nil {
		t.Fatal("os.ReadFile shares delete on this Go version: the test no longer shows anything")
	}
	if v, err := r.ReadMetadata(); err != nil || r.AdbServerPort() != 20391 {
		t.Fatalf("ReadMetadata: %v %v", v, err)
	}
	if b, ok := r.ModBuild(); !ok || b != 35 || r.EmulatorVersion() != "37.2.10" {
		t.Fatalf("source.properties: %d %v %q", b, ok, r.EmulatorVersion())
	}
	if got := r.VersionText(); got != "0.35.2" {
		t.Fatalf("manifest: %q", got)
	}
	if !HasSelfBuiltMark(exe) {
		t.Fatal("mark scan")
	}
}

func TestLockInstanceConfig(t *testing.T) {
	r := newRoot(t)
	if got := r.ConfigLockPath(3); got != filepath.Join(r.Dir, "avd", ".mas_3.config.lock") {
		t.Fatalf("path %s", got)
	}
	// No avd directory: nothing to guard, and nothing created.
	_, err := r.LockInstanceConfig(3)
	wantCode(t, err, errs.NotFound)
	if _, err := os.Stat(r.AvdHome()); !os.IsNotExist(err) {
		t.Fatal("avd directory created")
	}

	if err := os.MkdirAll(r.AvdHome(), 0o755); err != nil {
		t.Fatal(err)
	}
	l, err := r.LockInstanceConfig(3)
	if err != nil {
		t.Fatal(err)
	}
	old := configLockWait
	configLockWait = 200 * time.Millisecond
	defer func() { configLockWait = old }()
	// A second holder in the same process waits (goroutines are serialized too), then gives up with Busy.
	started := time.Now()
	_, err = r.LockInstanceConfig(3)
	wantCode(t, err, errs.Busy)
	if time.Since(started) < 150*time.Millisecond {
		t.Fatalf("did not wait (%v)", time.Since(started))
	}
	// Another instance has its own lock.
	other, err := r.LockInstanceConfig(4)
	if err != nil {
		t.Fatal(err)
	}
	other.Unlock()

	done := make(chan error, 1)
	configLockWait = 5 * time.Second
	go func() {
		l2, err := r.LockInstanceConfig(3)
		l2.Unlock()
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	l.Unlock()
	l.Unlock() // idempotent
	if err := <-done; err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	var none *InstanceLock
	none.Unlock() // nil-safe
}

func TestHasInstanceMeta(t *testing.T) {
	r := newRoot(t)
	if r.HasInstanceMeta(0) {
		t.Fatal("no metadata")
	}
	writeFile(t, r.MetadataPath(), []byte(`{"instances": {"0": {}, "1": 5}}`))
	if !r.HasInstanceMeta(0) || !r.HasInstanceMeta(1) || r.HasInstanceMeta(2) {
		t.Fatal("HasInstanceMeta")
	}
	writeFile(t, r.MetadataPath(), []byte(`{broken`))
	if r.HasInstanceMeta(0) {
		t.Fatal("unreadable metadata")
	}
}
