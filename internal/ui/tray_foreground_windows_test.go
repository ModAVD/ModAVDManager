//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"modavd/internal/testutil"
	"modavd/internal/win"
)

type foregroundProbeState struct {
	Command       string  `json:"command"`
	Success       bool    `json:"success"`
	Window        uintptr `json:"window"`
	Foreground    uintptr `json:"foreground"`
	ForegroundPID uint32  `json:"foreground_pid"`
	LastInput     uint32  `json:"last_input"`
	InputAgeMS    uint32  `json:"input_age_ms"`
	LockTimeoutMS uint32  `json:"lock_timeout_ms"`
}

func foregroundProbeSnapshot(command string, success bool, window uintptr) foregroundProbeState {
	u := syscall.NewLazyDLL("user32.dll")
	h, _, _ := u.NewProc("GetForegroundWindow").Call()
	var pid uint32
	u.NewProc("GetWindowThreadProcessId").Call(h, uintptr(unsafe.Pointer(&pid)))
	input := struct{ Size, Time uint32 }{Size: 8}
	u.NewProc("GetLastInputInfo").Call(uintptr(unsafe.Pointer(&input)))
	tick, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetTickCount64").Call()
	var timeout uint32
	u.NewProc("SystemParametersInfoW").Call(0x2000, 0, uintptr(unsafe.Pointer(&timeout)), 0)
	return foregroundProbeState{command, success, window, h, pid, input.Time, uint32(tick) - input.Time, timeout}
}

// The broker is a separate, test-owned native window. Only it can grant the test process foreground permission
// while it is foreground. No input is synthesized and no global policy or third-party window is modified.
func TestTrayForegroundBrokerHelper(t *testing.T) {
	if os.Getenv("MODAVD_FG_BROKER") != "1" {
		t.Skip("foreground probe child only")
	}
	parent, err := strconv.ParseUint(os.Getenv("MODAVD_FG_PARENT"), 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	tray, err := win.StartTray(win.TrayOptions{NoIcon: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	u := syscall.NewLazyDLL("user32.dll")
	// A temporary native test window. One actual click gives this process foreground eligibility.
	styleIndex := -16
	u.NewProc("SetWindowLongPtrW").Call(tray.HWND(), uintptr(styleIndex), 0x00cf0000)
	title, _ := syscall.UTF16PtrFromString("ModAVD foreground test")
	u.NewProc("SetWindowTextW").Call(tray.HWND(), uintptr(unsafe.Pointer(title)))
	u.NewProc("SetWindowPos").Call(tray.HWND(), 0, 40, 40, 420, 180, 0x0004|0x0020)
	send := func(command string, ok bool) {
		_ = json.NewEncoder(os.Stdout).Encode(foregroundProbeSnapshot(command, ok, tray.HWND()))
	}
	take := func() bool { r, _, _ := u.NewProc("SetForegroundWindow").Call(tray.HWND()); return r != 0 }
	send("ready", true)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		command := in.Text()
		var r uintptr
		switch command {
		case "prompt":
			topmost := -1
			r, _, _ = u.NewProc("SetWindowPos").Call(tray.HWND(), uintptr(topmost), 0, 0, 0, 0, 0x0040|0x0010|0x0002|0x0001)
		case "plain":
			normal := -2
			r, _, _ = u.NewProc("SetWindowPos").Call(tray.HWND(), uintptr(normal), 0, 0, 0, 0, 0x0010|0x0002|0x0001)
		case "take":
			send(command, take())
			continue
		case "grant":
			r, _, _ = u.NewProc("AllowSetForegroundWindow").Call(uintptr(parent))
		case "revoke":
			r, _, _ = u.NewProc("AllowSetForegroundWindow").Call(uintptr(os.Getpid()))
		case "quit":
			send(command, true)
			return
		default:
			t.Fatal("unknown probe command")
		}
		send(command, r != 0)
	}
}

func startForegroundBroker(t *testing.T) (int, uintptr, func(string) foregroundProbeState) {
	t.Helper()
	child := exec.Command(os.Args[0], "-test.run=^TestTrayForegroundBrokerHelper$")
	child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	child.Env = append(os.Environ(), "MODAVD_FG_BROKER=1", "MODAVD_FG_PARENT="+strconv.Itoa(os.Getpid()))
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		fmt.Fprintln(in, "quit")
		in.Close()
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = child.Process.Kill()
			<-done
		}
	})
	scan := bufio.NewScanner(out)
	if !scan.Scan() {
		t.Fatal("no broker response")
	}
	var state foregroundProbeState
	if err := json.Unmarshal(scan.Bytes(), &state); err != nil {
		t.Fatalf("broker response %q: %v", scan.Text(), err)
	}
	t.Logf("broker ready %+v", state)
	window := state.Window
	command := func(command string) foregroundProbeState {
		t.Helper()
		if _, err := fmt.Fprintln(in, command); err != nil {
			t.Fatal(err)
		}
		if !scan.Scan() {
			t.Fatal("broker stopped")
		}
		var reply foregroundProbeState
		if err := json.Unmarshal(scan.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Command != command || !reply.Success {
			t.Fatalf("broker %q: %+v", command, reply)
		}
		return reply
	}
	return child.Process.Pid, window, command
}

