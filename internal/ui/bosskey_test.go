//go:build windows

package ui

import (
	"errors"
	"maps"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"modavd/internal/bosskey"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

// fakeTray stands for win.Tray: registered keys by id, keys another program holds, and what was tried.
type fakeTray struct {
	mu     sync.Mutex
	reg    map[int]string
	held   map[string]bool // "another program" has these
	tries  []string
	closed bool
	ev     trayEvents
}

var errFakeTaken = errors.New("hot key already registered")

func (f *fakeTray) taken(key string) bool {
	if f.held[key] {
		return true
	}
	return slices.Contains(slices.Collect(maps.Values(f.reg)), key)
}

func (f *fakeTray) RegisterHotKey(id int, k bosskey.Key) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.taken(k.String()) || f.reg[id] != "" {
		return errFakeTaken
	}
	f.reg[id] = k.String()
	return nil
}

func (f *fakeTray) UnregisterHotKey(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.reg, id)
}

func (f *fakeTray) TryHotKey(k bosskey.Key) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries = append(f.tries, k.String())
	if f.taken(k.String()) {
		return errFakeTaken
	}
	return nil
}

func (f *fakeTray) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	clear(f.reg)
}

func (f *fakeTray) registered() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(maps.Values(f.reg))
}

func (f *fakeTray) idOf(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, k := range f.reg {
		if k == key {
			return id
		}
	}
	return 0
}

func (f *fakeTray) setHeld(key string, on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[key] = on
}

func trayHarness(t *testing.T) (*harness, *fakeTray) {
	t.Helper()
	tray := &fakeTray{reg: map[int]string{}, held: map[string]bool{}}
	h := newHarness(t, func(o *Options) {
		o.Tray = true
		o.newTray = func(tip string, ev trayEvents) (trayHost, error) {
			tray.ev = ev
			return tray, nil
		}
	})
	return h, tray
}

