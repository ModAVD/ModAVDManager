//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package win

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEdgeProfileMatches(t *testing.T) {
	profile := `C:\profiles\manager ui-edge2`
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"equals", []string{"msedge.exe", "--user-data-dir=" + profile}, true},
		{"separate", []string{"msedge.exe", "--user-data-dir", profile}, true},
		{"case and clean", []string{"msedge.exe", `--user-data-dir=c:\PROFILES\other\..\manager ui-edge2\`}, true},
		{"sibling", []string{"msedge.exe", "--user-data-dir=" + profile + "-other"}, false},
		{"URL substring", []string{"msedge.exe", "--app=http://localhost/?--user-data-dir=" + profile}, false},
		{"renderer", []string{"msedge.exe", "--user-data-dir=" + profile, "--type=renderer"}, false},
		{"last switch", []string{"msedge.exe", "--user-data-dir=" + profile, `--user-data-dir=C:\other`}, false},
		{"relative", []string{"msedge.exe", "--user-data-dir=manager ui-edge2"}, false},
		{"missing", []string{"msedge.exe", "--user-data-dir"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := edgeProfileMatches(tc.args, profile); got != tc.want {
				t.Fatalf("match = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEdgeProcessWindows(t *testing.T) {
	windows := testWindows(t, []testWindow{
		{class: "Chrome_WidgetWin_1", title: "manager one"},
		{class: "Chrome_WidgetWin_1", title: "manager two"},
		{class: "Chrome_WidgetWin_0", title: "helper"},
		{class: "Chrome_WidgetWin_1", title: ""},
		{class: "Chrome_WidgetWin_1", title: "dialog", owned: true},
		{class: fmt.Sprint("OtherWindow", os.Getpid()), title: "other app"},
	})
	p, ok := FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("self not found")
	}
	found, err := edgeProcessWindows(map[uint32]Process{p.PID: p})
	if err != nil {
		t.Fatal(err)
	}
	var got []uintptr
	for _, w := range found {
		got = append(got, w.HWND())
	}
	slices.Sort(got)
	want := []uintptr{windows[0], windows[1], windows[4]}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("windows %v, want %v", got, want)
	}
	for _, visible := range []bool{false, true} {
		for _, w := range found {
			if err := w.SetVisible(visible); err != nil {
				t.Fatal(err)
			}
		}
		if !waitFor(3*time.Second, func() bool {
			for _, w := range found {
				if w.Visible() != visible {
					return false
				}
			}
			return true
		}) {
			t.Fatalf("visibility did not become %v", visible)
		}
	}
	// An HWND is not enough: a mismatched process creation time must prevent a recycled PID from being touched.
	stale := found[0]
	stale.process.Created++
	if err := stale.SetVisible(false); err != nil {
		t.Fatal(err)
	}
	// SetVisible is asynchronous: keep observing long enough to catch an incorrectly posted hide.
	until := time.Now().Add(150 * time.Millisecond)
	for time.Now().Before(until) {
		if !found[0].Visible() {
			t.Fatal("stale identity hid window")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, h := range []uintptr{windows[2], windows[3], windows[5]} {
		r, _, _ := procIsWindowVisible.Call(h)
		if r == 0 {
			t.Fatalf("unrelated window %d hidden", h)
		}
	}
	found, err = edgeProcessWindows(map[uint32]Process{})
	if err != nil || len(found) != 0 {
		t.Fatalf("empty targets: %v %v", found, err)
	}
	// A nonexistent profile must not claim the user's ordinary Edge windows.
	found, err = EdgeProfileWindows(`C:\modavd-test-nonexistent-profile-` + strings.Repeat("x", 40))
	if err != nil || len(found) != 0 {
		t.Fatalf("unrelated Edge matched: %v %v", found, err)
	}
}
