//go:build windows

package emu

import (
	"context"
	"errors"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/jsonord"
)

// autoGrowTestInstance makes instance files on a fresh root (opsTestManager: nothing runs, nothing outside the test
// directory is read) with a data partition of dataGB, and replaces the host's free space with hostFree.
func autoGrowTestInstance(t *testing.T, dataGB int, hostFree func(string) (uint64, error)) (*Manager, int) {
	t.Helper()
	m, _ := opsTestManager(t)
	created, err := m.Create(context.Background(), CreateRequest{filesOnly: true, DataPartitionGB: &dataGB, NoBridge: true})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	saved := hostDiskFree
	hostDiskFree = func(path string) (uint64, error) {
		if !strings.HasPrefix(path, m.Root.Dir) {
			t.Errorf("host free space asked for %s, outside the test root", path)
		}
		return hostFree(path)
	}
	t.Cleanup(func() { hostDiskFree = saved })
	return m, created.Index
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

func growState(m *Manager, i int) dataGrowRecord {
	rec, _ := readDataGrow(m.Root.InstanceMeta(i))
	return rec
}

func freeHost(n uint64) func(string) (uint64, error) {
	return func(string) (uint64, error) { return n, nil }
}

// TestAutoGrowData: a low reading grows the data partition one step through avd.Update; the grown size then has no
// reading of its own, so the next start leaves it (and drops the record); host too full, off, at the limit: not grown.
func TestAutoGrowData(t *testing.T) {
	m, i := autoGrowTestInstance(t, 16, freeHost(500<<30))
	// what the after-boot step records: 16 GB disk, 1.2 GB free
	m.recordDataSpace(i, dataSize{Device: 16 << 30, FSSize: 16 << 30, FreeBytes: 1288490188, TotalBytes: 16 * gb}, false)
	if d := m.mustInfo(t, i).DataSpace; d.State != dataStatePending || d.ToGB != 32 || !d.Low || !d.AutoGrow {
		t.Fatalf("after a low reading: %+v", d)
	}
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 32 {
		t.Fatalf("not grown: %d GB", n)
	}
	if rec := growState(m, i); rec.State != dataStateGrown || rec.FromGB != 16 || rec.ToGB != 32 {
		t.Fatalf("record %+v", rec)
	}
	if d := m.mustInfo(t, i).DataSpace; d.State != dataStateGrown || d.FromGB != 16 || d.ToGB != 32 {
		t.Fatalf("info after growing: %+v", d)
	}
	// a second start before any boot read the new size: nothing more
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 32 {
		t.Fatalf("grown twice: %d GB", n)
	}
	if rec := growState(m, i); rec.State != "" {
		t.Fatalf("stale record kept: %+v", rec)
	}

	// the boot after a manual raise (device not grown yet): the reading belongs to no size, no growth
	m.recordDataSpace(i, dataSize{Device: 16 << 30, FSSize: 16 << 30, FreeBytes: gb, TotalBytes: 16 * gb}, false)
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 32 {
		t.Fatalf("grew on a reading of the old size: %d GB", n)
	}
}

func TestAutoGrowDataHostLow(t *testing.T) {
	need := uint64(autoGrowStepGB)<<30 + autoGrowHostReserve
	m, i := autoGrowTestInstance(t, 48, freeHost(need-1))
	m.recordDataSpace(i, dataSize{Device: 48 << 30, FSSize: 48 << 30, FreeBytes: 2 * gb, TotalBytes: 47 * gb}, false)
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 48 {
		t.Fatalf("grown with too little host space: %d GB", n)
	}
	rec := growState(m, i)
	if rec.State != dataStateHostLow || rec.HostFreeBytes != int64(need-1) {
		t.Fatalf("record %+v", rec)
	}
	d := m.mustInfo(t, i).DataSpace
	if d.State != dataStateHostLow || !strings.Contains(d.Advice, "modavd set") {
		t.Fatalf("info %+v", d)
	}

	// unreadable host: not grown either
	hostDiskFree = func(string) (uint64, error) { return 0, errors.New("no") }
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 48 || growState(m, i).State != dataStateHostLow {
		t.Fatalf("unreadable host: %d GB, %+v", n, growState(m, i))
	}
}

// TestAutoGrowOff: `set --auto-grow off` is stored as false, and a low disk then stays as it is; on grows again.
func TestAutoGrowOff(t *testing.T) {
	m, i := autoGrowTestInstance(t, 16, freeHost(500<<30))
	off, on := false, true
	up, err := m.Update(context.Background(), i, UpdateRequest{DataAutoGrow: &off})
	if err != nil {
		t.Fatal(err)
	}
	if up.Applied["data_auto_grow"] != false || up.Instance.DataSpace.AutoGrow || up.AppliesNextBoot {
		t.Fatalf("update %+v", up)
	}
	if v, ok := m.Root.InstanceMeta(i).Get(dataAutoGrowKey).BoolVal(); !ok || v {
		t.Fatalf("dataAutoGrow not stored as false")
	}
	m.recordDataSpace(i, dataSize{Device: 16 << 30, FSSize: 16 << 30, FreeBytes: gb, TotalBytes: 16 * gb}, false)
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 16 {
		t.Fatalf("grown while off: %d GB", n)
	}
	if d := m.mustInfo(t, i).DataSpace; d.State != dataStateLow || !strings.Contains(d.Advice, "--data-gb 32") {
		t.Fatalf("info while off: %+v", d)
	}
	if _, err := m.Update(context.Background(), i, UpdateRequest{DataAutoGrow: &on}); err != nil {
		t.Fatal(err)
	}
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 32 {
		t.Fatalf("not grown after turning it on: %d GB", n)
	}
}

func TestAutoGrowAtMax(t *testing.T) {
	m, i := autoGrowTestInstance(t, autoGrowMaxGB, freeHost(1<<40))
	m.recordDataSpace(i, dataSize{Device: autoGrowMaxGB << 30, FSSize: autoGrowMaxGB << 30, FreeBytes: gb, TotalBytes: 250 * gb}, false)
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != autoGrowMaxGB {
		t.Fatalf("grown past the limit: %d GB", n)
	}
	if rec := growState(m, i); rec.State != dataStateAtMax {
		t.Fatalf("record %+v", rec)
	}
}

func TestAutoGrowFSBehind(t *testing.T) {
	m, i := autoGrowTestInstance(t, 32, freeHost(1<<40))
	m.recordDataSpace(i, dataSize{Device: 32 << 30, FSSize: 30 << 30, FreeBytes: gb, TotalBytes: 30 * gb}, true)
	m.autoGrowData(i)
	if n := configuredGB(t, m, i); n != 32 || growState(m, i).State != dataStateFSBehind {
		t.Fatalf("%d GB, %+v", n, growState(m, i))
	}
}

// TestRecordDataSpaceKeepsOtherKeys: the readings go under instances.<i> next to what AUTO-MAS and others wrote.
func TestRecordDataSpaceKeepsOtherKeys(t *testing.T) {
	m, i := autoGrowTestInstance(t, 16, freeHost(1<<40))
	if err := m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set("someoneElse", jsonord.NewString("x"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.recordDataSpace(i, dataSize{Device: 16 << 30, FSSize: 16 << 30, FreeBytes: 10 * gb, TotalBytes: 16 * gb}, false)
	meta := m.Root.InstanceMeta(i)
	if s, _ := meta.Get("someoneElse").Str(); s != "x" {
		t.Fatal("other key lost")
	}
	if rec, ok := readDataSpace(meta); !ok || rec.SizeGB != 16 || rec.FreeBytes != 10*gb || rec.At == 0 {
		t.Fatalf("reading %+v %v", rec, ok)
	}
	// statfs missing: nothing recorded over the last reading
	m.recordDataSpace(i, dataSize{Device: 16 << 30, FSSize: 16 << 30}, false)
	if rec, _ := readDataSpace(m.Root.InstanceMeta(i)); rec.FreeBytes != 10*gb {
		t.Fatalf("reading overwritten without statfs: %+v", rec)
	}
}

func (m *Manager) mustInfo(t *testing.T, i int) *InstanceInfo {
	t.Helper()
	info, err := m.Info(context.Background(), i)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
