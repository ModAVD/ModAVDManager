package ui

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"modavd/internal/bosskey"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

// Boss keys and the tray icon (ModAVD/ModAVD#101). On Windows the manager keeps an icon in the notification area and
// registers the instances' boss keys as global hot keys (RegisterHotKey): a combination can be registered by one
// process only, and the manager is the one process that stays (closing its window leaves it in the tray; 退出 in the
// icon's menu ends it). A key is registered only while some instance using it runs with a window, so it is not held
// from other programs for nothing; one key used by several instances is registered once, and a press hides or shows
// all of them (emu.ToggleBossKey). A key another program holds is shown on the instance's settings page.

// trayHost is the tray icon with the hot keys (tray_windows.go: win.Tray); tests inject a fake.
type trayHost interface {
	RegisterHotKey(id int, k bosskey.Key) error
	UnregisterHotKey(id int)
	// TryHotKey registers k and lets go of it at once: nil when it could be registered now.
	TryHotKey(k bosskey.Key) error
	Close()
}

// trayEvents are delivered in order off the native window thread.
type trayEvents struct {
	hotKey                         func(id int)
	click, doubleClick, open, exit func()
}

type newTrayFunc func(tip string, ev trayEvents) (trayHost, error)

// bossKeys is the registration of the keys in use: one hot key id per key.
type bossKeys struct {
	mu    sync.Mutex
	host  trayHost
	log   *logx.Logger
	ids   map[string]int  // key -> id, registered now
	taken map[string]bool // another program has it: its last register or try failed
}

func newBossKeys(host trayHost, log *logx.Logger) *bossKeys {
	return &bossKeys{host: host, log: log, ids: map[string]int{}, taken: map[string]bool{}}
}

// freeID is the smallest hot key id not in use (1 on; 0 is left alone).
func (b *bossKeys) freeID() int {
	used := map[int]bool{}
	for _, id := range b.ids {
		used[id] = true
	}
	id := 1
	for used[id] {
		id++
	}
	return id
}

// sync makes the registered keys the wanted ones: releases the others, registers the new ones, and tries again the
// wanted keys another program had (it may have let go). Changes are logged once, not on every round.
func (b *bossKeys) sync(want map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, id := range b.ids {
		if !want[key] {
			b.host.UnregisterHotKey(id)
			delete(b.ids, key)
			b.log.Infof("老板键 %s 没有带窗口运行的实例在用，已注销", key)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if _, ok := b.ids[key]; ok || key == "" {
			continue
		}
		k, err := bosskey.Parse(key)
		if err != nil {
			continue // emu reads only valid keys
		}
		id := b.freeID()
		if err := b.host.RegisterHotKey(id, k); err != nil {
			if !b.taken[key] {
				b.log.Warnf("老板键 %s 注册不上，可能被别的程序占了，在这台实例的设置里换一个: %v", key, err)
			}
			b.taken[key] = true
			continue
		}
		if b.taken[key] {
			b.log.Infof("老板键 %s 现在注册上了", key)
		}
		delete(b.taken, key)
		b.ids[key] = id
		b.log.Infof("已注册老板键 %s", key)
	}
}

// keyOf is the key registered as id ("" when none).
func (b *bossKeys) keyOf(id int) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, kid := range b.ids {
		if kid == id {
			return key
		}
	}
	return ""
}

// check tries key now (a canonical key; "" = none): taken when another program has it. A key this manager holds
// is not taken: several instances may share one.
func (b *bossKeys) check(key string) bool {
	if key == "" {
		return false
	}
	k, err := bosskey.Parse(key)
	if err != nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.ids[key]; ok {
		return false
	}
	if err := b.host.TryHotKey(k); err != nil {
		b.taken[key] = true
		return true
	}
	delete(b.taken, key)
	return false
}

// takenKeys are the keys another program holds, as far as the manager found out.
func (b *bossKeys) takenKeys() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []string{}
	for key := range b.taken {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

// close lets go of every key and removes the icon.
func (b *bossKeys) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, id := range b.ids {
		b.host.UnregisterHotKey(id)
		delete(b.ids, key)
	}
	b.host.Close()
}

// ---- the server's side ------------------------------------------------------------------------------------------

// startTray puts up the icon and starts registering the boss keys. A tray that cannot start is only logged: the
// manager then runs as before (it exits with its window) and has no boss keys.
func (s *server) startTray(newTray newTrayFunc) {
	tip := "ModAVD 管理器 · " + s.root.Dir
	host, err := newTray(tip, trayEvents{hotKey: s.onHotKey, click: s.onTrayClick, doubleClick: s.onTrayDoubleClick, open: s.onTrayOpen, exit: s.onTrayExit})
	if err != nil {
		s.log.Warnf("放不了托盘图标，老板键也用不了；关掉窗口管理器就退出: %v", err)
		return
	}
	s.tray = host
	s.boss = newBossKeys(host, s.log)
	s.trayExit = make(chan struct{})
	s.log.Infof("管理器图标已放进托盘；关掉窗口后管理器留在托盘，老板键照常能用")
	go s.bossLoop()
}

