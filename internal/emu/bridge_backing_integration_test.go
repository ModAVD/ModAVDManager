//go:build windows

package emu

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
)

// MODAVD_TEST_SDK_ROOT supplies only qemu-img. Move a disposable root with real
// qcow2 clusters that differ from their bases, and compare the guest-visible disks.
func TestBridgeBackingRelocationIntegration(t *testing.T) {
	sdk := os.Getenv("MODAVD_TEST_SDK_ROOT")
	if sdk == "" {
		t.Skip("set MODAVD_TEST_SDK_ROOT to an SDK with qemu-img.exe")
	}
	qemu := filepath.Join(sdk, "emulator", "qemu-img.exe")
	t.Setenv("PATH", filepath.Dir(qemu)+string(os.PathListSeparator)+os.Getenv("PATH")) // DLLs of the packaged qemu-img
	parent := opsTestTempDir(t)
	path := filepath.Join(parent, "before")
	openRoot := func() (*Manager, *avd.Instance) {
		r, err := root.New(path)
		if err != nil {
			t.Fatal(err)
		}
		cfg := avd.NewIni()
		cfg.Set("image.sysdir.1", avd.SystemImageSysdir)
		return New(r, nil), &avd.Instance{Dir: avd.Dir(r, 3), Index: 3, Config: cfg}
	}
	if err := os.Mkdir(path, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, root.MetadataFile), []byte(`{"sdkRoot":"alternate SDK"}`), 0600); err != nil {
		t.Fatal(err)
	}
	m, in := openRoot()
	imageDir := func() string { return filepath.Join(m.Root.RuntimeSDK(), root.SystemImageDir) }
	mkdir := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) []byte {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	run := func(args ...string) {
		t.Helper()
		if b, err := exec.Command(qemu, args...).CombinedOutput(); err != nil {
			t.Fatalf("qemu-img %v: %v\n%s", args, err, b)
		}
	}
	mkdir(in.Dir)
	mkdir(imageDir())
	emuDir := filepath.Join(m.Root.RuntimeSDK(), "emulator")
	mkdir(emuDir)
	copyFile(t, qemu, filepath.Join(emuDir, "qemu-img.exe"))
	write(filepath.Join(in.Dir, bridgeMarkerName), nil)
	names := []string{"system.img", "vendor.img"}
	overlay := func(n string) string { return filepath.Join(in.Dir, n+".qcow2") }
	for i, n := range names {
		raw := bytes.Repeat([]byte{byte(42 + i)}, 1<<20)
		base := filepath.Join(imageDir(), n)
		write(base, raw)
		raw[0] = 5
		want := filepath.Join(parent, n+".expected")
		write(want, raw)
		run("convert", "-f", "raw", "-O", "qcow2", "-B", base, "-o", "backing_fmt=raw", want, overlay(n))
	}
	prepare := func() error { return m.prepareBridgeBackings(t.Context(), in) }
	// Enroll a legacy instance while its actual bases are still accessible.
	if err := prepare(); err != nil {
		t.Fatal(err)
	}
	originalSystem := read(overlay(names[0]))
	originalVendor := read(overlay(names[1]))
	proof := read(filepath.Join(in.Dir, bridgeBackingFile))
	dest := filepath.Join(parent, "after with spaces")
	if err := os.Rename(path, dest); err != nil {
		t.Fatal(err)
	}
	path = dest
	m, in = openRoot()
	reject := func(detail string) {
		t.Helper()
		err := prepare()
		if errs.CodeOf(err) != errs.PrecheckFailed || !strings.Contains(err.Error(), detail) {
			t.Fatalf("want rejection with %q, got %v", detail, err)
		}
		if !bytes.Equal(originalSystem, read(overlay(names[0]))) || !bytes.Equal(originalVendor, read(overlay(names[1]))) {
			t.Fatal("rejected pair changed an overlay")
		}
	}
	record := filepath.Join(in.Dir, bridgeBackingFile)
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	reject("SHA-256")
	write(record, proof)
	// The old backing string is part of the evidence, even for an identical
	// candidate. An unrelated same-named SDK is insufficient.
	oldVendor := filepath.Join(parent, "before", "alternate SDK", root.SystemImageDir, names[1])
	orphan := filepath.Join(parent, "unrelated SDK", root.SystemImageDir, names[1])
	run("rebase", "-u", "-f", "qcow2", "-F", "raw", "-b", orphan, overlay(names[1]))
	if err := prepare(); errs.CodeOf(err) != errs.PrecheckFailed || !strings.Contains(err.Error(), orphan) {
		t.Fatalf("unrelated source: %v", err)
	}
	run("rebase", "-u", "-f", "qcow2", "-F", "raw", "-b", oldVendor, overlay(names[1]))
	originalVendor = read(overlay(names[1]))
	vendor := filepath.Join(imageDir(), names[1])
	data := read(vendor)
	if err := os.Remove(vendor); err != nil {
		t.Fatal(err)
	}
	reject(vendor)
	data[8] ^= 1
	write(vendor, data)
	reject("SHA-256")
	data[8] ^= 1
	write(vendor, data)
	// Block only the second rename. The first header may already be installed;
	// both paths must still be provable after closing this handle and retrying.
	write(overlay(names[0])+".modavd-rebase.tmp", []byte("interrupted copy"))
	held, err := os.Open(overlay(names[1]))
	if err != nil {
		t.Fatal(err)
	}
	err = prepare()
	held.Close()
	if err == nil || bytes.Equal(originalSystem, read(overlay(names[0]))) || !bytes.Equal(originalVendor, read(overlay(names[1]))) {
		t.Fatalf("expected interruption after system, got %v", err)
	}
	if err := prepare(); err != nil {
		t.Fatalf("retry: %v", err)
	}
	for _, n := range names {
		info, err := m.qemuImgInfo(t.Context(), overlay(n))
		if err != nil || info.BackingFilename != filepath.Join(imageDir(), n) || info.BackingFilenameFmt != "raw" {
			t.Fatalf("repaired %s: %+v %v", n, info, err)
		}
		run("compare", "-f", "qcow2", "-F", "raw", overlay(n), filepath.Join(parent, n+".expected"))
	}
	final := read(overlay(names[0]))
	if err := prepare(); err != nil || !bytes.Equal(final, read(overlay(names[0]))) {
		t.Fatalf("repeat: %v", err)
	}
}
