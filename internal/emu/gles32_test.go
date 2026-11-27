//go:build windows

package emu

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"modavd/internal/errs"
)

// glHooks is playStoreHooks (the qemu scan, starts and stops) plus the GLES 3.2 script's scripted runs: results per
// action, in order, the last repeating; a status entry is "<word>/<prop>"; "?" ran with no RESULT.
type glHooks struct {
	playStoreHooks
	results map[string][]string
}

func (h *glHooks) install(t *testing.T) {
	t.Helper()
	h.playStoreHooks.install(t)
	old := gles32Script
	t.Cleanup(func() { gles32Script = old })
	used := map[string]int{}
	gles32Script = func(_ *Manager, _ context.Context, _ int, action string) (playStoreRun, error) {
		h.calls = append(h.calls, "gles "+action)
		rs := h.results[action]
		if len(rs) == 0 {
			return playStoreRun{}, errs.New(errs.LaunchFailed, "no result scripted for "+action)
		}
		k := min(used[action], len(rs)-1)
		used[action]++
		word, prop, _ := strings.Cut(rs[k], "/")
		if word == "?" {
			return playStoreRun{Ran: true}, errs.New(errs.LaunchFailed, "the script stopped without RESULT")
		}
		run := playStoreRun{Result: word, Ran: true, Lines: []string{}}
		if action == "status" {
			run.Lines = []string{"prop " + prop, "line yes"}
		}
		return run, nil
	}
}

func glInfo(t *testing.T, m *Manager, i int) bool {
	t.Helper()
	in, err := m.instance(i)
	if err != nil {
		t.Fatal(err)
	}
	return m.instanceInfo(context.Background(), in, nil, nil).GLES32
}

// TestGLES32OnOff: on an instance that is off, on boots it, runs the script, boots cold, checks and stops it; a
// running one comes back as it ran (window) and stays running; off does the same the other way. The record and
// InstanceInfo.GLES32 follow, and bridge uninstall refuses while it is on.
func TestGLES32OnOff(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	ctx := context.Background()
	var h glHooks
	h.results = map[string][]string{"on": {gles32ResultReboot}, "off": {gles32ResultReboot},
		"status": {"on/196610"}}
	h.install(t)

	if err := m.GLES32Check(i, "on"); err != nil {
		t.Fatalf("check: %v", err)
	}
	res, err := m.GLES32(ctx, i, "on")
	if err != nil || !res.Enabled || res.State != "on" || res.Prop != "196610" || !res.Restarted || !res.Stopped {
		t.Fatalf("on: %+v %v", res, err)
	}
	if want := []string{"start", "gles on", "stop", "start", "gles status", "stop"}; !slices.Equal(h.calls, want) {
		t.Errorf("on calls %v, want %v", h.calls, want)
	}
	if !m.gles32Wanted(i) || !glInfo(t, m, i) || len(res.Notes) != 0 {
		t.Errorf("wanted %v, info %v, notes %q", m.gles32Wanted(i), glInfo(t, m, i), res.Notes)
	}
	if _, err := m.Bridge(ctx, i, "uninstall"); errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "GLES") {
		t.Errorf("bridge uninstall with the switch on: %v", err)
	}

	// running, with a window: restarted the same way, left running
	h.calls, h.running, h.starts = nil, true, nil
	h.results["status"] = []string{"off/196609"}
	res, err = m.GLES32(ctx, i, "off")
	if err != nil || res.Enabled || res.State != "off" || !res.Restarted || res.Stopped {
		t.Fatalf("off: %+v %v", res, err)
	}
	if want := []string{"gles off", "stop", "start", "gles status"}; !slices.Equal(h.calls, want) {
		t.Errorf("off calls %v, want %v", h.calls, want)
	}
	if m.gles32Wanted(i) || glInfo(t, m, i) {
		t.Error("still recorded after off")
	}
	for k, opt := range h.starts {
		if opt.NoBridge || opt.Headless || !opt.bridgeLocked {
			t.Errorf("start %d: %+v", k, opt)
		}
	}

	// status of a running instance only asks
	h.calls = nil
	h.results["status"] = []string{"pending-on/196609"}
	res, err = m.GLES32(ctx, i, "status")
	if err != nil || res.State != "pending-on" || res.Enabled || res.Prop != "196609" {
		t.Fatalf("status: %+v %v", res, err)
	}
	if want := []string{"gles status"}; !slices.Equal(h.calls, want) {
		t.Errorf("status calls %v", h.calls)
	}
}

// TestGLES32AlreadyAndPending: a file that already is as asked needs no boot when the guest agrees, one when it does
// not (an earlier run stopped before its cold boot).
func TestGLES32AlreadyAndPending(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	var h glHooks
	h.running = true
	h.results = map[string][]string{"on": {gles32ResultAlready}, "status": {"on/196610"}}
	h.install(t)
	res, err := m.GLES32(context.Background(), i, "on")
	if err != nil || !res.Enabled || res.Restarted {
		t.Fatalf("already: %+v %v", res, err)
	}
	if want := []string{"gles on", "gles status", "gles status"}; !slices.Equal(h.calls, want) {
		t.Errorf("calls %v, want %v", h.calls, want)
	}

	var p glHooks
	p.running = true
	p.results = map[string][]string{"on": {gles32ResultAlready}, "status": {"pending-on/196609", "on/196610"}}
	p.install(t)
	res, err = m.GLES32(context.Background(), i, "on")
	if err != nil || !res.Enabled || !res.Restarted {
		t.Fatalf("pending: %+v %v", res, err)
	}
	if want := []string{"gles on", "gles status", "stop", "start", "gles status"}; !slices.Equal(p.calls, want) {
		t.Errorf("calls %v, want %v", p.calls, want)
	}
}

