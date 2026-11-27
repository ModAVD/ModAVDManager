package emu

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

// A stop requests cancellation on disk so the creator, even in another process, also stops between boots and cannot
// report success after an interrupted final shutdown. The stop lock serializes requests with the start / end of an
// attempt; the create lock keeps its cancellation file from being reused by a later attempt.
func (m *Manager) createCancelPath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".create.cancel")
}

func (m *Manager) createCanceled(i int) bool { return fileOrDirExists(m.createCancelPath(i)) }

// lockCreation clears a dead attempt's cancellation before publishing the new attempt to Stop.
func (m *Manager) lockCreation(ctx context.Context, i int) (*host.FileLock, error) {
	stop, err := m.lock(ctx, i, "stop", stopLockWait, "正在关机")
	if err != nil {
		return nil, err
	}
	defer stop.Unlock()
	lock, err := m.tryInstanceLock(i, "create", "正在新建")
	if err != nil {
		return nil, err
	}
	if err := os.Remove(m.createCancelPath(i)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		lock.Unlock()
		return nil, errs.Wrap(errs.Internal, err, "清理新建取消标记失败")
	}
	return lock, nil
}

// watchCreateCancel stops the setup's context when another process requests cancellation. Its caller must stop
// and join the watcher before removing the file or releasing the create lock.
func (m *Manager) watchCreateCancel(ctx context.Context, cancel context.CancelFunc, i int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if m.createCanceled(i) {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return done
}

// finishCreation shares the stop lock with external cancellation: either a request wins and this fails, or this
// finishes first and a later stop sees a ready, offline instance. A late cancel can never be erased by success.
func (m *Manager) finishCreation(ctx context.Context, i int) error {
	stop, err := m.lock(ctx, i, "stop", stopLockWait, "正在关机")
	if err != nil {
		return err
	}
	defer stop.Unlock()
	if m.createCanceled(i) {
		return errs.New(errs.Canceled, "新建初始化已被关机打断")
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	return m.setCreation(i, "", "", "")
}
