package emu

import (
	"modavd/internal/errs"
	"modavd/internal/win"
)

// compactBusy probes a kernel lock, never the existence of its persistent file.
// Compact takes disk before all lifecycle locks and releases it last. Its other
// acquisitions never wait, so a concurrent lifecycle operation can safely win.
func (m *Manager) compactBusy(i int) error {
	held, err := win.ExclusiveLockHeld(m.lockPath(i, "disk"))
	if err != nil {
		return errs.Wrap(errs.Internal, err, "实例磁盘锁文件打不开: "+m.lockPath(i, "disk"))
	}
	if held {
		return errs.Newf(errs.Busy, "魔改 AVD 实例 %d 正在整理磁盘，请等整理结束后重试", i)
	}
	return nil
}

// tryOperationLock closes the check/lock race: if compact acquired disk after
// our first probe and then won kind, probe again before reporting another reason.
// A successful start/stop/bridge/create lock prevents compact from acquiring its
// full set. Other callers still receive the busy reason while compact is active.
func (m *Manager) tryOperationLock(i int, kind string) (*win.FileLock, bool, error) {
	if err := m.compactBusy(i); err != nil {
		return nil, false, err
	}
	l, ok, err := win.TryLock(m.lockPath(i, kind))
	if err != nil {
		return nil, false, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+m.lockPath(i, kind))
	}
	if !ok {
		err = m.compactBusy(i)
	}
	return l, ok, err
}