// TestGLES32PlayStoreNote: an instance with the Play Store gets the note about the store's old device info.
func TestGLES32PlayStoreNote(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	if err := m.setPlayStoreWanted(i, true); err != nil {
		t.Fatal(err)
	}
	var h glHooks
	h.running = true
	h.results = map[string][]string{"on": {gles32ResultReboot}, "status": {"on/196610"}}
	h.install(t)
	res, err := m.GLES32(context.Background(), i, "on")
	if err != nil || len(res.Notes) != 1 || res.Notes[0] != GLES32PlayStoreNote || !strings.Contains(res.Notes[0], "清除数据") {
		t.Fatalf("notes: %+v %v", res, err)
	}
}

// TestGLES32Refused: on and off need the bridge (refused before anything is touched, with the reason the manager
// offers a way out by), status a running instance, the action a known word; a script that cannot write is not ready.
func TestGLES32Refused(t *testing.T) {
	m, i := opsTestInstance(t)
	ctx := context.Background()
	var h glHooks
	h.install(t)
	os.Remove(bridgeMarker(m.Root, i)) // a files-only create placed it
	reason := func(err error) string { return errs.DetailsOf(err)[errs.Reason] }
	for _, action := range []string{"on", "off"} {
		for _, call := range []func() error{
			func() error { _, err := m.GLES32(ctx, i, action); return err },
			func() error { return m.GLES32Check(i, action) },
		} {
			if err := call(); errs.CodeOf(err) != errs.NotReady || reason(err) != GLES32ReasonNoBridge || !strings.Contains(err.Error(), "执行桥") {
				t.Errorf("%s without the bridge: %v %v", action, err, errs.DetailsOf(err))
			}
		}
	}
	if len(h.guestCalls()) != 0 || m.gles32Wanted(i) {
		t.Errorf("touched: %v", h.calls)
	}
	if _, err := m.GLES32(ctx, i, "status"); errs.CodeOf(err) != errs.Unsupported {
		t.Errorf("status of an instance that is off: %v", err)
	}
	if _, err := m.GLES32(ctx, i, "toggle"); errs.CodeOf(err) != errs.Usage {
		t.Errorf("unknown action: %v", err)
	}
	if err := m.GLES32Check(7, "on"); errs.CodeOf(err) != errs.NotFound {
		t.Errorf("an instance that is not there: %v", err)
	}
	if len(h.guestCalls()) != 0 {
		t.Errorf("calls %v", h.calls)
	}

	writeBridgeMarker(m.Root, i)
	h.results = map[string][]string{"on": {gles32ResultNotWriting}}
	if _, err := m.GLES32(ctx, i, "on"); errs.CodeOf(err) != errs.NotReady || reason(err) != GLES32ReasonNoBridge {
		t.Errorf("not writable: %v", err)
	}
	if m.gles32Wanted(i) {
		t.Error("recorded after a script that wrote nothing")
	}
}

// TestGLES32FailureStopsAgain: an instance that was off is shut down again when the script fails, when it stops
// without RESULT, when the cold boot fails and when the check after it disagrees; the last is an error that names the
// version the guest reports. A running one is left running.
func TestGLES32FailureStopsAgain(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	ctx := context.Background()
	cases := []struct {
		name    string
		results map[string][]string
		start   int // the n-th start fails (1-based; 0: none)
		want    []string
	}{
		{"script fails", map[string][]string{"on": {"failed"}}, 0, []string{"start", "gles on", "stop"}},
		{"no RESULT", map[string][]string{"on": {"?"}}, 0, []string{"start", "gles on", "stop"}},
		{"check disagrees", map[string][]string{"on": {gles32ResultReboot}, "status": {"pending-on/196609"}}, 0,
			[]string{"start", "gles on", "stop", "start", "gles status", "stop"}},
		{"cold boot fails", map[string][]string{"on": {gles32ResultReboot}, "status": {"on/196610"}}, 2,
			[]string{"start", "gles on", "stop", "start", "stop"}},
	}
	for _, c := range cases {
		var h glHooks
		h.results = c.results
		h.install(t)
		if c.start > 0 {
			n := 0
			inner := playStoreStart
			playStoreStart = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
				n++
				if n == c.start {
					h.calls = append(h.calls, "start")
					return nil, errs.New(errs.LaunchFailed, "boom")
				}
				return inner(m, ctx, i, opt)
			}
		}
		res, err := m.GLES32(ctx, i, "on")
		if err == nil || res == nil || !res.Stopped || !slices.Equal(h.calls, c.want) {
			t.Errorf("%s: %+v %v, calls %v want %v", c.name, res, err, h.calls, c.want)
		}
		if c.name == "check disagrees" && (err == nil || !strings.Contains(err.Error(), "196609")) {
			t.Errorf("%s: the error does not name the version: %v", c.name, err)
		}
	}

	var h glHooks
	h.running = true
	h.results = map[string][]string{"on": {"failed"}}
	h.install(t)
	if res, err := m.GLES32(ctx, i, "on"); err == nil || res.Stopped || slices.Contains(h.calls, "stop") {
		t.Errorf("a running instance: %+v %v %v", res, err, h.calls)
	}
}

// TestGLES32TakesBridgeLock: one held elsewhere makes the switch busy.
func TestGLES32TakesBridgeLock(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	var h glHooks
	h.install(t)
	l, err := m.playStoreLock(i)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Unlock()
	for _, action := range []string{"on", "off", "status"} {
		if _, err := m.GLES32(context.Background(), i, action); errs.CodeOf(err) != errs.Busy {
			t.Errorf("%s while the bridge lock is held: %v", action, err)
		}
	}
	if len(h.guestCalls()) != 0 {
		t.Errorf("calls %v", h.calls)
	}
}