func (h *harness) setTargets(targets ...emu.BossKeyTarget) {
	h.f.mu.Lock()
	h.f.bossTargets = targets
	h.f.mu.Unlock()
	h.s.kickBossKeys()
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A key is registered once however many instances use it, only while one of them runs with a window, released when
// none does; a key another program holds is retried and reported as taken.
func TestBossKeyRegistration(t *testing.T) {
	h, tray := trayHarness(t)
	const k1, k9 = "Ctrl+Alt+Shift+1", "Ctrl+Alt+Shift+9"
	is := func(want ...string) func() bool {
		return func() bool { return slices.Equal(tray.registered(), want) }
	}
	// nothing runs with a window: nothing registered (a headless one does not count)
	h.setTargets(emu.BossKeyTarget{Index: 1, BossKey: k1}, emu.BossKeyTarget{Index: 2, BossKey: k1, PID: 7})
	time.Sleep(150 * time.Millisecond)
	if r := tray.registered(); len(r) != 0 {
		t.Fatalf("registered with no windowed instance: %v", r)
	}
	// two windowed instances on one key, one on another, one with no key: two registrations
	h.setTargets(emu.BossKeyTarget{Index: 1, BossKey: k1, Windowed: true, PID: 1}, emu.BossKeyTarget{Index: 2, BossKey: k1, Windowed: true, PID: 2},
		emu.BossKeyTarget{Index: 3, BossKey: k9, Windowed: true, PID: 3}, emu.BossKeyTarget{Index: 4, Windowed: true, PID: 4})
	waitUntil(t, "both keys", is(k1, k9))
	// a press is the key's toggle, whatever id it got
	tray.ev.hotKey(tray.idOf(k9))
	tray.ev.hotKey(999) // an id we never registered: nothing
	h.f.mu.Lock()
	toggles := slices.Clone(h.f.toggles)
	h.f.mu.Unlock()
	if !slices.Equal(toggles, []string{k9}) {
		t.Fatalf("toggles %v", toggles)
	}
	// 3 stops: its key goes
	h.setTargets(emu.BossKeyTarget{Index: 1, BossKey: k1, Windowed: true, PID: 1}, emu.BossKeyTarget{Index: 2, BossKey: k1, Windowed: true, PID: 2},
		emu.BossKeyTarget{Index: 3, BossKey: k9})
	waitUntil(t, "k9 released", is(k1))

	// another program holds k9: not registered, reported, and taken by us once it lets go
	tray.setHeld(k9, true)
	h.setTargets(emu.BossKeyTarget{Index: 3, BossKey: k9, Windowed: true, PID: 3})
	var keys struct {
		Supported bool     `json:"supported"`
		Taken     []string `json:"taken"`
	}
	waitUntil(t, "k9 taken", func() bool {
		expect(t, h.do("GET", "/api/boss-keys", nil), http.StatusOK, &keys)
		return keys.Supported && slices.Equal(keys.Taken, []string{k9})
	})
	if r := tray.registered(); len(r) != 0 {
		t.Fatalf("registered %v (k1 has no windowed instance now, k9 is held)", r)
	}
	tray.setHeld(k9, false)
	waitUntil(t, "k9 registered after release", is(k9))
	expect(t, h.do("GET", "/api/boss-keys", nil), http.StatusOK, &keys)
	if len(keys.Taken) != 0 {
		t.Fatalf("still taken: %v", keys.Taken)
	}
}

// The settings page: a key is tried when typed (check) and when saved (PATCH); one this manager holds for another
// instance is not taken; a cleared key is never taken.
func TestBossKeyCheckAndPatch(t *testing.T) {
	h, tray := trayHarness(t)
	type checked struct {
		Key   string `json:"key"`
		Taken bool   `json:"taken"`
	}
	var c checked
	expect(t, h.do("POST", "/api/boss-keys/check", map[string]any{"key": "shift+alt+f5"}), http.StatusOK, &c)
	if c.Key != "Alt+Shift+F5" || c.Taken {
		t.Fatalf("free key: %+v", c)
	}
	tray.setHeld("Alt+Shift+F5", true)
	expect(t, h.do("POST", "/api/boss-keys/check", map[string]any{"key": "Alt+Shift+F5"}), http.StatusOK, &c)
	if !c.Taken {
		t.Fatalf("held key: %+v", c)
	}
	expect(t, h.do("POST", "/api/boss-keys/check", map[string]any{"key": "none"}), http.StatusOK, &c)
	if c.Key != "" || c.Taken {
		t.Fatalf("none: %+v", c)
	}
	expectError(t, h.do("POST", "/api/boss-keys/check", map[string]any{"key": "Win+1"}), http.StatusBadRequest, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/boss-keys/check", map[string]any{"key": "1"}), http.StatusBadRequest, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/boss-keys/check", map[string]any{}), http.StatusBadRequest, errs.Usage)

	// held by us for a running instance: not taken for another one
	h.setTargets(emu.BossKeyTarget{Index: 1, BossKey: "Ctrl+Alt+Shift+1", Windowed: true, PID: 1})
	waitUntil(t, "registered", func() bool { return slices.Equal(tray.registered(), []string{"Ctrl+Alt+Shift+1"}) })
	expect(t, h.do("POST", "/api/boss-keys/check", map[string]any{"key": "Ctrl+Alt+Shift+1"}), http.StatusOK, &c)
	if c.Taken {
		t.Fatalf("our own key: %+v", c)
	}

	var res struct {
		emu.UpdateResult
		BossKeyTaken *bool `json:"boss_key_taken"`
	}
	expect(t, h.do("PATCH", "/api/instances/3", map[string]any{"boss_key": "alt+shift+f5"}), http.StatusOK, &res)
	if res.Instance.BossKey != "Alt+Shift+F5" || res.BossKeyTaken == nil || !*res.BossKeyTaken {
		t.Fatalf("patch to a held key: %+v", res)
	}
	expect(t, h.do("PATCH", "/api/instances/3", map[string]any{"boss_key": ""}), http.StatusOK, &res)
	if res.Instance.BossKey != "" || res.BossKeyTaken == nil || *res.BossKeyTaken {
		t.Fatalf("patch to none: %+v", res)
	}
	expectError(t, h.do("PATCH", "/api/instances/3", map[string]any{"boss_key": "Ctrl+Win+3"}), http.StatusBadRequest, errs.InvalidArgument)
	// other settings: no boss_key_taken
	var plain map[string]any
	expect(t, h.do("PATCH", "/api/instances/3", map[string]any{"fps": 30}), http.StatusOK, &plain)
	if _, ok := plain["boss_key_taken"]; ok {
		t.Fatalf("boss_key_taken without boss_key: %v", plain)
	}
}

