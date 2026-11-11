package emu

// Automatic growth of the data partition (ModAVD/ModAVD#153). After every boot the after-boot step reads /data's free
// space (checkDataSpace, next to growData) and records it in mas-avd.json instances.<i>.dataSpace. When that was low,
// the next start grows the partition by one step before it launches qemu (autoGrowData, from prepareLaunch), through
// avd.Update like `set --data-gb`: the emulator grows the disk image at launch and growData grows the file system after
// boot. The disk image is sparse (qcow2 clusters are allocated as the guest writes): 16 -> 32 GB took 0.33 GB of host
// disk on 11-11, so growing does not take the step from the host right away; the host check below keeps room for the
// guest filling it.
//
// On by default; instances.<i>.dataAutoGrow = false turns it off for one instance (`set <i> --auto-grow off`).
// Best effort and never in the boot's way: every problem is a log line and a record that status shows.

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"modavd/internal/avd"
	"modavd/internal/host"
	"modavd/internal/jsonord"
)

const (
	// autoGrowStepGB is how much one automatic growth adds: a large game update fits, and the host is not asked for
	// much more than that at a time.
	autoGrowStepGB = 16
	// autoGrowMaxGB is where automatic growth stops (manual `set --data-gb` goes up to avd.MaxDataPartitionGB).
	autoGrowMaxGB = 256
	// /data is low when less than autoGrowLowBytes or autoGrowLowPercent of it is free.
	autoGrowLowBytes   = int64(5) << 30
	autoGrowLowPercent = 10
	// autoGrowHostReserve is what the host volume keeps free once the guest has filled the new step completely.
	autoGrowHostReserve = uint64(20) << 30
	// dataSizeSlack: /data's device is a little smaller than the disk (the metadata encryption's own space); a device
	// smaller than the configured size by more than this has not been grown to it yet.
	dataSizeSlack = int64(1) << 30
)

// mas-avd.json instances.<i> keys.
const (
	dataAutoGrowKey = "dataAutoGrow" // false: off; absent or anything else: on
	dataSpaceKey    = "dataSpace"    // the last after-boot reading (dataSpaceRecord)
	dataGrowKey     = "dataGrow"     // what the last start did about a low /data (dataGrowRecord)
)

// Auto-grow states (DataSpace.State, dataGrowRecord.State).
const (
	dataStatePending  = "pending"   // /data is low; the next start grows it
	dataStateGrown    = "grown"     // a start grew it
	dataStateHostLow  = "host_low"  // not grown: the host volume has too little free space
	dataStateAtMax    = "at_max"    // not grown: autoGrowMaxGB reached
	dataStateFSBehind = "fs_behind" // not grown: the file system did not follow the last growth (old layout)
	dataStateFailed   = "failed"    // growing failed (config.ini could not be written)
	dataStateLow      = "low"       // /data is low and automatic growth is off
)

// hostDiskFree is host.DiskFree; tests replace it.
var hostDiskFree = host.DiskFree

// dataSpaceRecord is instances.<i>.dataSpace.
type dataSpaceRecord struct {
	FreeBytes, TotalBytes int64 // statfs of /data: available to apps, total
	SizeGB                int   // the configured size the reading belongs to; 0: the disk was not at that size yet
	FSBehind              bool  // the file system is smaller than its device after growData tried
	At                    int64 // Unix seconds
}

// dataGrowRecord is instances.<i>.dataGrow.
type dataGrowRecord struct {
	State         string
	FromGB, ToGB  int
	HostFreeBytes int64 // when State is host_low; -1: could not be read
	Error         string
	At            int64
}

// dataLow: /data counts as low (both values in bytes, from statfs).
func dataLow(free, total int64) bool {
	return free < autoGrowLowBytes || free*100 < total*autoGrowLowPercent
}

// autoGrowTarget is the size the next automatic growth goes to from current GB.
func autoGrowTarget(current int) int {
	return min(autoGrowMaxGB, current+autoGrowStepGB)
}

// manualGrowTarget is the size the advice suggests for `set --data-gb` (up to the manual maximum).
func manualGrowTarget(current int) int {
	return min(avd.MaxDataPartitionGB, current+autoGrowStepGB)
}

// growPlan is what a start does about the data partition.
type growPlan struct {
	State    string // "": nothing to do; dataStateGrown: grow to To; otherwise why not (a dataState*)
	From, To int
	HostFree int64 // read when the host was checked; -1 when it could not be read
	HostNeed uint64
}

