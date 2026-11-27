//go:build windows

package emu

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

// diskTestManager is a Manager on a fresh root (opsTestManager: nothing runs, nothing outside the test directory is
// read) whose host free space is hostFree and whose data disk files take 3 GB each on the host.
func diskTestManager(t *testing.T, hostFree uint64) *Manager {
	t.Helper()
	m, _ := opsTestManager(t)
	savedFree, savedAlloc := hostDiskFree, hostAllocated
	inRoot := func(path string) {
		if !strings.HasPrefix(path, m.Root.Dir) {
			t.Errorf("host asked about %s, outside the test root", path)
		}
	}
	hostDiskFree = func(path string) (uint64, error) { inRoot(path); return hostFree, nil }
	hostAllocated = func(path string) (uint64, error) {
		inRoot(path)
		if filepath.Base(path) == userdataOverlay {
			return 3 << 30, nil
		}
		return 0, errors.New("not there")
	}
	t.Cleanup(func() { hostDiskFree, hostAllocated = savedFree, savedAlloc })
	return m
}

func createFiles(t *testing.T, m *Manager, dataGB *int) int {
	t.Helper()
	created, err := m.Create(context.Background(), CreateRequest{filesOnly: true, DataPartitionGB: dataGB, NoBridge: true})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	return created.Index
}

func configuredGB(t *testing.T, m *Manager, i int) int {
	t.Helper()
	in, err := avd.Get(m.Root, i)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := in.DataPartitionGB()
	return n
}

func diskMode(m *Manager, i int) string {
	s, _ := m.Root.InstanceMeta(i).Get(avd.DataDiskModeKey).Str()
	return s
}

