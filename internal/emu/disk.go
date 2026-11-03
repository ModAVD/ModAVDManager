package emu

// modavd disk <id>: reclaim the space the guest freed on /data. `trim` triggers
// a guest FITRIM at runtime so the emulator's virtio-blk discard path frees the
// qcow2 clusters behind deleted files (ModAVD/ModAVD#2). `info` reports the data
// disk's sizes (what a reclaim could save); `compact` lands in a later commit. trim is only useful once the emulator advertises
// discard on the data disk (the mas/discard emulator build); an older build
// reports no discard and trim returns errs.Unsupported with a clear message.

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/errs"
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
	DiscardMaxBytes int64 `json:"discardMaxBytes"`
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
	if _, running := m.qemuOf(i); !running {
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
	probe := m.su(ctx, i, "cat "+vdcDiscardMaxPath, 0)
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	maxBytes, ok := parseDiscardMaxBytes(probe.Out)
	if !ok || maxBytes <= 0 {
		return nil, errs.Newf(errs.Unsupported,
			"这个模拟器版本不支持回收磁盘空间（客体数据盘没有 discard）").
			WithAdvice("请把魔改 AVD 更新到带磁盘回收的版本，实例和游戏数据不受影响")
	}

	// vold idle maintenance runs fstrim on the mounted file systems. (The exact
	// mechanism may be refined to a targeted FITRIM after on-device testing,
	// ModAVD/ModAVD#2 §4.3.)
	res := m.su(ctx, i, "sm idle-maint run", diskTrimTimeout)
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
	NeedsShutdown bool `json:"needsShutdown"`
	// LogicalBytes is the virtual size of the disk (qcow2 header); 0 when it could not be read.
	LogicalBytes int64 `json:"logicalBytes,omitempty"`
	// OnDiskBytes is what the overlay occupies on the host volume (GetCompressedFileSize: a sparse file counts only
	// its data).
	OnDiskBytes int64 `json:"onDiskBytes"`
	// ClusterBytes is the qcow2 cluster size.
	ClusterBytes int64 `json:"clusterBytes,omitempty"`
	// AllocatedBytes is the guest data the overlay really holds (allocated clusters x cluster size, qemu-img check);
	// nil while the instance runs.
	AllocatedBytes *int64 `json:"allocatedBytes,omitempty"`
	// ReclaimableBytes estimates what a compact would save: OnDiskBytes minus AllocatedBytes (the rewritten image is
	// about AllocatedBytes plus a little metadata, so the real saving is slightly less). nil while the instance runs
	// or when the image check found errors.
	ReclaimableBytes *int64   `json:"reclaimableBytes,omitempty"`
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
	if c.AllocatedClusters == nil || *c.AllocatedClusters < 0 {
		return c, errs.New(errs.Internal, "qemu-img check 的输出里没有 allocated-clusters")
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
	cmd := exec.CommandContext(cctx, exe, "check", "--output=json", image)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	win.HideConsoleCmd(cmd)
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil, "", errs.Wrap(errs.Canceled, ctx.Err(), "已取消（qemu-img 没跑完）")
	}
	if cctx.Err() == context.DeadlineExceeded {
		return nil, "", errs.Newf(errs.Timeout, "qemu-img check %.0f 分钟没跑完", diskCheckTimeout.Minutes())
	}
	if cmd.ProcessState == nil {
		return nil, "", errs.Wrap(errs.Internal, err, "运行不了 qemu-img: "+err.Error())
	}
	switch code := cmd.ProcessState.ExitCode(); code {
	case 0, 2, 3:
		return stdout.Bytes(), "", nil
	default:
		return nil, fmt.Sprintf("qemu-img check 失败（退出码 %d）: %s", code,
			orText(tail(strings.TrimSpace(stderr.String()), 300), "没有输出")), nil
	}
}

// readQcow2Header reads the first bytes of the qcow2 file at path.
func readQcow2Header(path string) (qcow2Header, error) {
	f, err := os.Open(path)
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
// instance; the allocated and reclaimable figures need qemu-img on the image and so only a stopped instance gets them
// (NeedsShutdown otherwise). errs.NotFound when the instance has no data disk overlay; errs.NotReady when qemu-img is
// missing (stopped instance only).
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
	_, running := m.qemuOf(i)
	res := &DiskInfoResult{Index: i, Overlay: overlay, Running: running, NeedsShutdown: running}
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	onDisk, err := win.GetCompressedFileSize(overlay)
	if err != nil {
		return nil, err
	}
	res.OnDiskBytes = int64(onDisk)

	// The header read needs no qemu-img. A running qemu has the file open: if that blocks the read, leave the
	// logical size out rather than fail.
	hdr, herr := readQcow2Header(overlay)
	switch {
	case herr == nil:
		res.LogicalBytes, res.ClusterBytes = hdr.VirtualSize, hdr.ClusterSize
	case !running:
		return nil, herr
	default:
		res.Warnings = append(res.Warnings, "实例在运行，读不到数据盘的逻辑大小")
	}
	if running {
		res.Warnings = append(res.Warnings, "实例在运行：已分配和可回收的大小要关机后才能算")
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
		res.Warnings = append(res.Warnings, "数据盘镜像检查发现错误，已分配和可回收的大小不可信，已省略")
		return res, nil
	}
	if res.ClusterBytes == 0 {
		return nil, errs.New(errs.Internal, "读不到数据盘的簇大小")
	}
	alloc := *chk.AllocatedClusters * res.ClusterBytes
	rec := reclaimable(res.OnDiskBytes, alloc)
	res.AllocatedBytes, res.ReclaimableBytes = &alloc, &rec
	return res, nil
}
