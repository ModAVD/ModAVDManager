package emu

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

func TestBridgeBackingProof(t *testing.T) {
	target := filepath.Join(t.TempDir(), "vendor.img")
	const original = "raw original"
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	disk := backingDisk{overlay: "vendor.img.qcow2", old: "original/vendor.img", target: target, virtualSize: int64(len(original)), stale: true}
	sum := fmt.Sprintf("%x", sha256.Sum256([]byte(original)))
	proof := backingProof{Path: disk.old, Size: disk.virtualSize, SHA256: sum}
	for _, source := range []struct {
		p  backingProof
		ok bool
	}{
		{proof, true}, {backingProof{Path: target, Previous: disk.old, Size: proof.Size, SHA256: sum}, true},
		{backingProof{}, false}, {backingProof{Path: target, Size: proof.Size, SHA256: sum}, false},
		{backingProof{Path: disk.old, Size: proof.Size, SHA256: "broken"}, false},
	} {
		if got := source.p.matchesSource(disk.old); got != source.ok {
			t.Fatalf("matchesSource(%+v) = %v", source.p, got)
		}
	}
	next, err := disk.verify(t.Context(), base, proof)
	if err != nil || next.Path != target || next.Previous != disk.old || next.SHA256 != sum {
		t.Fatalf("moved: %+v, %v", next, err)
	}
	// Even if the ID, change stamp, path and size look current (interrupted repair),
	// a stale header always needs a byte-for-byte content hash of the candidate.
	proof = next
	proof.SHA256 = strings.Repeat("a", 64)
	if _, err = base.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	_, err = disk.verify(t.Context(), base, proof)
	if errs.CodeOf(err) != errs.PrecheckFailed || !strings.Contains(err.Error(), "SHA-256") {
		t.Fatalf("same-size substitute: %v", err)
	}
	disk.virtualSize++
	if _, err = disk.verify(t.Context(), base, next); errs.CodeOf(err) != errs.PrecheckFailed {
		t.Fatalf("virtual size: %v", err)
	}
	disk.virtualSize--
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = disk.verify(ctx, base, next); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("canceled verification: %v", err)
	}
}

func TestBridgeBackingsWithoutMarker(t *testing.T) {
	dir := t.TempDir()
	// The stock boot does not use the offline bridge disks or their record.
	if err := os.WriteFile(filepath.Join(dir, bridgeBackingFile), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	in := &avd.Instance{Dir: dir}
	if err := m.prepareBridgeBackings(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bridgeMarkerName), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.prepareBridgeBackings(t.Context(), in); errs.CodeOf(err) != errs.PrecheckFailed {
		t.Fatalf("corrupt record: %v", err)
	}
}

func TestBridgeBackingEnrollmentAndChangedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "system.img")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	disk := backingDisk{old: path, target: path, overlay: path + ".qcow2"}
	verify := func(proof backingProof) (backingProof, error) {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		return disk.verify(t.Context(), f, proof)
	}
	proof, err := verify(backingProof{})
	if err != nil || proof.Path != path || proof.Size != 8 || proof.SHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte("original"))) {
		t.Fatalf("before first boot: %+v %v", proof, err)
	}
	disk.virtualSize = 8
	if got, err := verify(proof); err != nil || got != proof {
		t.Fatalf("existing disk: %+v %v", got, err)
	}
	replacement := path + ".new"
	if err := os.WriteFile(replacement, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(proof); errs.CodeOf(err) != errs.PrecheckFailed {
		t.Fatalf("different file at recorded path: %v", err)
	}
}

func TestBridgeBaseReadProtectsAgainstWriters(t *testing.T) {
	if !root.CurrentPlatform().Windows() {
		t.Skip("Windows file sharing")
	}
	path := filepath.Join(t.TempDir(), "raw")
	if err := os.WriteFile(path, []byte("raw"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := host.OpenStableRead(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.WriteFile(path, []byte("bad"), 0600); err == nil {
		t.Fatal("could write a base being verified")
	}
	if err := os.Rename(path, path+".moved"); err == nil {
		t.Fatal("could replace a base being verified")
	}
}
