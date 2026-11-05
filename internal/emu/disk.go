package emu

// modavd disk <id>: reclaim the space the guest freed on /data (ModAVD/ModAVD#2).
// `trim` triggers a guest FITRIM at runtime so the emulator's virtio-blk discard
// path frees the qcow2 clusters behind deleted files; `info` reports the data disk's
// sizes (what a reclaim could save); `compact` rewrites the overlay offline to drop
// the freed clusters and shrink the file. trim is only useful once the emulator
// advertises discard on the data disk (the mas/discard emulator build); an older
// build reports no discard and trim returns errs.Unsupported with a clear message.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

// userdataOverlay / userdataBacking name the data-disk files inside an instance
// directory (used by info and compact; the backing is read-only).
const (
	userdataOverlay = "userdata-qemu.img.qcow2"
	userdataBacking = "userdata-qemu.img"
)

// diskTrimTimeout bounds the guest idle-maintenance run. A first full trim of a
// large /data walks all the free space and can take a few seconds; later trims
// are quick. Generous so a big first pass does not time out.
const diskTrimTimeout = 2 * time.Minute

// vdcDiscardMaxPath is the data disk's advertised discard limit in the guest. 0
// (or the file missing) means the emulator does not advertise
// VIRTIO_BLK_F_DISCARD on /data -- i.e. an emulator without the discard
// backport, where a trim would silently do nothing.
const vdcDiscardMaxPath = "/sys/block/vdc/queue/discard_max_bytes"

// DiskTrimResult reports what `modavd disk <id> trim` did.
type DiskTrimResult struct {
	Index int `json:"index"`
	// DiscardMaxBytes is the data disk's advertised discard limit (guest
	// /sys/block/vdc/queue/discard_max_bytes); >0 confirms the emulator's
	// discard path is live.
	DiscardMaxBytes int64 `json:"discard_max_bytes"`
}

