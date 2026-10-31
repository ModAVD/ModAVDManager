//go:build windows

package emu

// Create / update / delete. Port of AUTO-MAS manager.py create_instance / _free_index / write_instance_settings /
// delete_instance; the files themselves are written by package avd.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

// Create makes a new instance (validated options, free index and ports), sets it up with one headless boot
// (createinit.go) and returns it, stopped. req.Resume runs that setup again on an instance whose setup failed.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (*InstanceInfo, error) {
	if req.Resume {
		return m.resumeCreate(ctx, req)
	}
	memory, cpu, dataGB, err := avd.ValidateOptions(req.MemoryMB, req.CPU, req.DataPartitionGB)
	if err != nil {
		return nil, err
	}
	fps, err := avd.ValidateFPS(req.FPS)
	if err != nil {
		return nil, err
	}
	// A root used by modavd alone has no mas-avd.json yet (AUTO-MAS writes it when the root is added).
	if _, err := m.Root.EnsureMetadata(); err != nil {
		return nil, err
	}
	insts, err := avd.List(m.Root)
	if err != nil {
		return nil, err
	}
	existing := map[int]bool{}
	for _, in := range insts {
		existing[in.Index] = true
	}

	var i int
	if req.Index != nil {
		i = *req.Index
		if !root.ValidIndex(i) {
			return nil, errs.Newf(errs.InvalidArgument, "实例编号 %d 不可用（只能是 0–9，5 除外）", i)
		}
		if existing[i] {
			if err := m.notCreated(i, "不能再新建"); err != nil {
				return nil, err
			}
			return nil, errs.Newf(errs.AlreadyExists, "实例 %s 已存在", root.AvdName(i))
		}
		// Leftovers of a delete or create that stopped half-way: not listed, but avd.Create refuses the index. Say
		// what is there and how to clear it (Delete takes such leftovers) rather than a bare "already exists".
		if left := instanceFilesOnDisk(m.Root, i); len(left) > 0 {
			return nil, errs.Newf(errs.AlreadyExists, "实例 %s 已存在，但不完整（只剩：%s），可能是上次删除或新建到一半",
				root.AvdName(i), strings.Join(left, "、")).
				WithAdvice(fmt.Sprintf("先用 modavd delete %d --yes 清掉这些残留再新建，或换一个编号", i))
		}
	} else if i, err = m.freeIndex(existing); err != nil {
		return nil, err
	}

	// Held from before the files exist until the setup is done: nobody else starts, changes or deletes the instance
	// meanwhile, and status tells "creating" from a create that was killed (createinit.go).
	lock, err := m.tryInstanceLock(i, "create", "正在新建")
	if err != nil {
		return nil, err
	}
	in, err := avd.Create(m.Root, i, avd.CreateOptions{
		Title: req.Title, MemoryMB: memory, CPU: cpu, DataPartitionGB: dataGB, FPS: fps,
	})
	if err != nil {
		lock.Unlock()
		return nil, err
	}
	m.Log.Infof("已新建魔改 AVD 实例 %d（内存 %d MB / %d 核 / 数据盘 %d GB / 帧率 %d）", i, memory, cpu, dataGB, fps)
	if req.NoBridge {
		// recorded, so no start installs it (bridge.go)
		if err := m.setBridgeWanted(i, false); err != nil {
			m.Log.Warnf("实例 %d 没记下「不装执行桥」，开机时还是会装: %v", i, err)
		}
	} else if err := writeBridgeMarker(m.Root, i); err != nil {
		// first boot writable; the setup below (or, for filesOnly, any start) installs the bridge (bridge.go)
		m.Log.Warnf("实例 %d 没放上执行桥的开机标记: %v", i, err)
	}
	if req.filesOnly {
		lock.Unlock()
		info := m.instanceInfo(ctx, in, nil, nil)
		return &info, nil
	}
	return m.setupNew(ctx, i, req, lock)
}

// FreeIndex is the index Create picks when none is given (the manager names its create job after it).
func (m *Manager) FreeIndex() (int, error) {
	insts, err := avd.List(m.Root)
	if err != nil {
		return 0, err
	}
	existing := map[int]bool{}
	for _, in := range insts {
		existing[in.Index] = true
	}
	return m.freeIndex(existing)
}

