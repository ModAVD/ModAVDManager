//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCompactRealCompressedSkip(t *testing.T) {
	sdk := os.Getenv("MODAVD_TEST_SDK_ROOT")
	if sdk == "" {
		t.Skip("requires existing qemu-img SDK")
	}
	qemuImg := filepath.Join(sdk, "emulator", "qemu-img.exe")
	m := New(managerTestRoot(t, 2), nil)
	// Root metadata, rather than a shared installation, selects the SDK.
	meta, _ := json.Marshal(map[string]any{"sdkRoot": sdk, "adbServerPort": 28391, "scriptAdbServerPort": 28392})
	if err := os.WriteFile(m.Root.MetadataPath(), meta, 0600); err != nil {
		t.Fatal(err)
	}
	in, err := m.instance(2)
	if err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(m.Root.Dir, "compressed-source.raw")
	data := bytes.Repeat([]byte{0x63}, 64<<20)
	if err := os.WriteFile(raw, data, 0600); err != nil {
		t.Fatal(err)
	}
	backing := filepath.Join(in.Dir, userdataBacking)
	overlay := filepath.Join(in.Dir, userdataOverlay)
	qemu := func(args ...string) {
		t.Helper()
		cmd := exec.Command(qemuImg, args...)
		cmd.Dir = in.Dir
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, b)
		}
	}
	qemu("create", "-f", "qcow2", backing, "64M")
	qemu("convert", "-c", "-O", "qcow2", "-B", userdataBacking, "-o", "backing_fmt=qcow2", raw, overlay)
	addCompactGarbage(t, overlay)
	before, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	id := compactFileID(t, overlay)
	res, err := m.DiskCompact(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || res.OnDiskDeltaBytes != 0 || res.FreedBytes != 0 || res.BeforeOnDiskBytes != res.AfterOnDiskBytes {
		t.Fatalf("compressed data must not be expanded for no estimated benefit: %+v", res)
	}
	if after, err := os.ReadFile(overlay); err != nil || !bytes.Equal(before, after) || id != compactFileID(t, overlay) {
		t.Fatalf("skip changed compressed image: %v", err)
	}
	qemu("compare", raw, overlay)
	qemu("check", overlay)
	t.Logf("real compressed skip: %+v", res)
}
