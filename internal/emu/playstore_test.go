//go:build windows

package emu

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/host"
)

// playStoreHooks replaces what the store's code does to a guest for one test: the component check, the guest script
// (scripted results per action, in order), and for PlayStore the qemu scan, starts and stops.
type playStoreHooks struct {
	ready   bool
	running bool
	results map[string][]string // action -> RESULT words, in order (the last one repeats)
	calls   []string            // "script <action>", "start", "stop", "ready", "put"
	starts  []StartOptions
}

func (h *playStoreHooks) install(t *testing.T) {
	t.Helper()
	ready, script, find, start, stop, put := playStoreFilesReady, playStoreScript, playStoreFindQemu, playStoreStart, playStoreStop, createPlayStore
	t.Cleanup(func() {
		playStoreFilesReady, playStoreScript, playStoreFindQemu, playStoreStart, playStoreStop, createPlayStore = ready, script, find, start, stop, put
	})
	playStoreFilesReady = func(string) bool { h.calls = append(h.calls, "ready"); return h.ready }
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
	createPlayStore = func(m *Manager, ctx context.Context, i int) error {
		h.calls = append(h.calls, "put")
		return m.putPlayStore(ctx, i)
	}
}

// TestCreateWithoutPlayStore: a create that does not ask for the store (the default) does nothing about it: no
// component check, no guest script, no step in the plan, no key in mas-avd.json.
func TestCreateWithoutPlayStore(t *testing.T) {
	m, _ := opsTestManager(t)
	var h createHooks
	h.install(t)
	var ps playStoreHooks
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
	if slices.Contains(steps, "play_store") {
		t.Errorf("progress steps %v", steps)
	}
	if meta := m.Root.InstanceMeta(6); meta == nil || meta.Get(playStoreMetaKey) != nil {
		t.Errorf("mas-avd.json has %s", playStoreMetaKey)
	}
	if m.playStoreWanted(6) {
		t.Error("wanted without asking")
	}
}

// TestCreatePlayStoreRefused: --play-store with --no-bridge, or without the component, is refused before any file is
// written (no instance, no store calls beyond the check).
func TestCreatePlayStoreRefused(t *testing.T) {
	m, _ := opsTestManager(t)
	var h createHooks
	h.install(t)
	var ps playStoreHooks
	ps.install(t)
	i := 6
	_, err := m.Create(context.Background(), CreateRequest{Index: &i, PlayStore: true, NoBridge: true})
	opsSkipIfNotImplemented(t, err)
	if errs.CodeOf(err) != errs.InvalidArgument || !strings.Contains(err.Error(), "--no-bridge") {
		t.Errorf("with --no-bridge: %v", err)
	}
	_, err = m.Create(context.Background(), CreateRequest{Index: &i, PlayStore: true})
	if errs.CodeOf(err) != errs.NotReady || !strings.Contains(errs.As(err).Advice, "components fetch --play-store") {
		t.Errorf("without the component: %v", err)
	}
	if left := instanceFilesOnDisk(m.Root, 6); len(left) != 0 || len(h.opts) != 0 {
		t.Errorf("files %v, boots %d", left, len(h.opts))
	}
	if !slices.Equal(ps.calls, []string{"ready"}) {
		t.Errorf("store calls %v", ps.calls)
	}
}

