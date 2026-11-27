//go:build windows

package emu

import (
	"context"
	"strings"
	"sync"
	"testing"

	"modavd/internal/adb"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
)

func TestNotificationSoundUnset(t *testing.T) {
	for out, want := range map[string]bool{
		"Deleted 1 rows\nnull\n":     true,
		"Deleted 0 rows\r\nnull\r\n": true,
		"null":                       true,
		"Deleted 1 rows\ncontent://media/internal/audio/media/184?title=Pixie%20Dust&canonical=1\n": false,
		"":                                 false,
		"Error: Unknown command: delete\n": false,
		"null\nsomething\n":                false,
	} {
		if got := notificationSoundUnset(out); got != want {
			t.Errorf("notificationSoundUnset(%q) = %v, want %v", out, got, want)
		}
	}
}

// fakeSoundGuest is the guest shell of the tests: it answers the mute command with out / code and counts the calls.
type fakeSoundGuest struct {
	mu       sync.Mutex
	out      string
	code     int
	commands []string
}

func (f *fakeSoundGuest) run(command string) adb.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	return adb.Result{Code: f.code, Out: f.out}
}

func (f *fakeSoundGuest) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.commands)
}

func newSoundTestManager(t *testing.T) (*Manager, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var warnings []string
	m := New(managerTestRoot(t, 1), logx.New(func(level logx.Level, message string) {
		if level == logx.Warn {
			mu.Lock()
			warnings = append(warnings, message)
			mu.Unlock()
		}
	}))
	setInitialized(t, m, 1, true)
	return m, &warnings
}

func setInitialized(t *testing.T, m *Manager, i int, on bool) {
	t.Helper()
	if err := m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set("initialized", jsonord.NewBool(on))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The first boot of a new instance (not initialized yet) leaves the setting alone: the system sets its default tones
// when its first media scan ends, which may put the default back. The next boot does it.
func TestMuteNotificationSoundWaitsForFirstBoot(t *testing.T) {
	m, _ := newSoundTestManager(t)
	setInitialized(t, m, 1, false)
	guest := &fakeSoundGuest{out: "Deleted 1 rows\nnull\n"}
	m.muteNotificationSound(context.Background(), 1, guest.run)
	if guest.calls() != 0 || m.notificationSoundDone(1) {
		t.Fatalf("first boot: commands %q, marker %v", guest.commands, m.notificationSoundDone(1))
	}
	setInitialized(t, m, 1, true)
	m.muteNotificationSound(context.Background(), 1, guest.run)
	if guest.calls() != 1 || !m.notificationSoundDone(1) {
		t.Fatalf("next boot: commands %q, marker %v", guest.commands, m.notificationSoundDone(1))
	}
}

// The sound is turned off once: the marker is written, and a later boot does not touch the setting again (a tone the
// user chose meanwhile stays).
func TestMuteNotificationSoundOnce(t *testing.T) {
	m, warnings := newSoundTestManager(t)
	ctx := context.Background()
	guest := &fakeSoundGuest{out: "Deleted 1 rows\nnull\n"}
	if m.notificationSoundDone(1) {
		t.Fatal("a new instance is marked as done")
	}
	m.muteNotificationSound(ctx, 1, guest.run)
	if guest.calls() != 1 || guest.commands[0] != muteNotificationSoundCommand {
		t.Fatalf("first boot: commands %q", guest.commands)
	}
	if !m.notificationSoundDone(1) {
		t.Fatal("no marker after success")
	}
	m.muteNotificationSound(ctx, 1, guest.run)
	m.muteNotificationSound(ctx, 1, guest.run)
	if guest.calls() != 1 {
		t.Fatalf("later boots ran the guest command again: %q", guest.commands)
	}
	if m.notificationSoundDone(0) || len(*warnings) != 0 {
		t.Fatalf("other instance done %v, warnings %q", m.notificationSoundDone(0), *warnings)
	}
}

// An instance whose setting is already unset is only marked; a failed attempt (the setting is still there, a non-zero
// exit, a cancelled start) leaves no marker, so the next boot tries again.
func TestMuteNotificationSoundRetries(t *testing.T) {
	ctx := context.Background()
	still := "Deleted 0 rows\ncontent://media/internal/audio/media/184?title=Pixie%20Dust&canonical=1\n"
	for name, c := range map[string]struct {
		out    string
		code   int
		cancel bool
		done   bool
		warn   string
	}{
		"already unset":     {out: "Deleted 0 rows\nnull\n", done: true},
		"setting still set": {out: still, warn: "没关掉通知提示音"},
		"adb failed":        {out: "adb: device offline", code: 1, warn: "device offline"},
		"no output":         {code: 255, warn: "退出码 255"},
		"canceled":          {out: "Deleted 1 rows\nnull\n", cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			m, warnings := newSoundTestManager(t)
			guest := &fakeSoundGuest{out: c.out, code: c.code}
			runCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			if c.cancel {
				cancel()
			}
			m.muteNotificationSound(runCtx, 1, guest.run)
			if got := m.notificationSoundDone(1); got != c.done {
				t.Fatalf("marker = %v, want %v", got, c.done)
			}
			if c.warn == "" && len(*warnings) != 0 || c.warn != "" && !strings.Contains(strings.Join(*warnings, "|"), c.warn) {
				t.Fatalf("warnings %q, want one with %q", *warnings, c.warn)
			}
			if !c.done {
				guest.out, guest.code = "Deleted 1 rows\nnull\n", 0
				m.muteNotificationSound(ctx, 1, guest.run)
				if !m.notificationSoundDone(1) || guest.calls() != 2 {
					t.Fatalf("the next boot did not mute: done %v, calls %d", m.notificationSoundDone(1), guest.calls())
				}
			}
		})
	}
}
