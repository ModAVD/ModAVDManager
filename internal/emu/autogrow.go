package emu

// The data disk's two modes (ModAVD/ModAVD#153): 自动扩盘 (automatic, the default) and a size the user sets
// (mas-avd.json instances.<i>.dataDiskMode, avd.DataDiskModeKey).
//
// Automatic: the data partition is avd.AutoDataPartitionGB from the start. The disk image is sparse (qcow2 clusters are
// allocated as the guest writes; 16 -> 32 GB took 0.33 GB of host disk, 64 -> 256 GB about 1 GB, the new block groups'
// inode tables, on 11-11), so the host only pays for what the guest writes and the guest always has room, without
// waiting for a boot. An instance made before the modes (no dataDiskMode) is automatic too: the next start raises its
// data partition to the automatic size before it launches qemu (applyAutoDisk), through avd.Update like
// `set --data-gb`, the emulator grows the disk image at that launch and growData grows the file system after boot.
//
// Manual: the size stays what the user set.
//
// After every boot the after-boot step reads /data's free space (checkDataSpace, next to growData) and records it in
// instances.<i>.dataSpace; status, info and the manager's card say when it is low (with the commands to change the
// size or switch to automatic), and, for an automatic disk, when the host volume runs low.

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"modavd/internal/avd"
	"modavd/internal/host"
	"modavd/internal/jsonord"
)

const (
	// /data is low when less than dataLowBytes or dataLowPercent of it is free.
	dataLowBytes   = int64(5) << 30
	dataLowPercent = 10
	// hostLowBytes: an automatic data disk grows with what the guest writes; below this much free space on its host
	// volume status says so.
	hostLowBytes = int64(20) << 30
	// dataSizeSlack: /data's device is a little smaller than the disk (the metadata encryption's own space); a device
	// smaller than the configured size by more than this has not been grown to it yet.
	dataSizeSlack = int64(1) << 30
)

// dataSpaceKey is instances.<i>.dataSpace: the last after-boot reading (dataSpaceRecord).
const dataSpaceKey = "dataSpace"

// DataSpace states.
const (
	dataStateLow     = "low"      // /data is low
	dataStateHostLow = "host_low" // automatic data disk, little free space on the host volume
)

// hostDiskFree and hostAllocated are host.DiskFree and host.AllocatedFileBytes; tests replace them.
var (
	hostDiskFree  = host.DiskFree
	hostAllocated = host.AllocatedFileBytes
)

// dataDiskFiles are the instance's data disk files on the host: the overlay the guest writes to and its base.
var dataDiskFiles = []string{userdataOverlay, "userdata-qemu.img"}

// dataSpaceRecord is instances.<i>.dataSpace.
type dataSpaceRecord struct {
	FreeBytes, TotalBytes int64 // statfs of /data: available to apps, total
	SizeGB                int   // the configured size the reading belongs to; 0: the disk was not at that size yet
	FSBehind              bool  // the file system is smaller than its device after growData tried
	At                    int64 // Unix seconds
}

// dataLow: /data counts as low (both values in bytes, from statfs).
func dataLow(free, total int64) bool {
	return free < dataLowBytes || free*100 < total*dataLowPercent
}

// manualGrowTarget is the size the advice suggests for `set --data-gb` (up to the manual maximum).
func manualGrowTarget(current int) int {
	return min(avd.MaxDataPartitionGB, max(current+16, current*3/2))
}

// autoDiskTarget is the size the next start gives an automatic data disk now current GB; 0 when it is to stay as it
// is (manual, unknown size, or at the automatic size or above).
func autoDiskTarget(auto bool, current int, currentOK bool) int {
	if !auto || !currentOK || current >= avd.AutoDataPartitionGB {
		return 0
	}
	return avd.AutoDataPartitionGB
}