func (m *Manager) mustInfo(t *testing.T, i int) *InstanceInfo {
	t.Helper()
	info, err := m.Info(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestCreateDataDiskModes: no size is 自动扩盘 at 256 GB, a size is manual; info says which and what the disk takes on
// the host.
func TestCreateDataDiskModes(t *testing.T) {
	m := diskTestManager(t, 500<<30)
	auto := createFiles(t, m, nil)
	if n := configuredGB(t, m, auto); n != avd.AutoDataPartitionGB || diskMode(m, auto) != avd.DataDiskAuto {
		t.Fatalf("default create: %d GB, mode %q", n, diskMode(m, auto))
	}
	info := m.mustInfo(t, auto)
	if info.Config.DataDiskMode != avd.DataDiskAuto || info.Config.DataPartitionGB != avd.AutoDataPartitionGB ||
		info.DataSpace.HostBytes != 3<<30 || info.DataSpace.HostFreeBytes != 500<<30 || info.DataSpace.State != "" {
		t.Fatalf("info %+v %+v", info.Config, info.DataSpace)
	}
	n := 48
	manual := createFiles(t, m, &n)
	if got := configuredGB(t, m, manual); got != 48 || diskMode(m, manual) != avd.DataDiskManual {
		t.Fatalf("manual create: %d GB, mode %q", got, diskMode(m, manual))
	}
	if info := m.mustInfo(t, manual); info.Config.DataDiskMode != avd.DataDiskManual {
		t.Fatalf("manual info %+v", info.Config)
	}
}

// TestApplyAutoDisk: an instance from before the modes (no dataDiskMode, 64 GB) is raised to 256 GB before its next
// launch and stays automatic; a manual one, and one at 256 GB, are left alone.
func TestApplyAutoDisk(t *testing.T) {
	m := diskTestManager(t, 500<<30)
	n := 64
	old := createFiles(t, m, &n)
	if err := m.Root.EditInstanceMeta(old, func(meta *jsonord.Obj) error {
		meta.Delete(avd.DataDiskModeKey) // made before the modes
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.applyAutoDisk(old)
	if got := configuredGB(t, m, old); got != avd.AutoDataPartitionGB || diskMode(m, old) != avd.DataDiskAuto {
		t.Fatalf("old instance: %d GB, mode %q", got, diskMode(m, old))
	}
	m.applyAutoDisk(old) // nothing more
	if got := configuredGB(t, m, old); got != avd.AutoDataPartitionGB {
		t.Fatalf("second start: %d GB", got)
	}

	manual := createFiles(t, m, &n)
	m.applyAutoDisk(manual)
	if got := configuredGB(t, m, manual); got != 64 || diskMode(m, manual) != avd.DataDiskManual {
		t.Fatalf("manual grown: %d GB, mode %q", got, diskMode(m, manual))
	}
}

// TestUpdateDataDiskMode: set --data-gb N makes it manual (only larger), set --data-gb auto makes it automatic at
// 256 GB (or keeps a larger size); both at once are refused.
func TestUpdateDataDiskMode(t *testing.T) {
	m := diskTestManager(t, 500<<30)
	n := 32
	i := createFiles(t, m, &n)
	ctx := context.Background()
	up, err := m.Update(ctx, i, UpdateRequest{DataDiskAuto: true})
	if err != nil {
		t.Fatal(err)
	}
	if up.Applied["data_disk_mode"] != avd.DataDiskAuto || up.Applied["data_partition_gb"] != avd.AutoDataPartitionGB ||
		up.Instance.Config.DataDiskMode != avd.DataDiskAuto || configuredGB(t, m, i) != avd.AutoDataPartitionGB {
		t.Fatalf("to auto: %+v", up)
	}
	big := 300
	if up, err = m.Update(ctx, i, UpdateRequest{DataPartitionGB: &big}); err != nil {
		t.Fatal(err)
	}
	if up.Applied["data_disk_mode"] != avd.DataDiskManual || diskMode(m, i) != avd.DataDiskManual || configuredGB(t, m, i) != 300 {
		t.Fatalf("to manual 300: %+v", up)
	}
	small := 64
	if _, err := m.Update(ctx, i, UpdateRequest{DataPartitionGB: &small}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("shrink: %v", err)
	}
	// back to automatic: 300 GB stays
	if _, err := m.Update(ctx, i, UpdateRequest{DataDiskAuto: true}); err != nil || configuredGB(t, m, i) != 300 || diskMode(m, i) != avd.DataDiskAuto {
		t.Fatalf("auto above 256: %v, %d GB, %q", err, configuredGB(t, m, i), diskMode(m, i))
	}
	if _, err := m.Update(ctx, i, UpdateRequest{DataDiskAuto: true, DataPartitionGB: &big}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Fatalf("both: %v", err)
	}
}

// TestDataSpaceHostLow: an automatic disk on a host volume with less than 20 GB free says so; a manual one does not.
func TestDataSpaceHostLow(t *testing.T) {
	m := diskTestManager(t, 10<<30)
	auto := createFiles(t, m, nil)
	if d := m.mustInfo(t, auto).DataSpace; d.State != dataStateHostLow || !strings.Contains(d.Message, "10.0 GB") {
		t.Fatalf("auto %+v", d)
	}
	n := 64
	manual := createFiles(t, m, &n)
	if d := m.mustInfo(t, manual).DataSpace; d.State != "" {
		t.Fatalf("manual %+v", d)
	}
}

// TestRecordDataSpace: a reading belongs to the configured size only once the device has it, a low one shows, and the
// other keys under instances.<i> stay.
func TestRecordDataSpace(t *testing.T) {
	m := diskTestManager(t, 500<<30)
	n := 48
	i := createFiles(t, m, &n)
	if err := m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set("someoneElse", jsonord.NewString("x"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.recordDataSpace(i, dataSize{Device: 48 << 30, FSSize: 48 << 30, FreeBytes: gb, TotalBytes: 47 * gb}, false)
	meta := m.Root.InstanceMeta(i)
	if s, _ := meta.Get("someoneElse").Str(); s != "x" {
		t.Fatal("other key lost")
	}
	if rec, ok := readDataSpace(meta); !ok || rec.SizeGB != 48 || rec.FreeBytes != gb || rec.At == 0 {
		t.Fatalf("reading %+v %v", rec, ok)
	}
	if d := m.mustInfo(t, i).DataSpace; d.State != dataStateLow || !d.Low || !strings.Contains(d.Advice, "--data-gb auto") {
		t.Fatalf("info %+v", d)
	}
	// raised by hand, the device not grown yet: the reading belongs to no size
	big := 96
	if _, err := m.Update(context.Background(), i, UpdateRequest{DataPartitionGB: &big}); err != nil {
		t.Fatal(err)
	}
	m.recordDataSpace(i, dataSize{Device: 48 << 30, FSSize: 48 << 30, FreeBytes: gb, TotalBytes: 47 * gb}, false)
	if rec, _ := readDataSpace(m.Root.InstanceMeta(i)); rec.SizeGB != 0 {
		t.Fatalf("reading of the old size kept as the new: %+v", rec)
	}
	if d := m.mustInfo(t, i).DataSpace; d.State != "" {
		t.Fatalf("low shown for a size not reached yet: %+v", d)
	}
	// statfs missing: nothing recorded over the last reading
	m.recordDataSpace(i, dataSize{Device: 96 << 30, FSSize: 96 << 30}, false)
	if rec, _ := readDataSpace(m.Root.InstanceMeta(i)); rec.FreeBytes != gb {
		t.Fatalf("reading overwritten without statfs: %+v", rec)
	}
}