// TestCreatePlayStore: --play-store puts the store in after the setup's boot (the bridge's rounds), before the check
// and the shutdown, with its own progress step; a failed install fails the create, and --resume puts it in again.
func TestCreatePlayStore(t *testing.T) {
	m, _ := opsTestManager(t)
	var h createHooks
	var ps playStoreHooks
	h.start = func(m *Manager, ctx context.Context, i int, opt StartOptions) (*StartResult, error) {
		ps.calls = append(ps.calls, "boot")
		return &StartResult{HostIME: true}, nil
	}
	h.check = nil
	h.install(t)
	ps.ready = true
	ps.results = map[string][]string{"install": {playStoreNotWritable, playStoreInstalled}}
	ps.install(t)
	var steps []string
	i := 6
	_, err := m.Create(context.Background(), CreateRequest{Index: &i, PlayStore: true,
		Progress: func(p Progress) { steps = append(steps, p.Step) }})
	opsSkipIfNotImplemented(t, err)
	if errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "新建失败") {
		t.Fatalf("an install that cannot write: %v", err)
	}
	if !m.playStoreWanted(6) || h.stopped != 1 {
		t.Fatalf("wanted %v, stopped %d", m.playStoreWanted(6), h.stopped)
	}
	ps.calls, steps = nil, nil
	info, err := m.Create(context.Background(), CreateRequest{Index: &i, Resume: true,
		Progress: func(p Progress) { steps = append(steps, p.Step) }})
	if err != nil || info.Status != StatusOffline {
		t.Fatalf("resume: %+v %v", info, err)
	}
	if want := []string{"boot", "put", "script install"}; !slices.Equal(ps.calls, want) {
		t.Errorf("resume calls %v, want %v", ps.calls, want)
	}
	if k := slices.Index(steps, "play_store"); k < 0 || slices.Index(steps, "create_stop") < k {
		t.Errorf("progress steps %v", steps)
	}
	if len(h.opts) == 0 || h.opts[len(h.opts)-1].NoBridge {
		t.Errorf("setup boots %+v, want the bridge", h.opts)
	}
}

// TestPlayStoreInstallUninstall: install on an instance that is off boots it, runs the script, boots cold and checks,
// then stops it; uninstall runs the script, boots cold when it asks for a reboot, runs it again, and records that
// the store is gone. A running instance comes back as it ran and is left running.
func TestPlayStoreInstallUninstall(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	ctx := context.Background()
	var ps playStoreHooks
	ps.ready = true
	ps.results = map[string][]string{"install": {playStoreInstalled}, "status": {playStoreInstalled},
		"uninstall": {playStoreReboot, playStoreGone}}
	ps.install(t)

	res, err := m.PlayStore(ctx, i, "install")
	if err != nil || !res.Installed || res.State != playStoreInstalled || !res.Restarted || !res.Stopped {
		t.Fatalf("install: %+v %v", res, err)
	}
	if want := []string{"ready", "start", "script install", "stop", "start", "script status", "stop"}; !slices.Equal(ps.calls, want) {
		t.Errorf("install calls %v, want %v", ps.calls, want)
	}
	if !m.playStoreWanted(i) || len(res.Files) != 2 {
		t.Errorf("wanted %v, files %q", m.playStoreWanted(i), res.Files)
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
	if m.playStoreWanted(i) {
		t.Error("still wanted after uninstall")
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
	var ps playStoreHooks
	ps.ready = true
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
	if slices.Contains(ps.calls, "start") || slices.ContainsFunc(ps.calls, func(c string) bool { return strings.HasPrefix(c, "script") }) {
		t.Errorf("calls %v", ps.calls)
	}
}

// TestPlayStoreRefused: install needs the bridge and the component, status a running instance, the action a known
// word; a script that cannot write is not ready, one that stops early a failure; nothing is recorded then.
func TestPlayStoreRefused(t *testing.T) {
	m, i := opsTestInstance(t)
	ctx := context.Background()
	var ps playStoreHooks
	ps.install(t)
	os.Remove(bridgeMarker(m.Root, i)) // a files-only create placed it
	if _, err := m.PlayStore(ctx, i, "install"); errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "执行桥") {
		t.Errorf("without the bridge marker: %v", err)
	}
	writeBridgeMarker(m.Root, i)
	if _, err := m.PlayStore(ctx, i, "install"); errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "组件") {
		t.Errorf("without the component: %v", err)
	}
	if _, err := m.PlayStore(ctx, i, "status"); errs.CodeOf(err) != errs.Unsupported {
		t.Errorf("status of an instance that is off: %v", err)
	}
	if _, err := m.PlayStore(ctx, i, "remove"); errs.CodeOf(err) != errs.Usage {
		t.Errorf("unknown action: %v", err)
	}
	ps.ready = true
	ps.results = map[string][]string{"install": {playStoreNotWritable}}
	if _, err := m.PlayStore(ctx, i, "install"); errs.CodeOf(err) != errs.NotReady || !strings.Contains(errs.As(err).Advice, "bridge") {
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
	if _, err := m.PlayStore(ctx, i, "uninstall"); err == nil || m.playStoreWanted(i) {
		t.Errorf("after the failures: wanted %v, %v", m.playStoreWanted(i), err)
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