// parseDiscardMaxBytes reads the number printed by cat of a sysfs
// discard_max_bytes file; ok is false when the output is empty or not a number
// (the file was missing, or cat failed).
func parseDiscardMaxBytes(out string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// probeDiscardMax reads the data disk's advertised discard limit in the guest (vdcDiscardMaxPath, root-only). ok is
// false when it could not be read (adb failed, the file is missing); the caller checks ctx itself. timeout 0 is the
// default of m.su.
func (m *Manager) probeDiscardMax(ctx context.Context, i int, timeout time.Duration) (int64, bool) {
	probe := m.su(ctx, i, "cat "+vdcDiscardMaxPath, timeout)
	return parseDiscardMaxBytes(probe.Out)
}

// runIdleMaint triggers the guest's vold idle maintenance (it runs fstrim on the mounted file systems). Shared by
// DiskTrim and the best-effort trim after boot (autotrim.go).
func (m *Manager) runIdleMaint(ctx context.Context, i int, timeout time.Duration) adb.Result {
	return m.su(ctx, i, "sm idle-maint run", timeout)
}

// DiskTrim triggers a guest FITRIM on /data (via vold idle maintenance) so the
// virtio-blk discard path frees the qcow2 clusters behind deleted files. The
// instance must be running and booted. errs.Unsupported when the instance is
// not up, or when the emulator does not advertise discard on the data disk (an
// old build); errs.Internal when the guest command fails.
func (m *Manager) DiskTrim(ctx context.Context, i int) (*DiskTrimResult, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	if _, running, err := m.qemuOf(i); err != nil {
		return nil, err
	} else if !running {
		return nil, errs.Newf(errs.Unsupported,
			"魔改 AVD 实例 %d 没有在运行，请先开机再回收磁盘空间", i)
	}
	if !m.bootCompleted(ctx, i, statusAdbTimeout, true) {
		if err := ctxErr(ctx); err != nil {
			return nil, err
		}
		return nil, errs.Newf(errs.Unsupported,
			"魔改 AVD 实例 %d 还没开机完成，请开机后再回收磁盘空间", i)
	}

	// Gate on the emulator actually advertising discard on /data: an old build
	// reports 0 here, and a trim would silently do nothing.
	maxBytes, ok := m.probeDiscardMax(ctx, i, 0)
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if !ok || maxBytes <= 0 {
		return nil, errs.Newf(errs.Unsupported,
			"这个模拟器版本不支持回收磁盘空间（客体数据盘没有 discard）").
			WithAdvice("请把魔改 AVD 更新到带磁盘回收的版本，实例和游戏数据不受影响")
	}

	// vold idle maintenance runs fstrim on the mounted file systems. (The exact
	// mechanism may be refined to a targeted FITRIM after on-device testing,
	// ModAVD/ModAVD#2 §4.3.)
	res := m.runIdleMaint(ctx, i, diskTrimTimeout)
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if res.Code != 0 {
		return nil, errs.Newf(errs.Internal, "触发磁盘回收失败: %s",
			orText(tail(res.Out, 300), "退出码 "+strconv.Itoa(res.Code)))
	}
	return &DiskTrimResult{Index: i, DiscardMaxBytes: maxBytes}, nil
}

// diskCheckTimeout bounds one `qemu-img check` of the data disk: it reads the refcount and L1/L2 tables, seconds even
// for a large image, but a slow or busy disk gets room.
const diskCheckTimeout = 5 * time.Minute

// DiskInfoResult reports `modavd disk <id> info`: the sizes of the instance's data disk (the qcow2 overlay; its
// backing image is read-only and shared), so the user sees what a reclaim would save.
type DiskInfoResult struct {
	Index int `json:"index"`
	// Overlay is the data disk's qcow2 overlay file.
	Overlay string `json:"overlay"`
	// Running is true when the instance's qemu is up. The overlay is then open in qemu: AllocatedBytes and
	// ReclaimableBytes are left out (NeedsShutdown), qemu-img is not run on it.
	Running       bool `json:"running"`
	NeedsShutdown bool `json:"needs_shutdown"`
	// LogicalBytes is the virtual size of the disk (qcow2 header); 0 when it could not be read.
	LogicalBytes int64 `json:"logical_bytes,omitempty"`
	// OnDiskBytes is what the overlay occupies on the host volume (GetCompressedFileSize: a sparse file counts only
	// its data).
	// nil when the host allocation cannot be read; unknown is never reported as zero.
	OnDiskBytes *int64 `json:"on_disk_bytes,omitempty"`
	// ClusterBytes is the qcow2 cluster size.
	ClusterBytes int64 `json:"cluster_bytes,omitempty"`
	// AllocatedBytes is the guest data the overlay really holds (allocated clusters x cluster size, qemu-img check);
	// nil while the instance runs.
	AllocatedBytes *int64 `json:"allocated_bytes,omitempty"`
	// ReclaimableBytes estimates what a compact would save: OnDiskBytes minus AllocatedBytes (the rewritten image is
	// about AllocatedBytes plus a little metadata, so the real saving is slightly less). nil while the instance runs
	// or when the image check found errors.
	ReclaimableBytes *int64   `json:"reclaimable_bytes,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

// qcow2Header is the part of a qcow2 header info needs.
type qcow2Header struct {
	VirtualSize int64
	ClusterSize int64
}

// parseQcow2Header reads the magic, cluster_bits (offset 20) and size (offset 24) of the first bytes of a qcow2 file
// (all big-endian).
func parseQcow2Header(b []byte) (qcow2Header, error) {
	if len(b) < 32 {
		return qcow2Header{}, errs.New(errs.Internal, "数据盘文件太短，不是 qcow2 镜像")
	}
	if binary.BigEndian.Uint32(b[0:4]) != 0x514649fb { // "QFI\xfb"
		return qcow2Header{}, errs.New(errs.Internal, "数据盘文件不是 qcow2 镜像")
	}
	bits := binary.BigEndian.Uint32(b[20:24])
	size := binary.BigEndian.Uint64(b[24:32])
	if bits < 9 || bits > 21 || size > 1<<62 {
		return qcow2Header{}, errs.New(errs.Internal, "数据盘 qcow2 头里的簇大小或容量不合理")
	}
	return qcow2Header{VirtualSize: int64(size), ClusterSize: int64(1) << bits}, nil
}

// qemuImgCheck is the part of `qemu-img check --output=json` info reads. Pointers: a missing field is not 0.
type qemuImgCheck struct {
	AllocatedClusters *int64 `json:"allocated-clusters"`
	ImageEndOffset    *int64 `json:"image-end-offset"`
	CheckErrors       int64  `json:"check-errors"`
	Corruptions       int64  `json:"corruptions"`
	TotalClusters     *int64 `json:"total-clusters"`
	Format            string `json:"format"`
	allocationKnown   bool
}

// parseQemuImgCheck decodes the JSON object of `qemu-img check --output=json`. Anything before its first "{" (a
// warning line) is ignored. errs.Internal when there is no object or no allocated-clusters.
func parseQemuImgCheck(out []byte) (qemuImgCheck, error) {
	var c qemuImgCheck
	start := bytes.IndexByte(out, '{')
	if start < 0 {
		return c, errs.Newf(errs.Internal, "qemu-img check 没有输出 JSON: %s", orText(tail(string(out), 200), "（空）"))
	}
	if err := json.NewDecoder(bytes.NewReader(out[start:])).Decode(&c); err != nil {
		return c, errs.Wrap(errs.Internal, err, "读不懂 qemu-img check 的输出: "+err.Error())
	}
	// qemu-img omits allocated-clusters when it is 0 (an empty image); a clean decode with it missing means 0, not an
	// error (ModAVD/ModAVD#2 R-3). Callers gate on check-errors/corruptions before they use the count.
	c.allocationKnown = c.AllocatedClusters != nil || (c.TotalClusters != nil && *c.TotalClusters >= 0 && c.Format == "qcow2")
	if c.AllocatedClusters == nil {
		zero := int64(0)
		c.AllocatedClusters = &zero
	} else if *c.AllocatedClusters < 0 {
		return c, errs.New(errs.Internal, "qemu-img check 的 allocated-clusters 不合理")
	}
	return c, nil
}

// runQemuImgCheck runs `qemu-img check --output=json <image>` and returns its stdout. Exit 0 (clean), 2 (corrupt) and
// 3 (leaked clusters) still carry the JSON report; any other exit (the file is open in a qemu, not a qcow2 ...) comes
// back as failure, the text of why, with a nil error. An error means qemu-img did not run to the end (missing, not
// startable, canceled, timed out). The image must not be open in a running qemu.
func (m *Manager) runQemuImgCheck(ctx context.Context, image string) (out []byte, failure string, err error) {
	exe, err := m.Root.QemuImgExe()
	if err != nil {
		return nil, "", err
	}
	cctx, cancel := context.WithTimeout(ctx, diskCheckTimeout)
	defer cancel()
	stdout, stderr, state, err := win.RunBound(cctx, exe, "check", "--output=json", image)
	if ctx.Err() != nil {
		return nil, "", errs.Wrap(errs.Canceled, ctx.Err(), "已取消（qemu-img 没跑完）")
	}
	if cctx.Err() == context.DeadlineExceeded {
		return nil, "", errs.Newf(errs.Timeout, "qemu-img check %.0f 分钟没跑完", diskCheckTimeout.Minutes())
	}
	if state == nil {
		return nil, "", errs.Wrap(errs.Internal, err, "运行不了 qemu-img: "+err.Error())
	}
	switch code := state.ExitCode(); code {
	case 0, 2, 3:
		return stdout, "", nil
	default:
		return nil, fmt.Sprintf("qemu-img check 失败（退出码 %d）: %s", code,
			orText(tail(strings.TrimSpace(string(stderr)), 300), "没有输出")), nil
	}
}

// readQcow2Header reads the first bytes of the qcow2 file at path.
func readQcow2Header(path string) (qcow2Header, error) {
	f, err := root.OpenShared(path)
	if err != nil {
		return qcow2Header{}, errs.Wrap(errs.Internal, err, "打不开数据盘文件: "+err.Error())
	}
	defer f.Close()
	buf := make([]byte, 32)
	if _, err := io.ReadFull(f, buf); err != nil {
		return qcow2Header{}, errs.Wrap(errs.Internal, err, "读不了数据盘文件头: "+err.Error())
	}
	return parseQcow2Header(buf)
}

// reclaimable is the estimate of what compact saves: the space the overlay occupies beyond the clusters it holds,
// never negative.
func reclaimable(onDisk, allocated int64) int64 {
	if onDisk > allocated {
		return onDisk - allocated
	}
	return 0
}

// DiskInfo reports the data disk's sizes of instance i (see DiskInfoResult). It works for a running and a stopped
// instance. Allocated and reclaimable figures require an offline instance and its start lock. Lock failures fall
// back to readable header/host sizes with warnings; an unreadable size is omitted. errs.NotFound when no overlay
// exists; errs.NotReady when qemu-img is missing and a locked offline check was attempted.
func (m *Manager) DiskInfo(ctx context.Context, i int) (*DiskInfoResult, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	overlay := filepath.Join(in.Dir, userdataOverlay)
	if !regularFile(overlay) {
		return nil, errs.Newf(errs.NotFound, "实例 %d 没有数据盘覆盖层: %s", i, overlay).
			WithAdvice("实例还没开过机，或数据盘不是这种格式")
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	res := &DiskInfoResult{Index: i, Overlay: overlay}
	// A held start lock excludes compact/delete/start and the updater's SDK swap.
	// If it cannot be opened or acquired, never invoke qemu-img on this image.
	startLock, locked, lockErr := m.tryOperationLock(i, "start")
	defer startLock.Unlock()
	if lockErr != nil {
		res.Warnings = append(res.Warnings, lockErr.Error()+"；仅报告当前可读取的文件头和宿主占用，未运行 qemu-img check")
	} else if !locked {
		res.NeedsShutdown = true
		res.Warnings = append(res.Warnings, "实例启动锁被占用，可能正在启动或更新；仅报告当前可读取的文件头和宿主占用，未运行 qemu-img check")
	}
	_, running, err := m.qemuOf(i)
	if err != nil {
		return nil, err
	}
	res.Running = running
	if running {
		res.NeedsShutdown = true
		res.Warnings = append(res.Warnings, "实例在运行：已分配和可回收的大小要关机后才能算")
	}

	if onDisk, err := win.GetCompressedFileSize(overlay); err != nil {
		res.Warnings = append(res.Warnings, "读不到数据盘的宿主占用："+err.Error())
	} else {
		res.OnDiskBytes = new(int64(onDisk))
	}
	// Both reads are best effort snapshots. They neither repair the image nor
	// claim a consistent allocation estimate while another process changes it.
	hdr, herr := readQcow2Header(overlay)
	if herr != nil {
		res.Warnings = append(res.Warnings, herr.Error())
	} else {
		res.LogicalBytes, res.ClusterBytes = hdr.VirtualSize, hdr.ClusterSize
	}
	if !locked || running || herr != nil {
		return res, nil
	}

	out, failure, err := m.runQemuImgCheck(ctx, overlay)
	if err != nil {
		return nil, err
	}
	if failure != "" {
		// e.g. the file is still open in a qemu that is shutting down: the sizes read so far stand
		res.Warnings = append(res.Warnings, "算不出已分配和可回收的大小："+failure)
		return res, nil
	}
	chk, err := parseQemuImgCheck(out)
	if err != nil {
		return nil, err
	}
	if chk.CheckErrors > 0 || chk.Corruptions > 0 {
		res.Warnings = append(res.Warnings, "数据盘镜像检查发现错误，已分配和可回收的大小不可信，已省略。"+damagedDiskAdvice)
		return res, nil
	}
	if res.ClusterBytes == 0 {
		return nil, errs.New(errs.Internal, "读不到数据盘的簇大小")
	}
	alloc := *chk.AllocatedClusters * res.ClusterBytes
	res.AllocatedBytes = &alloc
	if res.OnDiskBytes != nil {
		rec := reclaimable(*res.OnDiskBytes, alloc)
		res.ReclaimableBytes = &rec
	}
	return res, nil
}

// ---- compact -----------------------------------------------------------------

const (
	// diskInfoTimeout bounds a quick `qemu-img info`.
	diskInfoTimeout = time.Minute
	// compactSpaceSlack is the headroom required on the volume beyond the estimated new overlay (allocated x 1.05),
	// so a convert cannot fill the volume and stall other instances sharing it (ModAVD/ModAVD#2 P1-2).
	compactSpaceSlack = 2 << 30 // 2 GiB
)

// userdataCompactTmp is the scratch file compact converts into before swapping it over the overlay. Its name is no
// longer than the overlay's, so the temp path can never overflow MAX_PATH where the overlay path itself fits
// (ModAVD/ModAVD#2 P3-3). A stray one from an interrupted run is removed on the next run (clearStrayCompactTmp); on
// NTFS/ReFS the original overlay is intact whenever a temp is present, so the temp is never the only copy.
const userdataCompactTmp = "userdata-qemu.cmp"

// compactFileSystems are the journaled file systems compact runs on: a write-through replace is one transaction
// there, so the final swap is atomic and a crash never leaves the overlay missing with only the temp file behind.
var compactFileSystems = map[string]bool{"NTFS": true, "REFS": true}

// requireCompactFS refuses to compact on a file system where the final rename is not atomic (exFAT, FAT32): a crash
// mid-rename could leave only the temp file, and the emulator would then build an empty overlay over the backing,
// so a later run would delete the one good copy (ModAVD/ModAVD#2 P2-1).
func requireCompactFS(overlay string) error {
	fs, err := win.VolumeFileSystem(overlay)
	if err != nil {
		return err
	}
	if !compactFileSystems[strings.ToUpper(fs)] {
		return errs.Newf(errs.Unsupported, "数据盘所在卷是 %s，只能在 NTFS 或 ReFS 卷上整理磁盘", fs).
			WithAdvice("在这种文件系统上换文件不是原子操作，中途断电可能丢数据；请把实例放到 NTFS 卷上再整理")
	}
	return nil
}

// DiskCompactResult reports `modavd disk <id> compact`.
type DiskCompactResult struct {
	Index             int    `json:"index"`
	Overlay           string `json:"overlay"`
	BeforeOnDiskBytes int64  `json:"before_on_disk_bytes"`
	AfterOnDiskBytes  int64  `json:"after_on_disk_bytes"`
	// FreedBytes keeps its original nonnegative contract; growth is represented
	// by OnDiskDeltaBytes (after minus before; positive means increased host disk usage).
	FreedBytes                int64  `json:"freed_bytes"`
	OnDiskDeltaBytes          int64  `json:"on_disk_delta_bytes"`
	Skipped                   bool   `json:"skipped"`
	EstimatedReclaimableBytes *int64 `json:"estimated_reclaimable_bytes,omitempty"`
	ReclaimThresholdBytes     int64  `json:"reclaim_threshold_bytes"`
}

// qemuImgInfoResult is the part of `qemu-img info --output=json` compact reads.
type qemuImgInfoResult struct {
	Format             string            `json:"format"`
	VirtualSize        int64             `json:"virtual-size"`
	BackingFilename    string            `json:"backing-filename"`
	BackingFilenameFmt string            `json:"backing-filename-format"`
	Snapshots          []json.RawMessage `json:"snapshots"`
}

// lockInstanceForCompact takes the per-instance locks that keep anyone from booting,
// stopping, bridging or creating instance i while compact rewrites its data disk.
// Disk is taken first and released last to identify the owner of these locks;
// errs.Busy when any is already held. The returned func releases them; call it once.
func (m *Manager) lockInstanceForCompact(i int) (func(), error) {
	kinds := []struct{ kind, what string }{
		{"disk", "正在整理磁盘"},
		{"start", "正在启动"},
		{"stop", "正在关闭"},
		{"bridge", "正在装卸执行桥"},
		{"create", "正在新建"},
	}
	var locks []*win.FileLock
	unlock := func() {
		for k := len(locks) - 1; k >= 0; k-- {
			locks[k].Unlock()
		}
	}
	for _, kw := range kinds {
		path := m.lockPath(i, kw.kind)
		var l *win.FileLock
		var ok bool
		var err error
		if kw.kind == "disk" {
			// Let a short shared probe finish. Lifecycle locks below never wait.
			l, ok, err = win.Lock(path, 100*time.Millisecond)
		} else {
			l, ok, err = win.TryLock(path)
		}
		if err != nil {
			unlock()
			return nil, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+path)
		}
		if !ok {
			unlock()
			if kw.kind == "disk" {
				if err := m.compactBusy(i); err != nil {
					return nil, err
				}
				return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 的磁盘状态正在读取，请稍后再整理", i)
			}
			return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d %s，无法整理磁盘", i, kw.what)
		}
		locks = append(locks, l)
	}
	return unlock, nil
}

// runQemuImg runs qemu-img with args and returns its stdout, stderr and the process
// exit code. err is non-nil only when qemu-img did not run to the end (missing, not
// startable, canceled, timed out); a non-zero exit with err==nil is the caller's to
// interpret. The image must not be open in a running qemu.
func (m *Manager) runQemuImg(ctx context.Context, timeout time.Duration, args ...string) (stdout, stderr []byte, code int, err error) {
	exe, err := m.Root.QemuImgExe()
	if err != nil {
		return nil, nil, 0, err
	}
	cctx, cancel := qemuImgContext(ctx, timeout)
	defer cancel()
	so, se, state, runErr := win.RunBound(cctx, exe, args...)
	if ctx.Err() != nil {
		return nil, nil, 0, errs.Wrap(errs.Canceled, ctx.Err(), "已取消（qemu-img 没跑完）")
	}
	if cctx.Err() == context.DeadlineExceeded {
		return nil, nil, 0, errs.Newf(errs.Timeout, "qemu-img %s %s没跑完", args[0], qemuImgTimeoutText(timeout))
	}
	if state == nil {
		return nil, nil, 0, errs.Wrap(errs.Internal, runErr, "运行不了 qemu-img: "+runErr.Error())
	}
	return so, se, state.ExitCode(), nil
}

// qemuImgFail picks the most useful tail of a failed qemu-img run (stderr first).
func qemuImgFail(stdout, stderr []byte) string {
	return orText(tail(strings.TrimSpace(string(stderr)), 300),
		orText(tail(strings.TrimSpace(string(stdout)), 300), "没有输出"))
}

// qemuImgInfo reads the overlay's backing filename, backing format and whether it has
// internal snapshots from `qemu-img info --output=json`.
func (m *Manager) qemuImgInfo(ctx context.Context, image string) (*qemuImgInfoResult, error) {
	so, se, code, err := m.runQemuImg(ctx, diskInfoTimeout, "info", "--output=json", image)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errs.Newf(errs.Internal, "qemu-img info 失败（退出码 %d）: %s", code, qemuImgFail(so, se))
	}
	start := bytes.IndexByte(so, '{')
	if start < 0 {
		return nil, errs.New(errs.Internal, "qemu-img info 没有输出 JSON")
	}
	var r qemuImgInfoResult
	if err := json.NewDecoder(bytes.NewReader(so[start:])).Decode(&r); err != nil {
		return nil, errs.Wrap(errs.Internal, err, "读不懂 qemu-img info 的输出: "+err.Error())
	}
	return &r, nil
}

// verifyCompacted checks the converted image is sound (qemu-img check: no errors or
// corruptions) and holds the same guest-visible content as the original (qemu-img
// compare), before it is swapped in.
func (m *Manager) verifyCompacted(ctx context.Context, orig, tmp string) error {
	out, failure, err := m.runQemuImgCheck(ctx, tmp)
	if err != nil {
		return err
	}
	if failure != "" {
		return errs.Newf(errs.Internal, "整理后的数据盘检查没通过：%s，放弃替换，原盘未动", failure)
	}
	chk, err := parseQemuImgCheck(out)
	if err != nil {
		return err
	}
	if chk.CheckErrors > 0 || chk.Corruptions > 0 {
		return errs.Newf(errs.Internal, "整理后的数据盘有 %d 处检查错误、%d 处损坏，放弃替换，原盘未动",
			chk.CheckErrors, chk.Corruptions)
	}
	// compare the effective content (overlay + backing); equal means the swap is safe.
	equal, err := m.compareEqual(ctx, orig, tmp)
	if err != nil {
		return err
	}
	if !equal {
		return errs.New(errs.Internal, "整理前后的数据盘内容对不上，放弃替换，原盘未动")
	}
	return nil
}

// compareEqual runs `qemu-img compare a b` and reports whether they hold identical guest-visible content (whole
// backing chain, both sides). compare exits 0 when identical and 1 when they differ; any other exit (>=2, or a
// killed process) is a real failure, returned as err so a mismatch is never mistaken for a compare that could not
// run (ModAVD/ModAVD#2 P3-7).
func (m *Manager) compareEqual(ctx context.Context, a, b string) (bool, error) {
	var virtual int64
	ah, ae := readQcow2Header(a)
	bh, be := readQcow2Header(b)
	if ae == nil && be == nil {
		virtual = max(ah.VirtualSize, bh.VirtualSize)
	}
	so, se, code, err := m.runQemuImg(ctx, compactTimeout(virtual, 0, 0, 2, 20*time.Minute), "compare", a, b)
	if err != nil {
		return false, err
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, errs.Newf(errs.Internal, "qemu-img compare 没跑完（退出码 %d）: %s", code, qemuImgFail(so, se))
	}
}

// syncFile flushes the file's pending writes to disk (FlushFileBuffers on Windows).
func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	serr := f.Sync()
	if cerr := f.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// dirHasEntries is true when path is a directory with at least one entry.
func dirHasEntries(path string) bool {
	es, err := os.ReadDir(path)
	return err == nil && len(es) > 0
}

// disk info checks the header and host allocation; offline with a lock it also
// runs a read-only qcow2 metadata check. It cannot repair qcow2 or guest ext4 data.
const damagedDiskAdvice = "modavd disk <id> info 只读取文件头和宿主占用，关机且取得锁后还会运行只读的 qemu-img check 检查 qcow2 元数据；不会修复镜像，也不检查客体文件系统。请保持关机，先备份整个实例目录及其引用的后备镜像；有正常备份时恢复整套镜像，无备份时保留副本和检查输出并联系维护者排查。确认不需要原有数据后，可用 modavd delete <id> --yes 删除，再用 modavd create 新建"

// probeCompactAllocated runs `qemu-img check` on the overlay and returns the bytes its live clusters hold
// (allocated clusters x cluster size), used to size the volume headroom compact needs (ModAVD/ModAVD#2 P1-2). It
// refuses a disk whose check reports errors or corruption: the size would be unreliable and compacting a damaged
// image is unsafe. The overlay must not be open in a running qemu (compact holds the locks that guarantee that).
func (m *Manager) probeCompactAllocated(ctx context.Context, overlay string, clusterBytes int64) (*int64, error) {
	out, failure, err := m.runQemuImgCheck(ctx, overlay)
	if err != nil {
		return nil, err
	}
	if failure != "" {
		return nil, errs.Newf(errs.Internal, "整理前检查数据盘没通过：%s，没有整理", failure)
	}
	chk, err := parseQemuImgCheck(out)
	if err != nil {
		return nil, err
	}
	if chk.CheckErrors > 0 || chk.Corruptions > 0 {
		return nil, errs.Newf(errs.Unsupported,
			"数据盘镜像有 %d 处检查错误、%d 处损坏，先修复再整理", chk.CheckErrors, chk.Corruptions).
			WithAdvice(damagedDiskAdvice)
	}
	if !chk.allocationKnown {
		return nil, nil
	}
	allocated, err := checkedCompactAllocation(*chk.AllocatedClusters, clusterBytes)
	if err != nil {
		return nil, err
	}
	return &allocated, nil
}

func compactHostBytes(path string) (int64, error) {
	n, err := win.GetCompressedFileSize(path)
	if err != nil {
		return 0, err
	}
	if n > math.MaxInt64 {
		return 0, errs.New(errs.Internal, "宿主文件占用超出可表示范围，无法整理")
	}
	return int64(n), nil
}

// clearStrayCompactTmp removes a temp file left by an interrupted earlier compact, before a new run reuses the path.
// Compact runs only on NTFS/ReFS, where the final swap is one atomic transaction, so whenever a temp is present the
// overlay is still the intact original (the swap never completed): the temp is at most a half-written or stale copy
// and is always safe to drop (ModAVD/ModAVD#2 P2-1, R-1). os.Lstat (not Stat) so a symlink at the name -- even a
// dangling one -- is seen and its link entry removed here, never followed by the convert below (R-2).
func (m *Manager) clearStrayCompactTmp(tmp string) error {
	if _, err := os.Lstat(tmp); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errs.Wrap(errs.Internal, err, "查看整理临时文件失败: "+err.Error())
	}
	m.Log.Infof("删除上次整理遗留的临时文件（数据盘是完整的原盘，未受影响）：%s", tmp)
	return removeCompactTmp(tmp)
}

// removeCompactTmp deletes a compact temp file; a missing file is not an error.
func removeCompactTmp(tmp string) error {
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return errs.Wrap(errs.Internal, err, "删除整理临时文件失败: "+err.Error())
	}
	return nil
}

// DiskCompact offline-compacts instance i's data disk: it converts the overlay to a
// fresh qcow2 that keeps only the clusters still in use (same backing), checks and
// compares the copy, then atomically swaps it in with a write-through replace. The
// instance must be powered off; compact holds its disk/start/stop/bridge/create locks so
// nothing boots it mid-rewrite. It runs only on NTFS/ReFS (where the swap is atomic),
// refuses when the data disk or the instance has snapshots (the convert would drop
// them), and refuses when the volume lacks room for the copy (so a convert cannot fill
// it and stall other instances). The original is untouched until the final replace, so
// a crash at any earlier point leaves a working instance (a stray temp from an
// interrupted run is removed on the next run; the original is intact on NTFS/ReFS).
// errs.Busy when the instance is running or a lock is held; errs.Unsupported for a
// linked overlay, non-journaled volume, snapshots, a missing backing or too little free space.
func (m *Manager) DiskCompact(ctx context.Context, i int) (*DiskCompactResult, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	in, err := m.instance(i)
	if err != nil {
		return nil, err
	}
	overlay := filepath.Join(in.Dir, userdataOverlay)
	st, err := os.Lstat(overlay)
	if err != nil && !os.IsNotExist(err) {
		return nil, errs.Wrap(errs.Internal, err, "查看数据盘覆盖层失败: "+overlay+": "+err.Error())
	}
	if err == nil {
		// Check the filesystem entry before locks, qemu-img or scratch cleanup.
		// A qcow2 backing reference is not a filesystem link and remains supported.
		if err := checkCompactOverlayLinks(overlay, st); err != nil {
			return nil, err
		}
	}
	if err != nil || !st.Mode().IsRegular() {
		return nil, errs.Newf(errs.NotFound, "实例 %d 没有数据盘覆盖层: %s", i, overlay).
			WithAdvice("实例还没开过机，或数据盘不是这种格式")
	}
	if err := requireCompactFS(overlay); err != nil {
		return nil, err
	}
	unlock, err := m.lockInstanceForCompact(i)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if _, running, err := m.qemuOf(i); err != nil {
		return nil, err
	} else if running {
		return nil, errs.Newf(errs.Busy, "魔改 AVD 实例 %d 未关闭，无法整理磁盘", i).
			WithAdvice(fmt.Sprintf("先 modavd stop %d，关机后再整理", i))
	}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	info, err := m.qemuImgInfo(ctx, overlay)
	if err != nil {
		return nil, err
	}
	if len(info.Snapshots) > 0 {
		return nil, errs.Newf(errs.Unsupported, "数据盘里有 %d 个内部快照，整理磁盘会丢掉它们", len(info.Snapshots)).
			WithAdvice("先删掉快照再整理")
	}
	if dirHasEntries(filepath.Join(in.Dir, "snapshots")) {
		return nil, errs.New(errs.Unsupported, "实例里有内存快照（snapshots 目录非空），整理磁盘会和它们对不上").
			WithAdvice("先删掉快照再整理")
	}
	if info.BackingFilename == "" {
		return nil, errs.New(errs.Unsupported, "数据盘没有 backing，不整理（避免把整盘复制进来）")
	}
	if info.BackingFilenameFmt == "" {
		return nil, errs.New(errs.Unsupported, "读不到数据盘 backing 的格式，不整理")
	}
	backingName := filepath.Base(info.BackingFilename)

	before, err := compactHostBytes(overlay)
	if err != nil {
		return nil, err
	}

	// Clear a temp left by an interrupted earlier run first, so the space it would free counts toward the room check
	// below (it may be as large as the whole copy). On NTFS/ReFS the original is intact whenever a temp is present,
	// so the temp is never the only copy (ModAVD/ModAVD#2 P2-1).
	tmp := filepath.Join(in.Dir, userdataCompactTmp)
	if err := m.clearStrayCompactTmp(tmp); err != nil {
		return nil, err
	}

	// Size the rewrite and refuse if the volume cannot hold the copy: a convert that fills the volume would stall
	// other instances sharing it in io-error (ModAVD/ModAVD#2 P1-2). The new overlay is about the live-cluster size
	// plus a little metadata; keep headroom beyond that.
	hdr, err := readQcow2Header(overlay)
	if err != nil {
		return nil, err
	}
	allocated, err := m.probeCompactAllocated(ctx, overlay, hdr.ClusterSize)
	if err != nil {
		return nil, err
	}
	estimate, threshold := compactBenefit(before, allocated)
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	if estimate != nil && *estimate < threshold {
		m.Log.Infof("魔改 AVD 实例 %d 预计可回收 %d 字节，低于 %d 字节，跳过磁盘重写", i, *estimate, threshold)
		return compactResult(i, overlay, before, before, estimate, threshold, true), nil
	}
	// Without an allocation count, reserve for the complete virtual image and
	// do not skip. A missing count must not masquerade as an empty disk.
	workAllocated := hdr.VirtualSize
	if allocated != nil {
		workAllocated = *allocated
	}
	free, err := win.DiskFree(in.Dir)
	if err != nil {
		return nil, err
	}
	need, err := compactRequiredSpace(workAllocated)
	if err != nil {
		return nil, err
	}
	if free < uint64(need) {
		return nil, errs.Newf(errs.Unsupported,
			"整理磁盘大约需要 %s 可用空间，%s 所在磁盘只剩 %s，请先腾出约 %s 再整理",
			gib(need), in.Dir, gib(int64(free)), gib(need-int64(free))).
			WithAdvice("整理会先写一份新的数据盘，空间不够会写满磁盘、拖累同一个磁盘上开着的其他实例")
	}

	// convert: a fresh overlay holding only the live clusters, same backing. -B skips the backing's data. The source
	// is read through the host cache: a qcow2 source with cache=none (-T none) fails on 2.12's file-win32 because the
	// header read is not sector-aligned, and the cache only touches referenced clusters, not the holes (#2 P1-1).
	budget := compactTimeout(hdr.VirtualSize, workAllocated, before, 3, 30*time.Minute)
	so, se, code, err := m.runQemuImg(ctx, budget, "convert",
		"-O", "qcow2", "-B", backingName, "-o", "backing_fmt="+info.BackingFilenameFmt, overlay, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if code != 0 {
		_ = os.Remove(tmp)
		return nil, errs.Newf(errs.Internal, "qemu-img convert 失败（退出码 %d）: %s", code, qemuImgFail(so, se))
	}

	if err := m.verifyCompacted(ctx, overlay, tmp); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	// Flush the copy before measuring its host allocation.
	if err := syncFile(tmp); err != nil {
		_ = os.Remove(tmp)
		return nil, errs.Wrap(errs.Internal, err, "刷新整理后的数据盘失败")
	}
	// Measure while the original is still protected. Rename on the same volume
	// preserves allocation and no post-replace probe can turn success into error.
	after, err := compactHostBytes(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := ctxErr(ctx); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}
	if err := win.MoveFileReplaceThrough(tmp, overlay); err != nil {
		_ = os.Remove(tmp)
		return nil, err
	}

	m.Log.Infof("魔改 AVD 实例 %d 整理磁盘完成：占用 %s → %s", i, gib(before), gib(after))
	return compactResult(i, overlay, before, after, estimate, threshold, false), nil
}