// freeIndex is the smallest index that is not in existing, has no leftovers and whose ports are free.
func (m *Manager) freeIndex(existing map[int]bool) (int, error) {
	// Leftovers of a half-deleted instance (an .ini or a directory without the other) do not count as instances, but
	// avd.Create refuses their index: skip them here instead of failing with "already exists" (Delete clears them).
	for k := 0; k <= root.MaxIndex; k++ {
		if len(instanceFilesOnDisk(m.Root, k)) > 0 {
			existing[k] = true
		}
	}
	var busyErr error
	i, err := avd.FreeIndex(existing, func(ports []int) []int {
		busy, err := win.BusyPorts(ports)
		if err != nil {
			busyErr = err
			return ports
		}
		return busy
	})
	if err != nil {
		if busyErr != nil {
			return 0, errs.Wrap(errs.Internal, busyErr, "读不了端口占用情况: "+busyErr.Error())
		}
		return 0, err
	}
	return i, nil
}

// resumeCreate runs the setup again on an instance whose setup failed (or was killed); its bridge setting is the one
// recorded at create.
func (m *Manager) resumeCreate(ctx context.Context, req CreateRequest) (*InstanceInfo, error) {
	if req.Index == nil {
		return nil, errs.New(errs.Usage, "--resume 要给实例编号")
	}
	i := *req.Index
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	lock, err := m.tryInstanceLock(i, "create", "正在新建")
	if err != nil {
		return nil, err
	}
	// with the lock in hand, a recorded "creating" is one that was killed (creation() would see our own lock)
	if state := m.creationRecorded(i); state != creationFailed && state != creationCreating {
		lock.Unlock()
		return nil, errs.Newf(errs.Unsupported, "魔改 AVD 实例 %d 不是新建失败的实例，没有要重做的初始化", i).
			WithAdvice(fmt.Sprintf("直接 modavd start %d", i))
	}
	if _, running := m.findQemu(i); running {
		lock.Unlock()
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 的模拟器还开着", i).
			WithAdvice(fmt.Sprintf("先 modavd stop %d，再重试", i))
	}
	return m.setupNew(ctx, i, req, lock)
}

// Update changes title / memory / cpu / fps / data partition size (next boot).
func (m *Manager) Update(ctx context.Context, i int, req UpdateRequest) (*UpdateResult, error) {
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	if err := m.notCreated(i, "不能修改"); err != nil {
		return nil, err
	}
	applied, err := avd.Update(m.Root, i, avd.UpdateOptions{
		Title: req.Title, MemoryMB: req.MemoryMB, CPU: req.CPU, FPS: req.FPS, DataPartitionGB: req.DataPartitionGB,
	})
	if err != nil {
		return nil, err
	}
	if applied == nil {
		applied = map[string]any{}
	}
	m.Log.Infof("已写入魔改 AVD 实例 %d 的设置: %s", i, formatApplied(applied))
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	var p *win.Process
	if q, ok := m.qemuOf(i); ok {
		p = &q
		m.Log.Infof("魔改 AVD 实例 %d 正在运行，改动下次开机生效", i)
	}
	return &UpdateResult{Instance: m.instanceInfo(ctx, in, p, nil), Applied: applied, AppliesNextBoot: p != nil}, nil
}