// bossLoop keeps the registered keys in step with the instances: every pollEvery, and at once after a setting
// changed. It asks for processes and instance files only, no adb, so it runs whether or not a page is open.
func (s *server) bossLoop() {
	t := time.NewTicker(s.pollEvery)
	defer t.Stop()
	for {
		s.syncBossKeys()
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.bossKick:
		}
	}
}

func (s *server) kickBossKeys() {
	select {
	case s.bossKick <- struct{}{}:
	default:
	}
}

func (s *server) syncBossKeys() {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	targets, err := s.poll.BossKeyTargets(ctx)
	if err != nil {
		if s.ctx.Err() == nil {
			s.log.Debugf("老板键：读不了实例: %v", err)
		}
		return
	}
	want := map[string]bool{}
	for _, t := range targets {
		if t.Windowed && t.BossKey != "" {
			want[t.BossKey] = true
		}
	}
	s.boss.sync(want)
}

// onHotKey is a press of a registered key: the instances using it are hidden or shown, one press at a time.
func (s *server) onHotKey(id int) {
	key := s.boss.keyOf(id)
	if key == "" {
		return
	}
	s.toggleMu.Lock()
	defer s.toggleMu.Unlock()
	if s.trayStopped {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if _, err := s.mgr.ToggleBossKey(ctx, key); err != nil && s.ctx.Err() == nil {
		s.log.Warnf("老板键 %s 没切换成: %s", key, errs.As(err).Message)
	}
}

// onTrayClick brings every hidden window back.
func (s *server) onTrayClick() {
	s.toggleMu.Lock()
	defer s.toggleMu.Unlock()
	if s.trayStopped {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if _, err := s.mgr.ShowHiddenWindows(ctx); err != nil && s.ctx.Err() == nil {
		s.log.Warnf("没能显示藏起来的窗口: %s", errs.As(err).Message)
	}
}

// onTrayDoubleClick toggles every manager Edge window, or opens one when none exists.
func (s *server) onTrayDoubleClick() { s.trayManagerWindow(true) }

// onTrayOpen restores existing manager windows before opening a new one.
func (s *server) onTrayOpen() { s.trayManagerWindow(false) }

func (s *server) trayManagerWindow(toggle bool) {
	s.toggleMu.Lock()
	defer s.toggleMu.Unlock()
	if s.trayStopped || s.base == "" {
		return
	}
	if found, err := s.managerWindows(toggle); err != nil {
		s.log.Warnf("没能切换管理器窗口: %v", err)
		return
	} else if found {
		return
	}
	if _, err := s.launch(s.base + "?t=" + s.token); err != nil {
		s.log.Errorf("打不开管理器窗口: %v", err)
	}
}

// onTrayExit asks the manager to exit (waitForExit); the hidden windows are shown on the way out (stopTray).
func (s *server) onTrayExit() {
	s.trayExitOnce.Do(func() {
		s.log.Infof("在托盘菜单里点了退出")
		close(s.trayExit)
	})
}

// stopTray, when the manager exits: every hidden window is shown first (nothing would be left to bring it back),
// then the keys are released and the icon removed.
func (s *server) stopTray() {
	if s.tray == nil {
		return
	}
	s.toggleMu.Lock()
	if s.trayStopped {
		s.toggleMu.Unlock()
		return
	}
	// Already-dispatched callbacks may arrive after Close. None may hide windows again after cleanup.
	s.trayStopped = true
	if _, err := s.managerWindows(false); err != nil {
		s.log.Warnf("退出前没能显示管理器窗口: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if shown, err := s.mgr.ShowHiddenWindows(ctx); err != nil {
		s.log.Warnf("退出前没能显示藏起来的窗口: %s", errs.As(err).Message)
	} else if len(shown) > 0 {
		s.log.Infof("退出前显示了 %d 台实例藏起来的窗口", len(shown))
	}
	cancel()
	s.toggleMu.Unlock()
	s.boss.close()
}

// handleBossKeys is GET /api/boss-keys: whether this manager has boss keys, and the keys another program holds.
func (s *server) handleBossKeys(w http.ResponseWriter, r *http.Request) {
	out := struct {
		Supported bool     `json:"supported"`
		Taken     []string `json:"taken"`
	}{Taken: []string{}}
	if s.boss != nil {
		out.Supported, out.Taken = true, s.boss.takenKeys()
	}
	writeJSON(w, http.StatusOK, out)
}

// handleBossKeyCheck is POST /api/boss-keys/check {"key": "..."}: the key as it would be stored, and whether another
// program holds it right now (registered and let go at once).
func (s *server) handleBossKeyCheck(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key *string `json:"key"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Key == nil {
		writeError(w, errs.New(errs.Usage, "缺少 key"))
		return
	}
	if s.boss == nil {
		writeError(w, errs.New(errs.Unsupported, "这个管理器没有老板键（只有 Windows 有，或者托盘图标没放成）"))
		return
	}
	key, err := bosskey.Normalize(*body.Key)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Key   string `json:"key"`
		Taken bool   `json:"taken"`
	}{key, s.boss.check(key)})
}
