//go:build windows

package emu

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

// playStoreHooks replaces what the store's code does to a guest for one test: the guest script (scripted results per
// action, in order), the qemu scan, starts and stops, and with ready set the component check (without it the real
// check reads the test root's sdk\playstore).
type playStoreHooks struct {
	ready   *bool
	running bool
	results map[string][]string // action -> RESULT words, in order (the last one repeats)
	calls   []string            // "script <action>", "start", "stop", "ready"
	starts  []StartOptions
}

func (h *playStoreHooks) install(t *testing.T) {
	t.Helper()
	ready, script, find, start, stop := playStoreFilesReady, playStoreScript, playStoreFindQemu, playStoreStart, playStoreStop
	t.Cleanup(func() {
		playStoreFilesReady, playStoreScript, playStoreFindQemu, playStoreStart, playStoreStop = ready, script, find, start, stop
	})
	real := playStoreFilesReady
	playStoreFilesReady = func(dir string) bool {
		h.calls = append(h.calls, "ready")
		if h.ready == nil {
			return real(dir)
		}
		return *h.ready
	}
	used := map[string]int{}
	playStoreScript = func(_ *Manager, _ context.Context, _ int, action string) (playStoreRun, error) {
		h.calls = append(h.calls, "script "+action)
		rs := h.results[action]
		if len(rs) == 0 {
			return playStoreRun{}, errs.New(errs.LaunchFailed, "no result scripted for "+action)
		}
		k := min(used[action], len(rs)-1)
		used[action]++
		return playStoreRun{Result: rs[k], Lines: []string{"play /product/priv-app/Phonesky/Phonesky.apk", "stub hidden", "other"}}, nil
	}
	playStoreFindQemu = func(*Manager, int) (host.Process, bool, error) {
		if h.running {
			return host.Process{PID: 4242, Name: "qemu-system-x86_64-headless.exe"}, true, nil
		}
		return host.Process{}, false, nil
	}
	playStoreStart = func(_ *Manager, _ context.Context, _ int, opt StartOptions) (*StartResult, error) {
		h.calls = append(h.calls, "start")
		h.starts = append(h.starts, opt)
		return &StartResult{}, nil
	}
	playStoreStop = func(*Manager, context.Context, int) ([]string, error) {
		h.calls = append(h.calls, "stop")
		return nil, nil
	}
}

func yes() *bool { b := true; return &b }

// guestCalls are the calls that touch the instance (boots, stops, guest script runs).
func (h *playStoreHooks) guestCalls() []string {
	var out []string
	for _, c := range h.calls {
		if c != "ready" {
			out = append(out, c)
		}
	}
	return out
}

// playStoreInfo is instance i's InstanceInfo.PlayStore, read the way list reads it.
func playStoreInfo(t *testing.T, m *Manager, i int) bool {
	t.Helper()
	in, err := m.instance(i)
	if err != nil {
		t.Fatal(err)
	}
	return m.instanceInfo(context.Background(), in, nil, nil).PlayStore
}

// TestCreateLeavesPlayStoreAlone: create never puts the store in: no component check, no guest script, no step in
// the plan, no key in mas-avd.json.
func TestCreateLeavesPlayStoreAlone(t *testing.T) {
	m, _ := opsTestManager(t)
	var h createHooks
	h.install(t)
	var ps playStoreHooks
	ps.ready = yes()
	ps.install(t)
	var steps []string
	i := 6
	_, err := m.Create(context.Background(), CreateRequest{Index: &i, Progress: func(p Progress) { steps = append(steps, p.Step) }})
	opsSkipIfNotImplemented(t, err)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps.calls) != 0 {
		t.Errorf("store calls %v, want none", ps.calls)
	}
	if slices.ContainsFunc(steps, func(s string) bool { return strings.Contains(s, "play") }) {
		t.Errorf("progress steps %v", steps)
	}
	if meta := m.Root.InstanceMeta(6); meta == nil || meta.Get(playStoreMetaKey) != nil {
		t.Errorf("mas-avd.json has %s", playStoreMetaKey)
	}
	if m.playStoreWanted(6) || playStoreInfo(t, m, 6) {
		t.Error("the store is recorded without asking")
	}
}