func TestTrayManagerForegroundPermissions(t *testing.T) {
	if os.Getenv("MODAVD_FOREGROUND_PROBE") != "1" {
		t.Skip("MODAVD_FOREGROUND_PROBE=1 opens test windows")
	}
	name, err := syscall.UTF16PtrFromString(os.Getenv("MODAVD_INTERACTIVE_LOCK"))
	if err != nil || os.Getenv("MODAVD_INTERACTIVE_LOCK") == "" {
		t.Fatal("MODAVD_INTERACTIVE_LOCK is required")
	}
	lock, err := syscall.CreateFile(name, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.CloseHandle(lock) })
	if FindEdge() == "" {
		t.Fatal("Edge is required")
	}
	t.Setenv("LOCALAPPDATA", testutil.TempDir(t))
	u := syscall.NewLazyDLL("user32.dll")
	post := u.NewProc("PostMessageW")
	allow := u.NewProc("AllowSetForegroundWindow")
	send := u.NewProc("SendMessageTimeoutW")
	wait := func(what string, cond func() bool) {
		t.Helper()
		until := time.Now().Add(5 * time.Second)
		for !cond() {
			if time.Now().After(until) {
				t.Fatalf("timeout %s: %+v", what, foregroundProbeSnapshot(what, false, 0))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	brokerPID, brokerWindow, command := startForegroundBroker(t)
	take := func() {
		t.Helper()
		command("take")
		wait("broker foreground", func() bool { return foregroundProbeSnapshot("", false, 0).Foreground == brokerWindow })
	}
	window, err := LaunchWindow("about:blank#modavd-foreground-probe")
	if err != nil || !window.Edge {
		t.Fatalf("Edge launch: %v", err)
	}
	// Register cleanup before searching: a browser that starts slowly or has unexpected windows still belongs to
	// this isolated profile and must be closed even when target discovery fails.
	defer func() {
		until := time.Now().Add(10 * time.Second)
		for {
			ws, e := win.EdgeProfileWindows(EdgeProfileDir())
			if e != nil {
				t.Error(e)
			} else {
				for _, w := range ws {
					_ = w.SetVisible(true)
					post.Call(w.HWND(), 0x0010, 0, 0)
				}
			}
			exited := false
			select {
			case <-window.Done:
				exited = true
			default:
			}
			if e == nil && len(ws) == 0 && exited {
				break
			}
			if time.Now().After(until) {
				t.Error("Edge did not close its isolated probe profile")
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		removeProfile(t, EdgeProfileDir()) // wait for this test profile's crash-reporting file handles too
	}()
	var windows []win.EdgeWindow
	wait("one Edge app", func() bool {
		windows, err = win.EdgeProfileWindows(EdgeProfileDir())
		if err != nil {
			t.Fatal(err)
		}
		return len(windows) == 1
	})
	target := windows[0]
	// Showing the prompt does not activate it. Require a new actual input event while this window is foreground.
	inputBefore := foregroundProbeSnapshot("before human click", false, brokerWindow).LastInput
	command("prompt")
	clickedUntil := time.Now().Add(90 * time.Second)
	for {
		state := foregroundProbeSnapshot("waiting for human click", false, brokerWindow)
		if state.Foreground == brokerWindow && state.LastInput != inputBefore {
			t.Logf("actual input observed on test broker: %+v", state)
			break
		}
		if time.Now().After(clickedUntil) {
			t.Fatal("test window was not clicked")
		}
		time.Sleep(50 * time.Millisecond)
	}
	command("plain")
	take()
	measure := func(phase string) foregroundProbeState {
		t.Helper()
		before := foregroundProbeSnapshot(phase, false, target.HWND())
		if !target.Visible() {
			t.Fatal("foreground target is no longer a visible window of the identified Edge process")
		}
		accepted := target.Foreground()
		var ignored uintptr
		ack, _, _ := send.Call(target.HWND(), 0, 0, 0, 0x0002, 1000, uintptr(unsafe.Pointer(&ignored)))
		after := foregroundProbeSnapshot(phase, accepted, target.HWND())
		b, _ := json.Marshal(before)
		a, _ := json.Marshal(after)
		t.Logf("native before=%s after=%s WM_NULL_ack=%v", b, a, ack != 0)
		if ack == 0 {
			t.Fatal("Edge activation queue did not acknowledge")
		}
		if before.LastInput != after.LastInput {
			t.Fatal("external input changed probe conditions")
		}
		return after
	}
	command("revoke")
	negative := measure("no explicit grant")
	if negative.Success {
		t.Log("no-grant denial was not reproduced; this run can verify granted activation but cannot prove a permission contrast")
		allow.Call(uintptr(brokerPID))
		take()
	}
	h, fake := trayHarness(t)
	h.s.base = "http://127.0.0.1:33431/"
	h.s.managerWindows = changeManagerWindows
	events := fake.ev
	barrier := make(chan struct{}, 8)
	tray, err := win.StartTray(win.TrayOptions{NoIcon: true, Events: win.TrayEvents{
		DoubleClick: events.doubleClick, HotKey: func(id int) {
			if id == 1 {
				events.open()
			} else {
				barrier <- struct{}{}
			}
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer tray.Close()
	flush := func() {
		t.Helper()
		post.Call(tray.HWND(), 0x0312, 2, 0)
		select {
		case <-barrier:
		case <-time.After(3 * time.Second):
			t.Fatal("tray callback did not finish")
		}
	}
	double := func() {
		for _, m := range []uintptr{0x0202, 0x0203, 0x0202} {
			post.Call(tray.HWND(), 0x8001, 1, m)
		}
		flush()
	}
	lendBack := func() {
		t.Helper()
		r, _, _ := allow.Call(uintptr(brokerPID))
		if r == 0 {
			t.Fatal("return foreground grant to broker failed")
		}
		take()
	}
	for round := 1; round <= 3; round++ {
		command("grant")
		granted := measure(fmt.Sprintf("round %d granted", round))
		if !granted.Success {
			t.Fatal("authorized native request failed")
		}
		if round == 1 && !negative.Success {
			if negative.LastInput != granted.LastInput {
				t.Fatal("input changed between denied and granted requests; causal contrast is invalid")
			}
			t.Logf("permission contrast confirmed: same HWND %d, same last input %d, denied without grant and accepted with grant", target.HWND(), granted.LastInput)
		}
		wait("native foreground", func() bool { return foregroundProbeSnapshot("", false, 0).Foreground == target.HWND() })
		lendBack()
		command("grant")
		before := foregroundProbeSnapshot("posted double", false, target.HWND())
		double()
		if target.Visible() {
			t.Fatal("double did not hide window")
		}
		double()
		if !target.Visible() {
			t.Fatal("double did not show window")
		}
		wait("posted double foreground", func() bool { return foregroundProbeSnapshot("", false, 0).Foreground == target.HWND() })
		after := foregroundProbeSnapshot("posted double", true, target.HWND())
		if before.LastInput != after.LastInput {
			t.Fatal("posted messages changed last input")
		}
		lendBack()
		command("grant")
		post.Call(tray.HWND(), 0x0312, 1, 0)
		flush()
		wait("open foreground", func() bool { return foregroundProbeSnapshot("", false, 0).Foreground == target.HWND() })
		t.Logf("round %d authorized native / posted double / open activated Edge HWND %d, last input %d", round, target.HWND(), after.LastInput)
		lendBack()
	}
}
