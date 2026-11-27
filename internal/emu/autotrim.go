package emu

// The automatic trim of the data disk after boot (ModAVD/ModAVD#2, design §3.3 / §3.6): once the boot setup is done
// and the emulator advertises discard on /data, request reclamation of freed clusters (the same guest request as
// `modavd disk <i> trim`), at most once per 24 hours per instance, so nobody has to run it by hand.
//
// Strictly best effort and never in the boot's way: every problem (no discard, su failed, mas-avd.json unreadable or
// unwritable) is one log line and the step returns; nothing is returned to the start, nothing can panic out of it, and
// the guest commands have short timeouts of their own.

import (
	"context"
	"fmt"
	"time"

	"modavd/internal/jsonord"
)

const (
	// autoTrimInterval is the least time between two automatic trims of an instance.
	autoTrimInterval = 24 * time.Hour
	// autoTrimProbeTimeout bounds the read of the discard limit.
	autoTrimProbeTimeout = 10 * time.Second
	// autoTrimTimeout bounds the sm request, which the start waits for. It does not
	// bound or await vold's asynchronous FITRIM. See autoTrim for retry scheduling.
	autoTrimTimeout = 30 * time.Second
)

// diskTrimAtKey is instances.<i>.diskTrimAt in mas-avd.json: the Unix time in seconds of the last automatic trim
// attempt (an integer; absent or anything else: never).
const diskTrimAtKey = "diskTrimAt"

// shouldAutoTrim: an automatic trim is due when none has run yet (lastUnix <= 0), the interval has passed since the
// last one, or the clock stands before it (it was set back: waiting for the future would never end).
func shouldAutoTrim(lastUnix, nowUnix int64) bool {
	if lastUnix <= 0 || nowUnix < lastUnix {
		return true
	}
	return time.Duration(nowUnix-lastUnix)*time.Second >= autoTrimInterval
}

// readDiskTrimAt is the Unix time stored for instance i; 0 when there is none or it cannot be read.
func (m *Manager) readDiskTrimAt(i int) int64 {
	meta := m.Root.InstanceMeta(i)
	if meta == nil {
		return 0
	}
	n, _ := meta.Get(diskTrimAtKey).Int()
	return n
}

func (m *Manager) writeDiskTrimAt(i int, unix int64) error {
	return m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(diskTrimAtKey, jsonord.NewInt(unix))
		return nil
	})
}

// autoTrim is the boot step: trim instance i's data disk when it is due. It only logs. An instance this start merely
// adopted (setupAdopted) is left alone: a game may be running there, and a trim can stall the guest's disk for a
// moment (design §3.6: only after boot, before a game is up).
//
// The time is stored after the attempt whatever came of it (unless the start was canceled): a trim that fails or times
// out is retried after the interval, not at every boot, so the most a boot ever waits for it is autoTrimProbeTimeout +
// autoTrimTimeout once a day.
func (m *Manager) autoTrim(ctx context.Context, i int, mode bootSetup) {
	defer func() {
		if r := recover(); r != nil {
			m.Log.Warnf("实例 %d 开机后自动回收磁盘空间出错，已跳过: %v", i, r)
		}
	}()
	if mode == setupAdopted || ctx.Err() != nil || m.Root == nil {
		return
	}
	if !shouldAutoTrim(m.readDiskTrimAt(i), time.Now().Unix()) {
		return
	}
	maxBytes, ok := m.probeDiscardMax(ctx, i, autoTrimProbeTimeout)
	if ctx.Err() != nil {
		return
	}
	if !ok || maxBytes <= 0 {
		m.Log.Debugf("实例 %d 数据盘没有 discard（旧版模拟器），不自动回收磁盘空间", i)
		return
	}
	started := time.Now()
	res := m.runIdleMaint(ctx, i, autoTrimTimeout)
	if ctx.Err() != nil {
		return
	}
	if res.Code == 0 {
		m.Log.Infof("实例 %d 开机后已触发磁盘回收（命令往返 %.1f 秒，未等待客体回收完成）", i, time.Since(started).Seconds())
	} else {
		m.Log.Warnf("实例 %d 开机后触发磁盘回收失败，%.0f 小时后再试: %s", i, autoTrimInterval.Hours(),
			orText(tail(res.Out, 200), fmt.Sprintf("退出码 %d", res.Code)))
	}
	if err := m.writeDiskTrimAt(i, time.Now().Unix()); err != nil {
		m.Log.Warnf("实例 %d 记不下这次自动回收的时间（下次开机会再回收一次）: %v", i, err)
	}
}