// Delete removes a stopped instance with all its data. errs.Busy while it runs or is being started / stopped.
//
// The start and stop locks are held during the delete: AUTO-MAS only looks at the status, so a delete could slip in
// while an open has not started qemu yet (status still offline).
//
// An incomplete instance is deleted too, like AUTO-MAS's delete_instance lets NOT_FOUND through to
// delete_instance_files: os.RemoveAll goes on past a file it cannot delete (a data disk held by a virus scanner), so a
// failed delete leaves mas_<i>.ini with a directory that lost its config.ini, and a create killed half-way leaves a
// directory without the .ini. Neither is listed, auto-create skips the index, and an explicit create refuses it;
// without this the retry the error advises would end in "不存在". errs.NotFound only when none of mas_<i>.ini,
// mas_<i>.avd and instances.<i> is there.
func (m *Manager) Delete(ctx context.Context, i int) (*DeleteResult, error) {
	in, err := m.instance(i)
	var leftovers []string
	if errs.CodeOf(err) == errs.NotFound {
		if leftovers = instanceLeftovers(m.Root, i); len(leftovers) == 0 {
			return nil, err
		}
		in = &avd.Instance{Index: i, Name: root.AvdName(i), Dir: avd.Dir(m.Root, i)}
	} else if err != nil {
		return nil, err
	}
	startLock, err := m.tryInstanceLock(i, "start", "正在启动")
	if err != nil {
		return nil, err
	}
	stopLock, err := m.tryInstanceLock(i, "stop", "正在关闭")
	if err != nil {
		startLock.Unlock()
		return nil, err
	}
	bridgeLock, err := m.tryInstanceLock(i, "bridge", "正在装卸执行桥")
	if err != nil {
		startLock.Unlock()
		stopLock.Unlock()
		return nil, err
	}
	// a create still setting the instance up (between its boots no other lock is held); a failed one may go
	createLock, err := m.tryInstanceLock(i, "create", "正在新建")
	if err != nil {
		bridgeLock.Unlock()
		startLock.Unlock()
		stopLock.Unlock()
		return nil, err
	}
	unlock := func() {
		createLock.Unlock()
		bridgeLock.Unlock()
		stopLock.Unlock()
		startLock.Unlock()
	}
	if _, running := m.qemuOf(i); running {
		unlock()
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 未关闭，无法删除", i)
	}
	if len(leftovers) > 0 {
		m.Log.Warnf("魔改 AVD 实例 %d 不完整（只剩：%s），可能是上次删除或新建到一半，把这些残留删掉", i,
			strings.Join(leftovers, "、"))
	}
	if err := avd.Delete(m.Root, i); err != nil {
		unlock()
		return nil, err
	}
	// Confirm it is gone: a later create must not find a half-deleted instance under this index.
	if left := instanceFilesOnDisk(m.Root, i); len(left) > 0 {
		unlock()
		return nil, errs.Newf(errs.Internal, "魔改 AVD 实例 %d 没有删干净，还剩：%s", i, strings.Join(left, "、")).
			WithAdvice(fmt.Sprintf("关掉占用这些文件的程序后再运行一次 modavd delete %d --yes，会接着删", i))
	}
	// The lock files can only go once nobody holds them (an open file cannot be deleted).
	unlock()
	m.removeLockFiles(i)
	if len(leftovers) > 0 {
		m.Log.Infof("已清掉魔改 AVD 实例 %d 的残留", i)
	} else {
		m.Log.Infof("已删除魔改 AVD 实例 %d", i)
	}
	return &DeleteResult{Index: i, Name: in.Name, Path: in.Dir}, nil
}

// tryInstanceLock takes lock kind of instance i now or fails with errs.Busy ("魔改 AVD 实例 0 正在启动，无法删除").
func (m *Manager) tryInstanceLock(i int, kind, what string) (*win.FileLock, error) {
	path := m.lockPath(i, kind)
	l, ok, err := win.TryLock(path)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+path)
	}
	if !ok {
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d %s，无法删除", i, what)
	}
	return l, nil
}

// formatApplied is "cpu=4, memory_mb=4096" (sorted, so the log line is stable).
func formatApplied(applied map[string]any) string {
	if len(applied) == 0 {
		return "无改动"
	}
	keys := make([]string, 0, len(applied))
	for k := range applied {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for n, k := range keys {
		parts[n] = fmt.Sprintf("%s=%v", k, applied[k])
	}
	return strings.Join(parts, ", ")
}

// instanceFilesOnDisk are the files of index i that exist (its .ini and its directory), whether or not they make a
// complete instance.
func instanceFilesOnDisk(r *root.Root, i int) []string {
	var left []string
	for _, p := range []string{avd.IniPath(r, i), avd.Dir(r, i)} {
		if fileOrDirExists(p) {
			left = append(left, p)
		}
	}
	return left
}

// instanceLeftovers is what of index i exists when it is not a complete instance (for messages): its .ini, its
// directory (noting a missing config.ini) and instances.<i> in mas-avd.json.
func instanceLeftovers(r *root.Root, i int) []string {
	var left []string
	if p := avd.IniPath(r, i); fileOrDirExists(p) {
		left = append(left, p)
	}
	if d := avd.Dir(r, i); fileOrDirExists(d) {
		if !fileOrDirExists(avd.ConfigPath(r, i)) {
			d += "（里面没有 config.ini）"
		}
		left = append(left, d)
	}
	if r.HasInstanceMeta(i) {
		left = append(left, root.MetadataFile+" 里的 instances."+strconv.Itoa(i))
	}
	return left
}

// fileOrDirExists: something is at path. Lstat: a link whose target is gone still occupies the name (and
// avd.Delete refuses to follow it).
func fileOrDirExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Lstat(path)
	return err == nil
}
