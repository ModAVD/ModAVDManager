//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestTrayManagerWindowActions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		found   bool
		failure bool
	}{
		{"existing windows", true, false}, {"closed or default browser", false, false}, {"lookup failure", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, tray := trayHarness(t)
			h.s.base = "http://127.0.0.1:33400/"
			var modes []bool
			h.s.managerWindows = func(toggle bool) (bool, error) {
				modes = append(modes, toggle)
				if tc.failure {
					return false, errors.New("enumeration failed")
				}
				return tc.found, nil
			}
			launches := 0
			h.s.launch = func(url string) (*Window, error) {
				launches++
				if url != h.s.base+"?t="+testToken {
					t.Error("wrong login URL")
				}
				return &Window{}, nil
			}
			if tray.ev.doubleClick == nil {
				t.Fatal("double click not wired")
			}
			tray.ev.doubleClick()
			tray.ev.open()
			if len(modes) != 2 || !modes[0] || modes[1] {
				t.Fatalf("actions %v", modes)
			}
			want := 0
			if !tc.found && !tc.failure {
				want = 2
			}
			if launches != want {
				t.Fatalf("launches %d, want %d", launches, want)
			}
			h.f.mu.Lock()
			shown := h.f.showHidden
			h.f.mu.Unlock()
			if shown != 0 {
				t.Fatal("manager action restored instance windows")
			}
		})
	}
}

func TestTrayManagerRestoreOnEveryExit(t *testing.T) {
	for _, reason := range []string{"context", "update handoff", "tray menu"} {
		for _, hidden := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/hidden=%v", reason, hidden), func(t *testing.T) {
				h, tray := trayHarness(t)
				s := h.s
				s.base = "http://127.0.0.1:33401/"
				s.exitEvery = time.Millisecond
				visible := true
				s.managerWindows = func(toggle bool) (bool, error) {
					if toggle {
						visible = !visible
					} else {
						visible = true
					}
					return true, nil
				}
				if hidden {
					tray.ev.doubleClick()
					if visible {
						t.Fatal("not hidden")
					}
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				switch reason {
				case "context":
					cancel()
				case "update handoff":
					close(s.exitReq)
				case "tray menu":
					tray.ev.exit()
				}
				s.waitForExit(ctx, nil)
				s.stopTray()
				if !visible {
					t.Fatal("exit left manager hidden")
				}
				tray.ev.doubleClick()
				tray.ev.open()
				tray.ev.click()
				s.stopTray()
				if !visible {
					t.Fatal("late callback hid manager")
				}
				h.f.mu.Lock()
				shown := h.f.showHidden
				h.f.mu.Unlock()
				if shown != 1 {
					t.Fatalf("restore calls %d, want one exit cleanup", shown)
				}
			})
		}
	}
}

func TestTrayManagerConcurrentExit(t *testing.T) {
	h, tray := trayHarness(t)
	s := h.s
	s.base = "http://127.0.0.1:33402/"
	entered, release := make(chan struct{}), make(chan struct{})
	visible := true
	s.managerWindows = func(toggle bool) (bool, error) {
		if toggle {
			close(entered)
			<-release
			visible = false
		} else {
			visible = true
		}
		return true, nil
	}
	var wg sync.WaitGroup
	wg.Go(tray.ev.doubleClick)
	<-entered
	wg.Go(s.stopTray)
	close(release)
	wg.Wait()
	if !visible {
		t.Fatal("concurrent exit left manager hidden")
	}
	tray.ev.doubleClick()
	if !visible {
		t.Fatal("callback after cleanup hid manager")
	}
}
