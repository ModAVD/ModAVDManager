//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

// TestTrayManagerEdgeProbe is opt-in because it opens real Edge windows. Use an isolated LOCALAPPDATA and set
// MODAVD_INTERACTIVE_LOCK to the shared desktop-test lock path. No emulator or existing browser profile is used.
func TestTrayManagerEdgeProbe(t *testing.T) {
	if os.Getenv("MODAVD_TRAY_EDGE_PROBE") != "1" {
		t.Skip("MODAVD_TRAY_EDGE_PROBE=1 opens real Edge windows")
	}
	lockPath := os.Getenv("MODAVD_INTERACTIVE_LOCK")
	if lockPath == "" {
		t.Fatal("MODAVD_INTERACTIVE_LOCK is required")
	}
	lockName, err := syscall.UTF16PtrFromString(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	var lock syscall.Handle
	deadline := time.Now().Add(3 * time.Minute)
	for {
		lock, err = syscall.CreateFile(lockName, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, 0, 0)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("interactive lock: %v", err)
		}
		time.Sleep(200 * time.Millisecond)
	}
	defer syscall.CloseHandle(lock)
	t.Log("exclusive interactive lock acquired")
	if FindEdge() == "" {
		t.Fatal("Edge is required for this probe")
	}
	t.Setenv("LOCALAPPDATA", testutil.TempDir(t))
	profile, foreign := EdgeProfileDir(), EdgeProfileDir()+"-other"
	user32 := syscall.NewLazyDLL("user32.dll")
	post := user32.NewProc("PostMessageW")
	foreground := user32.NewProc("GetForegroundWindow")
	getWindows := func(profile string) []win.EdgeWindow {
		t.Helper()
		w, e := win.EdgeProfileWindows(profile)
		if e != nil {
			t.Fatal(e)
		}
		return w
	}
	wait := func(what string, condition func() bool) {
		t.Helper()
		end := time.Now().Add(15 * time.Second)
		for !condition() {
			if time.Now().After(end) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	// Restore before closing, and only ever touch the two profiles created by this test.
	defer func() {
		for _, p := range []string{profile, foreign} {
			ws, e := win.EdgeProfileWindows(p)
			if e != nil {
				t.Error(e)
				continue
			}
			for _, w := range ws {
				_ = w.SetVisible(true)
				post.Call(w.HWND(), 0x0010, 0, 0)
			} // WM_CLOSE
			// Keep the interactive lock until the test windows have actually gone, not just until WM_CLOSE was posted.
			until := time.Now().Add(5 * time.Second)
			for {
				left, e := win.EdgeProfileWindows(p)
				if e != nil {
					t.Error(e)
					break
				}
				if len(left) == 0 {
					break
				}
				if time.Now().After(until) {
					t.Errorf("test profile still has %d windows during cleanup", len(left))
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	}()
	ln, err := net.Listen("tcp", "127.0.0.1:33430")
	if err != nil {
		t.Fatal(err)
	}
	var pages atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		pages.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<!doctype html><title>ModAVD tray probe</title><input value='page state retained'>")
	})}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String() + "/"
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	s, err := newServer(Options{Root: r, newBackend: func(l *logx.Logger) backend { return fakeBackend{f, l} }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.jobCancel()
	defer s.cancel()
	s.base = base
	var events trayEvents
	s.startTray(func(tip string, ev trayEvents) (trayHost, error) { events = ev; return realNewTray(tip, ev) })
	if s.tray == nil {
		t.Fatal("real tray did not start")
	}
	defer s.stopTray()
	tray := s.tray.(winTray)
	var launches atomic.Int32
	s.launch = func(url string) (*Window, error) { launches.Add(1); return LaunchWindow(url) }
	first, err := LaunchWindow(base + "?one")
	if err != nil || !first.Edge {
		t.Fatalf("first launch: %v", err)
	}
	wait("first Edge window", func() bool { return len(getWindows(profile)) == 1 })
	second, err := LaunchWindow(base + "?two")
	if err != nil || !second.Edge {
		t.Fatalf("second launch: %v", err)
	}
	wait("two Edge windows", func() bool { return len(getWindows(profile)) == 2 })
	select {
	case <-second.Done:
		t.Log("second launch exited after handing window to existing profile process")
	case <-time.After(5 * time.Second):
		t.Fatal("second launch did not exercise process handoff")
	}
	other := exec.Command(FindEdge(), edgeArgs(base+"?foreign", foreign)...)
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	go other.Wait()
	wait("foreign Edge window", func() bool { return len(getWindows(foreign)) == 1 })
	handles := func(ws []win.EdgeWindow) []uintptr {
		out := []uintptr{}
		for _, w := range ws {
			out = append(out, w.HWND())
		}
		slices.Sort(out)
		return out
	}
	owned := handles(getWindows(profile))
	// Posted messages are not real input. Windows may leave focus with another app even when visibility changed.
	// Set MODAVD_TRAY_REQUIRE_FOREGROUND=1 for a controlled interactive acceptance run.
	checkForeground := func() {
		t.Helper()
		until := time.Now().Add(time.Second)
		var h uintptr
		for {
			h, _, _ = foreground.Call()
			if slices.Contains(owned, h) {
				t.Logf("foreground observed on owned HWND %d", h)
				return
			}
			if time.Now().After(until) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if os.Getenv("MODAVD_TRAY_REQUIRE_FOREGROUND") == "1" {
			t.Fatalf("foreground not observed; current HWND %d", h)
		}
		t.Logf("foreground not observed after posted messages; current HWND %d; visibility assertions remain required", h)
	}
	unrelated := handles(getWindows(foreign))
	t.Logf("owned HWNDs %v; foreign HWNDs %v", owned, unrelated)
	wait("all pages loaded", func() bool { return pages.Load() >= 3 })
	loads := pages.Load()
	double := func() {
		for _, msg := range []uintptr{0x0202, 0x0203, 0x0202} {
			post.Call(tray.HWND(), 0x8001, 1, msg)
		}
	}
	check := func(visible bool) bool {
		ws := getWindows(profile)
		if len(ws) != 2 {
			return false
		}
		for _, w := range ws {
			if w.Visible() != visible {
				return false
			}
		}
		return true
	}
	for round := 1; round <= 3; round++ {
		double()
		wait("both manager windows hidden", func() bool { return check(false) })
		if round == 2 {
			events.open()
		} else {
			double()
		}
		wait("both manager windows restored", func() bool { return check(true) })
		if !slices.Equal(handles(getWindows(profile)), owned) {
			t.Fatal("toggle replaced manager HWNDs")
		}
		if !slices.Equal(handles(getWindows(foreign)), unrelated) || !getWindows(foreign)[0].Visible() {
			t.Fatal("foreign profile affected")
		}
		checkForeground()
		t.Logf("round %d: hidden/restored both windows; foreign unchanged", round)
	}
	// IsWindowVisible is true even while minimized. Wait for SW_RESTORE itself before foreground activation.
	minimized := getWindows(profile)[0]
	user32.NewProc("ShowWindowAsync").Call(minimized.HWND(), 6) // SW_MINIMIZE
	wait("minimized manager window", minimized.Minimized)
	events.open()
	wait("minimized window restored", func() bool { return minimized.Visible() && !minimized.Minimized() })
	checkForeground()
	t.Log("right-click open restored minimized window")
	user32.NewProc("ShowWindowAsync").Call(minimized.HWND(), 3) // SW_MAXIMIZE
	zoomed := func() bool { r, _, _ := user32.NewProc("IsZoomed").Call(minimized.HWND()); return r != 0 }
	wait("maximized manager window", zoomed)
	double()
	wait("maximized window hidden", func() bool { return check(false) })
	double()
	wait("maximized window restored", func() bool { return check(true) && zoomed() })
	t.Log("hide/show retained maximized placement")
	if launches.Load() != 0 {
		t.Fatal("restore opened an extra manager")
	}
	if pages.Load() != loads {
		t.Fatalf("toggle reloaded page: %d -> %d", loads, pages.Load())
	}
	f.mu.Lock()
	single := f.showHidden
	f.mu.Unlock()
	if single != 0 {
		t.Fatal("double click leaked instance restore")
	}
	// A single click really traverses the native timer to the server backend.
	post.Call(tray.HWND(), 0x8001, 1, 0x0202)
	wait("single-click instance restore", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.showHidden == 1 })
	for _, w := range getWindows(profile) {
		post.Call(w.HWND(), 0x0010, 0, 0)
	}
	wait("manager windows closed", func() bool { return len(getWindows(profile)) == 0 })
	double()
	wait("no-window double click opened manager", func() bool { return launches.Load() == 1 && len(getWindows(profile)) == 1 })
	t.Log("no-window double click opened one manager window")
	double()
	wait("new manager window hidden", func() bool { ws := getWindows(profile); return len(ws) == 1 && !ws[0].Visible() })
	s.stopTray()
	wait("exit restored manager window", func() bool { ws := getWindows(profile); return len(ws) == 1 && ws[0].Visible() })
	events.doubleClick()
	events.open()
	if launches.Load() != 1 || !getWindows(profile)[0].Visible() {
		t.Fatal("late callback changed window after exit")
	}
	t.Logf("exit restored window; page requests before close %d; isolated single click delivered once", loads)
	// Record only process identities belonging to our profiles; no unrelated command line is logged.
	ps, _ := win.ListProcesses()
	for _, p := range ps {
		if strings.EqualFold(p.Name, "msedge.exe") {
			args, e := p.CommandLine()
			if e == nil {
				for _, a := range args {
					if a == "--user-data-dir="+profile {
						t.Logf("profile process PID %d created %d", p.PID, p.Created)
					}
				}
			}
		}
	}
}