// Without the tray (other platforms, or it could not start) there are no boss keys and the manager ends with its
// window as before.
func TestBossKeysUnsupported(t *testing.T) {
	h := newHarness(t, nil)
	var keys struct {
		Supported bool     `json:"supported"`
		Taken     []string `json:"taken"`
	}
	expect(t, h.do("GET", "/api/boss-keys", nil), http.StatusOK, &keys)
	if keys.Supported || keys.Taken == nil {
		t.Fatalf("%+v", keys)
	}
	expectError(t, h.do("POST", "/api/boss-keys/check", map[string]any{"key": "Ctrl+1"}), http.StatusUnprocessableEntity, errs.Unsupported)

	failing := newHarness(t, func(o *Options) {
		o.Tray = true
		o.newTray = func(string, trayEvents) (trayHost, error) { return nil, errors.New("no tray here") }
	})
	if failing.s.tray != nil || failing.s.boss != nil || failing.s.trayExit != nil {
		t.Fatal("a failed tray left state behind")
	}
}

// With the tray, a closed window and an idle page do not end the manager; 退出 in the menu does, after the running
// jobs, even with a page open; on the way out the hidden windows are shown and the keys released.
func TestTrayLifetime(t *testing.T) {
	h, tray := trayHarness(t)
	s := h.s
	s.exitEvery = 10 * time.Millisecond
	s.idleExit = 50 * time.Millisecond
	s.closeGrace = 20 * time.Millisecond
	edge := make(chan struct{})
	done := make(chan struct{})
	go func() {
		s.waitForExit(t.Context(), &Window{Edge: true, Started: time.Now().Add(-time.Minute), Done: edge})
		s.stopTray()
		close(done)
	}()
	close(edge) // the window closed
	select {
	case <-done:
		t.Fatal("exited with its window although it has the tray")
	case <-time.After(400 * time.Millisecond):
	}
	// a running job holds the exit back
	h.f.mu.Lock()
	h.f.stopGate = make(chan struct{})
	gate := h.f.stopGate
	h.f.mu.Unlock()
	var job jobView
	expect(t, h.do("POST", "/api/instances/4/stop", nil), http.StatusAccepted, &job)
	tray.ev.exit()
	tray.ev.exit() // twice is fine
	select {
	case <-done:
		t.Fatal("exited while a job ran")
	case <-time.After(200 * time.Millisecond):
	}
	close(gate)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit after 退出")
	}
	h.f.mu.Lock()
	shown := h.f.showHidden
	h.f.mu.Unlock()
	if shown != 1 || !tray.closed {
		t.Fatalf("on exit: hidden windows shown %d times, tray closed %v", shown, tray.closed)
	}

	// the icon itself: a click shows the hidden windows; 打开管理器 opens a window
	h2, tray2 := trayHarness(t)
	tray2.ev.click()
	h2.f.mu.Lock()
	clicks := h2.f.showHidden
	h2.f.mu.Unlock()
	if clicks != 1 {
		t.Fatalf("click showed %d times", clicks)
	}
	opened := make(chan string, 1)
	h2.s.base = "http://127.0.0.1:1/"
	h2.s.launch = func(url string) (*Window, error) { opened <- url; return nil, nil }
	tray2.ev.open()
	if u := <-opened; u != "http://127.0.0.1:1/?t="+testToken {
		t.Fatalf("opened %q", u)
	}
}

// The registration bookkeeping on its own: ids are reused, a failing key is logged once.
func TestBossKeysSyncLogsOnce(t *testing.T) {
	tray := &fakeTray{reg: map[int]string{}, held: map[string]bool{"Ctrl+F1": true}}
	logs := &logx.Collector{}
	b := newBossKeys(tray, logx.New(logs.Sink()))
	for range 3 {
		b.sync(map[string]bool{"Ctrl+F1": true, "Ctrl+F2": true})
	}
	if got := tray.registered(); !slices.Equal(got, []string{"Ctrl+F2"}) {
		t.Fatalf("registered %v", got)
	}
	if n := len(logs.Messages()); n != 1 {
		t.Fatalf("warnings %q", logs.Messages())
	}
	b.sync(map[string]bool{})
	b.sync(map[string]bool{"Ctrl+F3": true})
	if id := tray.idOf("Ctrl+F3"); id != 1 {
		t.Fatalf("id %d, want 1 reused", id)
	}
	b.close()
	if !tray.closed || len(tray.registered()) != 0 {
		t.Fatal("close left keys")
	}
}
