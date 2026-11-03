package emu

// modavd disk <id>: reclaim the space the guest freed on /data. `trim` triggers
// a guest FITRIM at runtime so the emulator's virtio-blk discard path frees the
// qcow2 clusters behind deleted files (ModAVD/ModAVD#2). `info` and `compact`
// land in later commits. trim is only useful once the emulator advertises
// discard on the data disk (the mas/discard emulator build); an older build
// reports no discard and trim returns errs.Unsupported with a clear message.

import (
	"context"
	"strconv"
	"strings"
	"time"

	"modavd/internal/errs"
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