// TestPlayStoreInstallUninstall: install on an instance that is off boots it, runs the script, boots cold and checks,
// then stops it; uninstall runs the script, boots cold when it asks for a reboot, runs it again, and records that
// the store is gone. A running instance comes back as it ran and is left running. InstanceInfo.PlayStore follows.
func TestPlayStoreInstallUninstall(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	ctx := context.Background()
	var ps playStoreHooks
	ps.ready = yes()
	ps.results = map[string][]string{"install": {playStoreInstalled}, "status": {playStoreInstalled},
		"uninstall": {playStoreReboot, playStoreGone}}
	ps.install(t)

	if err := m.PlayStoreCheck(i, "install"); err != nil {
		t.Fatalf("check before install: %v", err)
	}
	ps.calls = nil
	res, err := m.PlayStore(ctx, i, "install")
	if err != nil || !res.Installed || res.State != playStoreInstalled || !res.Restarted || !res.Stopped {
		t.Fatalf("install: %+v %v", res, err)
	}
	if want := []string{"ready", "start", "script install", "stop", "start", "script status", "stop"}; !slices.Equal(ps.calls, want) {
		t.Errorf("install calls %v, want %v", ps.calls, want)
	}
	if !m.playStoreWanted(i) || !playStoreInfo(t, m, i) || len(res.Files) != 2 {
		t.Errorf("wanted %v, info %v, files %q", m.playStoreWanted(i), playStoreInfo(t, m, i), res.Files)
	}
	if _, err := m.Bridge(ctx, i, "uninstall"); errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "Google Play") {
		t.Errorf("bridge uninstall with the store: %v", err)
	}

	ps.calls, ps.running = nil, true
	ps.results["status"] = []string{playStoreAbsent}
	res, err = m.PlayStore(ctx, i, "uninstall")
	if err != nil || res.Installed || res.State != playStoreAbsent || !res.Restarted || res.Stopped {
		t.Fatalf("uninstall: %+v %v", res, err)
	}
	if want := []string{"script uninstall", "stop", "start", "script uninstall", "script status"}; !slices.Equal(ps.calls, want) {
		t.Errorf("uninstall calls %v, want %v", ps.calls, want)
	}
	if m.playStoreWanted(i) || playStoreInfo(t, m, i) {
		t.Error("still recorded after uninstall")
	}
	for k, opt := range ps.starts {
		if opt.NoBridge || opt.Headless || !opt.bridgeLocked {
			t.Errorf("start %d: %+v", k, opt)
		}
	}
}