func readDataSpace(meta *jsonord.Obj) (dataSpaceRecord, bool) {
	o := meta.Get(dataSpaceKey).Obj()
	if o == nil {
		return dataSpaceRecord{}, false
	}
	free, ok1 := o.Get("freeBytes").Int()
	total, ok2 := o.Get("totalBytes").Int()
	size, ok3 := o.Get("sizeGb").Int()
	if !ok1 || !ok2 || !ok3 || total <= 0 || free < 0 {
		return dataSpaceRecord{}, false
	}
	behind, _ := o.Get("fsBehind").BoolVal()
	at, _ := o.Get("at").Int()
	return dataSpaceRecord{FreeBytes: free, TotalBytes: total, SizeGB: int(size), FSBehind: behind, At: at}, true
}

func (m *Manager) writeDataSpace(i int, rec dataSpaceRecord) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		v := jsonord.NewObject()
		o := v.Obj()
		o.Set("freeBytes", jsonord.NewInt(rec.FreeBytes))
		o.Set("totalBytes", jsonord.NewInt(rec.TotalBytes))
		o.Set("sizeGb", jsonord.NewInt(int64(rec.SizeGB)))
		o.Set("fsBehind", jsonord.NewBool(rec.FSBehind))
		o.Set("at", jsonord.NewInt(rec.At))
		meta.Set(dataSpaceKey, v)
		return nil
	})
}

// applyAutoDisk is the step before a launch: an automatic data disk below the automatic size (an instance made before
// the modes) is raised to it now; the emulator grows the disk image at this launch. Only logs; a failure never stops
// the start.
func (m *Manager) applyAutoDisk(i int) {
	defer func() {
		if r := recover(); r != nil {
			m.Log.Warnf("实例 %d 数据盘自动扩盘出错，已跳过: %v", i, r)
		}
	}()
	in, err := avd.Get(m.Root, i)
	if err != nil {
		return
	}
	current, ok := in.DataPartitionGB()
	to := autoDiskTarget(avd.DataDiskAutoMeta(m.Root.InstanceMeta(i)), current, ok)
	if to == 0 {
		return
	}
	if _, err := avd.Update(m.Root, i, avd.UpdateOptions{DataDiskAuto: true}); err != nil {
		m.Log.Warnf("实例 %d 数据盘没能改成自动扩盘（%d GB → %d GB）: %v；可以手动 modavd set %d --data-gb auto", i, current, to, err, i)
		return
	}
	m.Log.Infof("实例 %d 数据盘自动扩盘：开机前从 %d GB 扩到 %d GB（按实际写入占宿主盘）", i, current, to)
}

// checkDataSpace is the after-boot step: grow the file system to its disk (growData), then record /data's free space.
func (m *Manager) checkDataSpace(ctx context.Context, i int) {
	size, ok, behind := m.growData(ctx, i)
	if !ok || ctx.Err() != nil {
		return
	}
	m.recordDataSpace(i, size, behind)
}

// recordDataSpace is checkDataSpace once /data has been read. The size the reading belongs to is the configured one,
// read again (this start may have raised it before the launch), and only when the device has reached it.
func (m *Manager) recordDataSpace(i int, size dataSize, behind bool) {
	if size.TotalBytes <= 0 {
		m.Log.Warnf("实例 %d 读不到数据盘剩余空间", i)
		return
	}
	rec := dataSpaceRecord{FreeBytes: size.FreeBytes, TotalBytes: size.TotalBytes, FSBehind: behind, At: time.Now().Unix()}
	if in, err := avd.Get(m.Root, i); err == nil {
		if gb, ok := in.DataPartitionGB(); ok && size.Device >= int64(gb)<<30-dataSizeSlack {
			rec.SizeGB = gb
		}
	}
	if err := m.writeDataSpace(i, rec); err != nil {
		m.Log.Warnf("实例 %d 记不下数据盘剩余空间: %v", i, err)
	}
	if rec.SizeGB > 0 && dataLow(rec.FreeBytes, rec.TotalBytes) {
		d := dataSpaceInfo(i, m.Root.InstanceMeta(i), rec.SizeGB, -1)
		m.Log.Warnf("实例 %d %s；%s", i, d.Message, d.Advice)
	}
}

