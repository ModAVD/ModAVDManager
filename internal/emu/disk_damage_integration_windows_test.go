//go:build windows

package emu

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
)

// Run with MODAVD_TEST_SDK_ROOT pointing to an existing SDK. Corrupt only a
// throwaway image's refcount, then exercise the real read-only diagnostic path.
func TestDiskDamagedAdviceIntegration(t *testing.T) {
	sdk := os.Getenv("MODAVD_TEST_SDK_ROOT")
	if sdk == "" {
		t.Skip("set MODAVD_TEST_SDK_ROOT to an existing SDK")
	}
	r := managerTestRoot(t, 3)
	if err := os.WriteFile(filepath.Join(r.Dir, root.MetadataFile), []byte(`{"sdkRoot":`+strconv.Quote(sdk)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(sdk, "emulator", "qemu-img.exe")
	dir := avd.Dir(r, 3)
	for _, args := range [][]string{
		{"create", "-f", "qcow2", userdataBacking, "16M"},
		{"create", "-f", "qcow2", "-b", userdataBacking, "-F", "qcow2", userdataOverlay},
	} {
		cmd := exec.Command(exe, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	image := filepath.Join(dir, userdataOverlay)
	b, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	table := binary.BigEndian.Uint64(b[48:56])
	block := binary.BigEndian.Uint64(b[table : table+8])
	// Cluster zero (the header) is referenced but its refcount now claims it is free.
	b[block], b[block+1] = 0, 0
	if err := os.WriteFile(image, b, 0o644); err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	info, err := m.DiskInfo(context.Background(), 3)
	if err != nil || info.AllocatedBytes != nil || info.ReclaimableBytes != nil || !strings.Contains(strings.Join(info.Warnings, " "), "不会修复") {
		t.Fatalf("damage info: %+v %v", info, err)
	}
	_, err = m.DiskCompact(context.Background(), 3)
	if errs.CodeOf(err) != errs.Unsupported || !strings.Contains(errs.As(err).Advice, "先备份") || !strings.Contains(errs.As(err).Advice, "modavd create") {
		t.Fatalf("damage advice: %v", err)
	}
	after, err := os.ReadFile(image)
	if err != nil || !bytes.Equal(b, after) {
		t.Fatal("diagnosis changed the damaged original")
	}
	unlock, err := m.lockInstanceForCompact(3)
	if err != nil {
		t.Fatalf("damage refusal leaked locks: %v", err)
	}
	unlock()
}