// TestPlayStoreTakesBridgeLock: the store's actions hold the bridge lock (status says bridging meanwhile, delete and
// bridge wait), so one held elsewhere makes them busy.
func TestPlayStoreTakesBridgeLock(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	var ps playStoreHooks
	ps.ready = yes()
	ps.install(t)
	l, ok, err := host.TryLock(m.lockPath(i, "bridge"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	defer l.Unlock()
	for _, action := range []string{"install", "uninstall", "status"} {
		if _, err := m.PlayStore(context.Background(), i, action); errs.CodeOf(err) != errs.Busy {
			t.Errorf("%s while the bridge lock is held: %v", action, err)
		}
	}
	if calls := ps.guestCalls(); len(calls) != 0 {
		t.Errorf("calls %v", calls)
	}
}

// TestPlayStoreRefused: install needs the bridge and the root's five files (the real check: their pinned SHA-256),
// status a running instance, the action a known word. Those refusals come before the instance is touched (no boot,
// no script, nothing recorded), carry the reason the manager offers a way out by, and PlayStoreCheck gives the same.
// A script that cannot write is not ready, one that stops early a failure; nothing is recorded then either.
func TestPlayStoreRefused(t *testing.T) {
	m, i := opsTestInstance(t)
	ctx := context.Background()
	var ps playStoreHooks
	ps.install(t)
	os.Remove(bridgeMarker(m.Root, i)) // a files-only create placed it
	untouched := func(what string) {
		t.Helper()
		if calls := ps.guestCalls(); len(calls) != 0 {
			t.Errorf("%s: calls %v", what, calls)
		}
		if meta := m.Root.InstanceMeta(i); meta == nil || meta.Get(playStoreMetaKey) != nil {
			t.Errorf("%s: mas-avd.json has %s", what, playStoreMetaKey)
		}
	}
	reason := func(err error) string { return errs.DetailsOf(err)[errs.Reason] }
	both := []func() error{
		func() error { _, err := m.PlayStore(ctx, i, "install"); return err },
		func() error { return m.PlayStoreCheck(i, "install") },
	}

	for _, call := range both {
		if err := call(); errs.CodeOf(err) != errs.NotReady || reason(err) != PlayStoreReasonNoBridge || !strings.Contains(err.Error(), "执行桥") {
			t.Errorf("without the bridge marker: %v %v", err, errs.DetailsOf(err))
		}
	}
	untouched("without the bridge")

	writeBridgeMarker(m.Root, i)
	for _, call := range both {
		err := call()
		if errs.CodeOf(err) != errs.NotReady || reason(err) != PlayStoreReasonFiles ||
			!strings.Contains(errs.As(err).Advice, "components import-playstore") {
			t.Errorf("without the files: %v %v", err, errs.DetailsOf(err))
		}
	}
	untouched("without the files")
	// files of the right names that are not the store's: still refused
	dir := filepath.Join(m.Root.RuntimeSDK(), root.PlayStoreDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range root.PlayStoreComponent().KeyFiles {
		os.WriteFile(filepath.Join(dir, name), []byte("not the store"), 0o644)
	}
	os.WriteFile(filepath.Join(dir, "source.properties"), []byte("Pkg.Revision=14\n"), 0o644)
	if _, err := m.PlayStore(ctx, i, "install"); reason(err) != PlayStoreReasonFiles {
		t.Errorf("with damaged files: %v", err)
	}
	untouched("with damaged files")

	if _, err := m.PlayStore(ctx, i, "status"); errs.CodeOf(err) != errs.Unsupported {
		t.Errorf("status of an instance that is off: %v", err)
	}
	if _, err := m.PlayStore(ctx, i, "remove"); errs.CodeOf(err) != errs.Usage {
		t.Errorf("unknown action: %v", err)
	}
	if err := m.PlayStoreCheck(i, "uninstall"); err != nil {
		t.Errorf("uninstall needs neither the files nor the bridge check: %v", err)
	}
	if err := m.PlayStoreCheck(7, "install"); errs.CodeOf(err) != errs.NotFound {
		t.Errorf("an instance that is not there: %v", err)
	}
	untouched("status, unknown action and checks")

	ps.ready = yes()
	ps.results = map[string][]string{"install": {playStoreNotWritable}}
	if _, err := m.PlayStore(ctx, i, "install"); errs.CodeOf(err) != errs.NotReady || reason(err) != PlayStoreReasonNoBridge ||
		!strings.Contains(errs.As(err).Advice, "bridge") {
		t.Errorf("not writable: %v", err)
	}
	ps.results = map[string][]string{"install": {"bad-file"}}
	if _, err := m.PlayStore(ctx, i, "install"); errs.CodeOf(err) != errs.LaunchFailed {
		t.Errorf("bad file: %v", err)
	}
	ps.results = map[string][]string{"uninstall": {playStoreReboot}}
	if _, err := m.PlayStore(ctx, i, "uninstall"); err == nil {
		t.Error("an uninstall that never finishes passed")
	}
	if m.playStoreWanted(i) {
		t.Error("recorded after the failures")
	}
}

// TestPlayStoreArm64Guest: an arm64 guest has no store, refused as unsupported.
func TestPlayStoreArm64Guest(t *testing.T) {
	m, i := opsTestInstance(t)
	var ps playStoreHooks
	ps.ready = yes()
	ps.install(t)
	cfg := avd.ConfigPath(m.Root, i)
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, append(b, "\r\nabi.type=arm64-v8a\r\nhw.cpu.arch=arm64\r\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	err = m.PlayStoreCheck(i, "install")
	if errs.CodeOf(err) != errs.Unsupported || errs.DetailsOf(err)[errs.Reason] != PlayStoreReasonUnsupported {
		t.Errorf("arm64: %v %v", err, errs.DetailsOf(err))
	}
}

// TestParsePlayStoreRun: the RESULT line is the result (the last wins), the rest are lines; fileLines keeps the file
// and placeholder lines.
func TestParsePlayStoreRun(t *testing.T) {
	run := parsePlayStoreRun("copied /product/a\r\nRESULT failed\n\nplay /product/a\nstub hidden\npackage /x\nRESULT installed\n")
	if run.Result != playStoreInstalled || len(run.Lines) != 4 {
		t.Fatalf("%+v", run)
	}
	if got := fileLines(run.Lines); !slices.Equal(got, []string{"play /product/a", "stub hidden", "package /x"}) {
		t.Errorf("fileLines %q", got)
	}
	if parsePlayStoreRun("error: closed").Result != "" {
		t.Error("a result without RESULT")
	}
}