// planAutoGrow is the pure decision of autoGrowData. on: dataAutoGrow; space/spaceOK: the last reading; current: the
// configured size in GB (currentOK false when config.ini has none that can be read); hostFree reads the host volume's
// free bytes, asked only when everything else says grow.
func planAutoGrow(on bool, space dataSpaceRecord, spaceOK bool, current int, currentOK bool,
	hostFree func() (uint64, error)) growPlan {
	// a reading of another size (the disk was raised since) or none says nothing about this one
	if !spaceOK || !currentOK || space.SizeGB != current || !dataLow(space.FreeBytes, space.TotalBytes) {
		return growPlan{}
	}
	p := growPlan{From: current, HostFree: -1}
	switch {
	case !on:
		p.State = dataStateLow
	case space.FSBehind:
		p.State = dataStateFSBehind
	case current >= autoGrowMaxGB:
		p.State = dataStateAtMax
	default:
		p.To = autoGrowTarget(current)
		p.HostNeed = uint64(p.To-current)<<30 + autoGrowHostReserve
		free, err := hostFree()
		if err != nil {
			p.State = dataStateHostLow
			return p
		}
		p.HostFree = int64(min(free, uint64(1)<<62))
		if free < p.HostNeed {
			p.State = dataStateHostLow
			return p
		}
		p.State = dataStateGrown
	}
	return p
}

// autoGrowOn: instances.<i>.dataAutoGrow is not false.
func autoGrowOn(meta *jsonord.Obj) bool {
	b, ok := meta.Get(dataAutoGrowKey).BoolVal()
	return !ok || b
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

func readDataGrow(meta *jsonord.Obj) (dataGrowRecord, bool) {
	o := meta.Get(dataGrowKey).Obj()
	if o == nil {
		return dataGrowRecord{}, false
	}
	state, ok := o.Get("state").Str()
	if !ok || state == "" {
		return dataGrowRecord{}, false
	}
	from, _ := o.Get("fromGb").Int()
	to, _ := o.Get("toGb").Int()
	hostFree, ok := o.Get("hostFreeBytes").Int()
	if !ok {
		hostFree = -1
	}
	msg, _ := o.Get("error").Str()
	at, _ := o.Get("at").Int()
	return dataGrowRecord{State: state, FromGB: int(from), ToGB: int(to), HostFreeBytes: hostFree, Error: msg, At: at}, true
}

func (m *Manager) setDataAutoGrow(i int, on bool) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(dataAutoGrowKey, jsonord.NewBool(on))
		return nil
	})
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

// writeDataGrow records rec, or removes the record when rec.State is "".
func (m *Manager) writeDataGrow(i int, rec dataGrowRecord) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		if rec.State == "" {
			meta.Delete(dataGrowKey)
			return nil
		}
		v := jsonord.NewObject()
		o := v.Obj()
		o.Set("state", jsonord.NewString(rec.State))
		o.Set("fromGb", jsonord.NewInt(int64(rec.FromGB)))
		if rec.ToGB > 0 {
			o.Set("toGb", jsonord.NewInt(int64(rec.ToGB)))
		}
		if rec.HostFreeBytes >= 0 {
			o.Set("hostFreeBytes", jsonord.NewInt(rec.HostFreeBytes))
		}
		if rec.Error != "" {
			o.Set("error", jsonord.NewString(rec.Error))
		}
		o.Set("at", jsonord.NewInt(rec.At))
		meta.Set(dataGrowKey, v)
		return nil
	})
}

// autoGrowData is the step before a launch: grow the data partition when the last boot found /data low (planAutoGrow).
// Only logs and records; a failure never stops the start.
func (m *Manager) autoGrowData(i int) {
	defer func() {
		if r := recover(); r != nil {
			m.Log.Warnf("实例 %d 数据盘自动扩容出错，已跳过: %v", i, r)
		}
	}()
	in, err := avd.Get(m.Root, i)
	if err != nil {
		return
	}
	meta := m.Root.InstanceMeta(i)
	space, spaceOK := readDataSpace(meta)
	current, currentOK := in.DataPartitionGB()
	plan := planAutoGrow(autoGrowOn(meta), space, spaceOK, current, currentOK, func() (uint64, error) {
		return hostDiskFree(in.Dir)
	})
	rec := dataGrowRecord{State: plan.State, FromGB: plan.From, HostFreeBytes: -1, At: time.Now().Unix()}
	free := gib(space.FreeBytes)
	switch plan.State {
	case "", dataStateLow:
		// nothing (more) to do; an old record of another size says nothing any more. Low with growth off: status
		// says so from the reading
		if _, had := readDataGrow(meta); had {
			rec.State = ""
			if err := m.writeDataGrow(i, rec); err != nil {
				m.Log.Debugf("实例 %d 清不掉上次的自动扩容记录: %v", i, err)
			}
		}
		return
	case dataStateGrown:
		to := plan.To
		if _, err := avd.Update(m.Root, i, avd.UpdateOptions{DataPartitionGB: &to}); err != nil {
			rec.State, rec.ToGB, rec.Error = dataStateFailed, to, err.Error()
			m.Log.Warnf("实例 %d 数据盘剩余 %s，自动扩容没做成: %v；可以手动 modavd set %d --data-gb %d", i, free, err, i, to)
		} else {
			rec.ToGB = to
			m.Log.Infof("实例 %d 数据盘上次开机只剩 %s，开机前自动从 %d GB 扩到 %d GB（宿主盘剩余 %s）", i, free,
				plan.From, to, gib(plan.HostFree))
		}
	case dataStateHostLow:
		rec.ToGB, rec.HostFreeBytes = plan.To, plan.HostFree
		if plan.HostFree < 0 {
			m.Log.Warnf("实例 %d 数据盘剩余 %s，读不到宿主盘剩余空间，这次不自动扩容", i, free)
		} else {
			m.Log.Warnf("实例 %d 数据盘剩余 %s，宿主盘只剩 %s，扩 %d GB 要留够 %s，这次不自动扩容", i, free,
				gib(plan.HostFree), plan.To-plan.From, gib(int64(plan.HostNeed)))
		}
	case dataStateAtMax:
		m.Log.Warnf("实例 %d 数据盘剩余 %s，已到自动扩容上限 %d GB，不再自动扩", i, free, autoGrowMaxGB)
	case dataStateFSBehind:
		m.Log.Warnf("实例 %d 数据盘剩余 %s，文件系统没跟上磁盘大小，不自动扩容", i, free)
	}
	if err := m.writeDataGrow(i, rec); err != nil {
		m.Log.Warnf("实例 %d 记不下这次自动扩容的结果: %v", i, err)
	}
}

