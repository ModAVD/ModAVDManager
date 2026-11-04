package ui

import (
	"context"
	"fmt"
	"net/http"
)

// The data-disk reclaim of an instance (emu disk.go, `modavd disk`) as jobs. emu checks the instance state itself
// (trim wants it booted, compact wants it shut down, info works either way) and answers with its own errors, so the
// handlers only start the job; the page greys the menu items by status but the server does not repeat the rule.

// handleDiskInfo is POST /api/instances/{i}/disk-info: the sizes of the data disk (logical, on the host, allocated,
// reclaimable). Read-only, so it is allowed while the instance runs; the last two are then left out (needs_shutdown).
func (s *server) handleDiskInfo(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok || !decode(w, r, &struct{}{}) {
		return
	}
	s.respondJob(w)(s.startJob(kindDiskInfo, i, func(ctx context.Context, j *job, b backend) (any, error) {
		s.setProgress(j, jobProgress{Step: "disk_info", Message: fmt.Sprintf("实例 %d 正在查看磁盘占用", i)})
		res, err := b.DiskInfo(ctx, i)
		if err != nil {
			return nil, err
		}
		s.setProgress(j, jobProgress{Step: "done", Message: fmt.Sprintf("实例 %d 的磁盘占用已查看", i), Done: progressTotal, Total: progressTotal})
		return res, nil
	}))
}

// handleDiskTrim is POST /api/instances/{i}/disk-trim: asks the booted guest to hand the space of deleted files back
// to the emulator (the file on the PC shrinks only after a compact).
func (s *server) handleDiskTrim(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok || !decode(w, r, &struct{}{}) {
		return
	}
	s.respondJob(w)(s.startJob(kindDiskTrim, i, func(ctx context.Context, j *job, b backend) (any, error) {
		s.setProgress(j, jobProgress{Step: "disk_trim", Message: fmt.Sprintf("实例 %d 正在回收磁盘空间", i)})
		res, err := b.DiskTrim(ctx, i)
		if err != nil {
			return nil, err
		}
		s.setProgress(j, jobProgress{Step: "done", Message: fmt.Sprintf("实例 %d 已回收磁盘空间", i), Done: progressTotal, Total: progressTotal})
		return res, nil
	}))
}

// handleDiskCompact is POST /api/instances/{i}/disk-compact: rewrites the shut-down instance's data disk file without
// the freed clusters. It copies the whole disk, so it takes minutes (the page offers no cancel; POST /api/jobs/{id}/cancel still ends it).
func (s *server) handleDiskCompact(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok || !decode(w, r, &struct{}{}) {
		return
	}
	s.respondJob(w)(s.startJob(kindDiskCompact, i, func(ctx context.Context, j *job, b backend) (any, error) {
		s.setProgress(j, jobProgress{Step: "disk_compact", Message: fmt.Sprintf("实例 %d 正在整理磁盘（重写数据盘文件，要几分钟）", i)})
		res, err := b.DiskCompact(ctx, i)
		if err != nil {
			return nil, err
		}
		s.setProgress(j, jobProgress{Step: "done", Message: fmt.Sprintf("实例 %d 已整理磁盘，释放 %s", i, humanBytes(res.FreedBytes)), Done: progressTotal, Total: progressTotal})
		return res, nil
	}))
}

// humanBytes is a size for a message: "1.5 GB", "320 MB", "12 KB".
func humanBytes(n int64) string {
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1f GB", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.0f MB", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.0f KB", float64(n)/k)
	}
	return fmt.Sprintf("%d B", n)
}
