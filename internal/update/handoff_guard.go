// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

const UpdatingMessage = "正在更新，换好后会自动打开管理器。"

const (
	// beginHandoffWait: how long the CLI retries the launch gate while nobody owns a hand-off.
	beginHandoffWait = 3 * time.Second
	// staleCancelAge: well past AfterPIDWait, the longest a CLI can still be about to read its cancel mark.
	staleCancelAge = 10 * time.Minute
)

type handoffOwner struct {
	ID      HandoffID `json:"id"`
	PID     uint32    `json:"pid"`
	Created uint64    `json:"created"`
	Root    string    `json:"root"`
}

func launchLockPath(r *root.Root) string {
	return filepath.Join(r.Dir, ".update", "manager-handoff.lock")
}
func ownerPath(r *root.Root) string                { return filepath.Join(r.Dir, ".update", "manager-handoff.json") }
func cancelPath(r *root.Root, id HandoffID) string { return handoffPath(r, id) + ".cancel" }

// ManagerStart serializes Windows manager startup with the entire CLI hand-off.
// The lock, not the marker or a PID, is authoritative. The OS releases it on a
// crash. Hold the returned lease until the manager's single-instance lock is taken.
func ManagerStart(r *root.Root) (release func(), updating bool, err error) {
	if !root.CurrentPlatform().Windows() {
		return func() {}, false, nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		lock, free, err := host.TryLock(launchLockPath(r))
		if err != nil {
			return nil, false, err
		}
		if free {
			// Stale metadata cannot strand a root, including a reused PID or broken JSON.
			os.Remove(ownerPath(r))
			removeStaleCancels(r, time.Now())
			return lock.Unlock, false, nil
		}
		if _, ok := liveHandoffOwner(r); ok {
			return nil, true, nil
		}
		// Another ordinary launch may be taking its single-instance lock, or the
		// CLI may still be publishing its owner. Neither is a stale-marker veto.
		if time.Now().After(deadline) {
			return nil, false, errs.New(errs.Busy, "管理器启动检查正被占用，请稍后再打开")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// HandoffGuard belongs to one CLI process. It excludes competing hand-offs and
// new managers from before waiting for the old manager through swap and recovery.
type HandoffGuard struct {
	root *root.Root
	id   HandoffID
	lock *host.FileLock
}

func BeginHandoff(r *root.Root, id HandoffID) (*HandoffGuard, error) {
	if _, err := ParseHandoffID(string(id)); err != nil {
		return nil, err
	}
	g := &HandoffGuard{root: r, id: id}
	if !root.CurrentPlatform().Windows() {
		return g, nil
	}
	// The manager that started us probes the gate every 50 ms (HandoffReady), and an ordinary launch holds it for a
	// moment: neither is another hand-off, so try again briefly. A live owner is one, and ends this at once.
	deadline := time.Now().Add(beginHandoffWait)
	var lock *host.FileLock
	for {
		l, free, err := host.TryLock(launchLockPath(r))
		if err != nil {
			return nil, err
		}
		if free {
			lock = l
			break
		}
		if _, ok := liveHandoffOwner(r); ok || time.Now().After(deadline) {
			return nil, errs.New(errs.Busy, UpdatingMessage)
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.lock = lock
	p, ok := host.FindProcess(uint32(os.Getpid()))
	if !ok || p.Created == 0 {
		lock.Unlock()
		return nil, errs.New(errs.Internal, "查不了更新进程的身份，新版没有换上")
	}
	b, err := json.Marshal(handoffOwner{id, p.PID, p.Created, realOrSame(r.Dir)})
	if err == nil {
		tmp := ownerPath(r) + ".tmp"
		defer os.Remove(tmp)
		if err = writeFileSync(tmp, b); err == nil {
			err = root.ReplaceWithRetry(tmp, ownerPath(r))
		}
	}
	if err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

func (g *HandoffGuard) Close() {
	if g.lock == nil {
		return
	}
	// Remove only while we still own the gate. A later owner can publish at once
	// after Unlock, and must never have its marker removed by this process.
	os.Remove(ownerPath(g.root))
	os.Remove(cancelPath(g.root, g.id))
	g.lock.Unlock()
	g.lock = nil
}

// HandoffReady is the manager's acknowledgement that its exact CLI hand-off has
// taken the launch gate, not merely a process that happened to survive startup.
func HandoffReady(r *root.Root, id HandoffID) bool {
	if !root.CurrentPlatform().Windows() {
		return false
	}
	held, err := host.ExclusiveLockHeld(launchLockPath(r))
	if err != nil || !held {
		return false
	}
	o, ok := liveHandoffOwner(r)
	return ok && o.ID == id
}

func liveHandoffOwner(r *root.Root) (handoffOwner, bool) {
	b, err := root.ReadFileShared(ownerPath(r))
	var o handoffOwner
	if err != nil || json.Unmarshal(b, &o) != nil || o.PID == 0 || o.Created == 0 ||
		!strings.EqualFold(o.Root, realOrSame(r.Dir)) {
		return o, false
	}
	if _, err := ParseHandoffID(string(o.ID)); err != nil {
		return o, false
	}
	return o, (host.Process{PID: o.PID, Created: o.Created}).Alive()
}

// CancelHandoff cancels only this hand-off's wait for the manager. IDs are never
// reused, so even a late request cannot cancel a retry or another owner.
func CancelHandoff(r *root.Root, id HandoffID) error {
	if _, err := ParseHandoffID(string(id)); err != nil {
		return err
	}
	path := cancelPath(r, id)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o600)
}

// Canceled: the manager took this hand-off back (CancelHandoff). Read where the
// answer decides something: the watcher of WaitContext may not have run yet when
// the manager canceled and exited before this process got here.
func (g *HandoffGuard) Canceled() bool {
	if g.lock == nil {
		return false
	}
	_, err := os.Stat(cancelPath(g.root, g.id))
	return err == nil
}

// removeStaleCancels deletes cancel marks nobody will read any more: of a CLI
// that never started, ended at once, or had finished already. Called with the
// launch gate held, so no hand-off is under way; the age leaves alone a mark
// whose CLI is still starting (it waits for its manager for AfterPIDWait).
func removeStaleCancels(r *root.Root, now time.Time) {
	dir := filepath.Dir(handoffPath(r, ""))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json.cancel") {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > staleCancelAge {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// WaitContext ends on cancellation by the still-running manager as well as CLI
// cancellation. Once the old manager exits the install owns the outcome.
func (g *HandoffGuard) WaitContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if g.lock == nil {
		return ctx, cancel
	}
	if g.Canceled() {
		cancel()
		return ctx, cancel
	}
	go func() {
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Stat(cancelPath(g.root, g.id)); err == nil {
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
	return ctx, cancel
}