// checkDataSpace is the after-boot step: grow the file system to its disk (growData), then record /data's free space
// for the next start's autoGrowData. The size the reading belongs to is the configured one, read again (this start
// may have raised it before the launch), and only when the device has reached it.
func (m *Manager) checkDataSpace(ctx context.Context, i int) {
	size, ok, behind := m.growData(ctx, i)
	if !ok || ctx.Err() != nil {
		return
	}
	m.recordDataSpace(i, size, behind)
}

// recordDataSpace is checkDataSpace once /data has been read.
func (m *Manager) recordDataSpace(i int, size dataSize, behind bool) {
	if size.TotalBytes <= 0 {
		m.Log.Warnf("实例 %d 读不到数据盘剩余空间，没检查要不要自动扩容", i)
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
	if !dataLow(rec.FreeBytes, rec.TotalBytes) || rec.SizeGB == 0 {
		return
	}
	meta := m.Root.InstanceMeta(i)
	switch {
	case !autoGrowOn(meta):
		m.Log.Warnf("实例 %d 数据盘只剩 %s（共 %s），自动扩容已关；要扩容: modavd set %d --data-gb %d", i,
			gib(rec.FreeBytes), gib(rec.TotalBytes), i, manualGrowTarget(rec.SizeGB))
	case rec.SizeGB < autoGrowMaxGB && !behind:
		m.Log.Infof("实例 %d 数据盘只剩 %s（共 %s），下次开机前自动扩到 %d GB", i, gib(rec.FreeBytes),
			gib(rec.TotalBytes), autoGrowTarget(rec.SizeGB))
	}
}

// DataSpace is InstanceInfo.DataSpace: /data's space as the last boot found it and what automatic growth does about
// it. Message and Advice are for people (Chinese); Message is "" when there is nothing to tell.
type DataSpace struct {
	// AutoGrow: automatic growth is on for this instance (mas-avd.json instances.<i>.dataAutoGrow is not false).
	AutoGrow bool `json:"auto_grow"`
	// FreeBytes and TotalBytes are /data's statfs after the last boot (0 when never read); CheckedAt is when (RFC 3339,
	// "" never).
	FreeBytes  int64  `json:"free_bytes"`
	TotalBytes int64  `json:"total_bytes"`
	CheckedAt  string `json:"checked_at"`
	// Low: that reading is under the threshold (less than 5 GB or 10% free) and the disk has not been grown since.
	Low bool `json:"low"`
	// State: "" (nothing to tell), pending (the next start grows it to ToGB), grown (a start grew it FromGB -> ToGB),
	// host_low (not grown: too little free space on the host volume), at_max (not grown: 256 GB reached), fs_behind
	// (not grown: the file system did not follow an earlier growth), failed (growing failed), low (low and
	// automatic growth is off).
	State   string `json:"state"`
	FromGB  int    `json:"from_gb"`
	ToGB    int    `json:"to_gb"`
	Message string `json:"message"`
	Advice  string `json:"advice"`
}

// dataSpaceInfo is the DataSpace of instance i from its metadata and configured size (current, 0 when unknown).
func dataSpaceInfo(i int, meta *jsonord.Obj, current int) DataSpace {
	d := DataSpace{AutoGrow: autoGrowOn(meta)}
	space, spaceOK := readDataSpace(meta)
	if spaceOK {
		d.FreeBytes, d.TotalBytes = space.FreeBytes, space.TotalBytes
		if space.At > 0 {
			d.CheckedAt = time.Unix(space.At, 0).Format(time.RFC3339)
		}
	}
	grow, growOK := readDataGrow(meta)
	d.Low = spaceOK && current > 0 && space.SizeGB == current && dataLow(space.FreeBytes, space.TotalBytes)
	free := gib(space.FreeBytes)
	manual := fmt.Sprintf("modavd set %d --data-gb %d", i, manualGrowTarget(current))
	switch {
	case d.Low && !d.AutoGrow:
		d.State = dataStateLow
		d.Message = fmt.Sprintf("数据盘只剩 %s（共 %s），自动扩容已关", free, gib(space.TotalBytes))
		d.Advice = fmt.Sprintf("手动扩容：%s（下次开机生效），或打开自动扩容：modavd set %d --auto-grow on", manual, i)
	case d.Low && growOK && grow.FromGB == current && grow.State != dataStateGrown:
		d.State, d.FromGB, d.ToGB = grow.State, grow.FromGB, grow.ToGB
		switch grow.State {
		case dataStateHostLow:
			if grow.HostFreeBytes < 0 {
				d.Message = fmt.Sprintf("数据盘只剩 %s，读不到宿主盘剩余空间，没有自动扩容", free)
			} else {
				d.Message = fmt.Sprintf("数据盘只剩 %s，宿主盘只剩 %s，没有自动扩容（扩 %d GB 要宿主盘留够 %s）", free,
					gib(grow.HostFreeBytes), grow.ToGB-grow.FromGB, gib(int64(uint64(grow.ToGB-grow.FromGB)<<30+autoGrowHostReserve)))
			}
			d.Advice = "腾出宿主盘空间后，下次开机会再自动扩；也可以手动：" + manual
		case dataStateAtMax:
			d.Message = fmt.Sprintf("数据盘只剩 %s，已到自动扩容上限 %d GB，不再自动扩", free, autoGrowMaxGB)
			d.Advice = "要更大就手动扩容：" + manual + "（最大 " + strconv.Itoa(avd.MaxDataPartitionGB) + " GB）"
			if current >= avd.MaxDataPartitionGB {
				d.Advice = "已是最大的数据盘，清理不用的游戏或数据"
			}
		case dataStateFSBehind:
			d.Message = fmt.Sprintf("数据盘只剩 %s，文件系统没能跟着磁盘扩大，没有自动扩容", free)
			d.Advice = "0.36 之前按 16 GB 及以下建的数据盘扩不过去，要更大只能新建一台实例再搬游戏"
		default:
			d.Message = fmt.Sprintf("数据盘只剩 %s，自动扩容没做成", free)
			if grow.Error != "" {
				d.Message += "：" + grow.Error
			}
			d.Advice = "手动扩容：" + manual + "（下次开机生效）"
		}
	case d.Low && space.FSBehind:
		d.State = dataStateFSBehind
		d.Message = fmt.Sprintf("数据盘只剩 %s，文件系统没能跟着磁盘扩大，不会自动扩容", free)
		d.Advice = "0.36 之前按 16 GB 及以下建的数据盘扩不过去，要更大只能新建一台实例再搬游戏"
	case d.Low && current >= autoGrowMaxGB:
		d.State = dataStateAtMax
		d.Message = fmt.Sprintf("数据盘只剩 %s，已到自动扩容上限 %d GB，不会再自动扩", free, autoGrowMaxGB)
		d.Advice = "要更大就手动扩容：" + manual + "（最大 " + strconv.Itoa(avd.MaxDataPartitionGB) + " GB）"
		if current >= avd.MaxDataPartitionGB {
			d.Advice = "已是最大的数据盘，清理不用的游戏或数据"
		}
	case d.Low:
		d.State, d.FromGB, d.ToGB = dataStatePending, current, autoGrowTarget(current)
		d.Message = fmt.Sprintf("数据盘只剩 %s（共 %s），下次开机前自动扩到 %d GB", free, gib(space.TotalBytes), d.ToGB)
		d.Advice = fmt.Sprintf("不想自动扩：modavd set %d --auto-grow off", i)
	case growOK && grow.State == dataStateGrown && grow.ToGB == current:
		d.State, d.FromGB, d.ToGB = dataStateGrown, grow.FromGB, grow.ToGB
		d.Message = fmt.Sprintf("数据盘快满了，开机前已自动从 %d GB 扩到 %d GB", grow.FromGB, grow.ToGB)
		d.Advice = fmt.Sprintf("不想自动扩：modavd set %d --auto-grow off", i)
	}
	return d
}