// hostDataBytes is what the instance's data disk files take on the host (allocated, sparse holes excluded); -1 when
// that cannot be read.
func hostDataBytes(dir string) int64 {
	var total int64
	found := false
	for _, name := range dataDiskFiles {
		n, err := hostAllocated(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		found = true
		total += int64(min(n, uint64(1)<<62))
	}
	if !found {
		return -1
	}
	return total
}

// DataSpace is InstanceInfo.DataSpace: the data disk's host side and /data's space as the last boot found it.
// Message and Advice are for people (Chinese); Message is "" when there is nothing to tell.
type DataSpace struct {
	// FreeBytes and TotalBytes are /data's statfs after the last boot (0 when never read); CheckedAt is when (RFC 3339,
	// "" never).
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	CheckedAt  string `json:"checked_at"`
	// Low: that reading is under the threshold (less than 5 GB or 10% free) and belongs to the current size.
	Low bool `json:"low"`
	// HostBytes is what the data disk files take on the host now (sparse: only what the guest wrote); -1 unknown.
	HostBytes int64 `json:"host_bytes"`
	// HostFreeBytes is the free space of the host volume they are on; -1 unknown.
	HostFreeBytes int64 `json:"host_free_bytes"`
	// State: "" (nothing to tell), low (/data is low), host_low (automatic data disk and less than 20 GB free on the
	// host volume).
	State   string `json:"state"`
	Message string `json:"message"`
	Advice  string `json:"advice"`
}

// dataSpaceInfo is the DataSpace of instance i from its metadata, configured size (current, 0 when unknown) and host
// volume free space (hostFree, -1 unknown). HostBytes is left to the caller.
func dataSpaceInfo(i int, meta *jsonord.Obj, current int, hostFree int64) DataSpace {
	d := DataSpace{HostBytes: -1, HostFreeBytes: hostFree}
	auto := avd.DataDiskAutoMeta(meta)
	space, spaceOK := readDataSpace(meta)
	if spaceOK {
		d.FreeBytes, d.TotalBytes = space.FreeBytes, space.TotalBytes
		if space.At > 0 {
			d.CheckedAt = time.Unix(space.At, 0).Format(time.RFC3339)
		}
	}
	d.Low = spaceOK && current > 0 && space.SizeGB == current && dataLow(space.FreeBytes, space.TotalBytes)
	bigger := fmt.Sprintf("modavd set %d --data-gb %d", i, manualGrowTarget(current))
	switch {
	case d.Low:
		d.State = dataStateLow
		d.Message = fmt.Sprintf("数据盘只剩 %s（共 %s）", gib(space.FreeBytes), gib(space.TotalBytes))
		switch {
		case space.FSBehind:
			d.Advice = "这台的数据盘是 0.36 之前按 16 GB 及以下建的，文件系统扩不过去；要更大只能新建一台实例再搬游戏"
		case current >= avd.MaxDataPartitionGB:
			d.Advice = "已是最大的数据盘，清理不用的游戏或数据"
		case auto && current >= avd.AutoDataPartitionGB:
			d.Message += fmt.Sprintf("，已到自动扩盘的 %d GB", avd.AutoDataPartitionGB)
			d.Advice = "要更大就改成指定大小：" + bigger + fmt.Sprintf("（最大 %d GB，下次开机生效）", avd.MaxDataPartitionGB)
		case auto:
			d.Advice = fmt.Sprintf("下次开机会自动扩到 %d GB", avd.AutoDataPartitionGB)
		default:
			d.Advice = fmt.Sprintf("改成自动扩盘：modavd set %d --data-gb auto；或者改大：%s（下次开机生效）", i, bigger)
		}
	case auto && hostFree >= 0 && hostFree < hostLowBytes:
		d.State = dataStateHostLow
		d.Message = fmt.Sprintf("宿主盘只剩 %s：自动扩盘的数据盘随写入增长，宿主盘满了游戏会写不进去", gib(hostFree))
		d.Advice = "腾出宿主盘的空间"
	}
	return d
}
