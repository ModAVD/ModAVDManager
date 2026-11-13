//go:build windows

package emu

import (
	"context"
	"testing"

	"modavd/internal/win"
)

// TestBridgePrepLaunch: a start asked for a window launches headless only when the bridge is surely installed after
// the launch (its marker not there yet, or the caller says so); with the marker there it launches as asked.
func TestBridgePrepLaunch(t *testing.T) {
	for _, c := range []struct {
		name                         string
		window, prep, bridge, marker bool
		want                         bool
	}{
		{"window, bridge wanted, no marker yet", true, false, true, false, true},
		{"window, marker there", true, false, true, true, false},
		{"window, marker there, caller knows", true, true, true, true, true},
		{"headless start", false, false, true, false, false},
		{"headless start, caller knows", false, true, true, true, false},
		{"no bridge", true, false, false, false, false},
		{"no bridge, caller knows", true, true, false, true, false},
	} {
		opt := StartOptions{Window: c.window, bridgePrep: c.prep}
		if got := bridgePrepLaunch(opt, c.bridge, c.marker); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

// TestStartBridgePrepHeadless: the end of a start that launched an instance whose bridge is still to be installed.
// With a window asked for, every boot that only prepares (the cold boot after a script run that found the system not
// writable yet) is headless, and the last boot, a cold boot after a normal stop, has the window: either the one after
// the install (which loads the bridge), or one more when the instance is left headless. A headless start stays
// headless throughout. The starts here are fakes (bridgeStart): no emulator, a temporary root.
func TestStartBridgePrepHeadless(t *testing.T) {
	m, i := opsTestInstance(t)
	if err := writeBridgeMarker(m.Root, i); err != nil {
		t.Fatal(err)
	}
	start, guest, script := bridgeStart, bridgeGuest, bridgeScript
	t.Cleanup(func() { bridgeStart, bridgeGuest, bridgeScript = start, guest, script })
	ctx := context.Background()
	const stock = "libndk_translation.so"
	for _, c := range []struct {
		name         string
		window       bool  // the start asked for the window
		launchWindow bool  // how the start launched the first boot
		locked       bool  // bridgeLocked (no update step: the guest is never asked for hashes)
		codes        []int // the script's exit codes, in order (0 after them)
		loadedAfter  int   // the guest runs the bridge after this many cold boots (-1: never)
		want         []bool
	}{
		// the production case: marker placed by this start, so the first boot is headless; verity off, install, load
		{"window, prep launch, three rounds", true, false, false, []int{3, 0, 0}, 2, []bool{false, true}},
		// the marker was there: the first boot had the window (not known before), the prep cold boot does not
		{"window, windowed launch, three rounds", true, true, false, []int{3, 0, 0}, 2, []bool{false, true}},
		{"window, prep launch, installed in one round", true, false, false, []int{0, 0}, 1, []bool{true}},
		// never writable: the prep boot is headless, then one more cold boot with the window
		{"window, prep launch, not writable twice", true, false, false, []int{3, 3}, -1, []bool{false, true}},
		// the install's boot had the window, the bridge was still missing, the next round's boot only prepared
		{"window, loaded late", true, false, false, []int{0, 3, 0}, 3, []bool{true, false, true}},
		// prep launch, and the guest has the bridge after all: one cold boot with the window
		{"window, prep launch, nothing to install", true, false, true, nil, 0, []bool{true}},
		{"headless, three rounds", false, false, false, []int{3, 0, 0}, 2, []bool{false, false}},
		{"headless, not writable twice", false, false, false, []int{3, 3}, -1, []bool{false}},
	} {
		var starts []StartOptions
		bridgeStart = func(_ *Manager, _ context.Context, _ int, opt StartOptions) (*StartResult, error) {
			starts = append(starts, opt)
			// what Start reports for a launch (the instance was stopped before each of these)
			return &StartResult{Launched: true, launchWindow: opt.Window && !opt.launchHeadless}, nil
		}
		bridgeGuest = func(*Manager, context.Context, int) string {
			if c.loadedAfter >= 0 && len(starts) >= c.loadedAfter {
				return bridgeProp
			}
			return stock
		}
		n := 0
		bridgeScript = func(*Manager, context.Context, int, string, bool) (bridgeRun, error) {
			code := 0
			if n < len(c.codes) {
				code = c.codes[n]
			}
			n++
			return bridgeRun{Code: code, Last: "done"}, nil
		}
		opt := StartOptions{Window: c.window, bridgeLocked: c.locked}
		res, err := m.afterStartBridge(ctx, i, opt, true, &StartResult{Launched: true, launchWindow: c.launchWindow})
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := make([]bool, len(starts))
		for k, o := range starts {
			got[k] = o.Window
			if !o.NoBridge || o.Headless || o.bridgePrep || o.launchHeadless {
				t.Errorf("%s: cold boot %d: NoBridge %v Headless %v bridgePrep %v launchHeadless %v", c.name, k+1,
					o.NoBridge, o.Headless, o.bridgePrep, o.launchHeadless)
			}
		}
		if !equalBools(got, c.want) {
			t.Errorf("%s: cold boots with Window %v, want %v", c.name, got, c.want)
		}
		if res.launchWindow != c.window {
			t.Errorf("%s: the instance ends with window %v, want %v", c.name, res.launchWindow, c.window)
		}
	}
}

// TestBridgeUninstallRestoresWindow: the uninstall's first cold boot on a windowed instance with the bridge is
// headless because a second one should follow; when the script then does not take the bridge off (no second cold
// boot), the instance still gets its window back with one more.
func TestBridgeUninstallRestoresWindow(t *testing.T) {
	m, i := opsTestInstance(t)
	windowed, _ := startSleeper(t) // a readable command line without -no-window
	findQemu, start, guest, script := bridgeFindQemu, bridgeStart, bridgeGuest, bridgeScript
	t.Cleanup(func() { bridgeFindQemu, bridgeStart, bridgeGuest, bridgeScript = findQemu, start, guest, script })
	var starts []StartOptions
	bridgeFindQemu = func(*Manager, int) (win.Process, bool, error) { return windowed, true, nil }
	bridgeStart = func(_ *Manager, _ context.Context, _ int, opt StartOptions) (*StartResult, error) {
		starts = append(starts, opt)
		return &StartResult{Launched: true, launchWindow: opt.Window}, nil
	}
	bridgeGuest = func(*Manager, context.Context, int) string { return bridgeProp }
	codes := []int{3, 1}
	n := 0
	bridgeScript = func(*Manager, context.Context, int, string, bool) (bridgeRun, error) {
		code := 0
		if n < len(codes) {
			code = codes[n]
		}
		n++
		return bridgeRun{Code: code, Last: "done"}, nil
	}
	if _, err := m.Bridge(context.Background(), i, "uninstall"); err != nil {
		t.Fatal(err)
	}
	got := make([]bool, len(starts))
	for k, o := range starts {
		got[k] = o.Window
	}
	if want := []bool{false, true}; !equalBools(got, want) {
		t.Fatalf("cold boots with Window %v, want %v", got, want)
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if a[k] != b[k] {
			return false
		}
	}
	return true
}
