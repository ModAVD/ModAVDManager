//go:build windows

package emu

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
	results map[string][]string // action -> RESULT words, in order (the last one repeats); "?": ran, no RESULT
	lines   map[string][]string // action -> the run's other lines (default: some of a status's)
	calls   []string            // "script <action>", "start", "stop", "ready"
	starts  []StartOptions
	// what the starts and stops return
	startErr, stopErr error
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
		lines := h.lines[action]
		if lines == nil {
			lines = []string{"play /product/priv-app/Phonesky/Phonesky.apk", "stub hidden", "other"}
		}
		if rs[k] == "?" {
			return playStoreRun{Lines: lines, Ran: true}, errs.New(errs.LaunchFailed, "the script stopped without RESULT")
		}
		return playStoreRun{Result: rs[k], Lines: lines, Ran: true}, nil
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
		if h.startErr != nil {
			return nil, h.startErr
		}
		return &StartResult{}, nil
	}
	playStoreStop = func(_ *Manager, ctx context.Context, _ int) ([]string, error) {
		h.calls = append(h.calls, "stop")
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, h.stopErr
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

// playStoreRecorded is instance i's InstanceInfo.PlayStore and PlayStorePartial, read the way list reads them.
func playStoreRecorded(t *testing.T, m *Manager, i int) (bool, bool) {
	t.Helper()
	in, err := m.instance(i)
	if err != nil {
		t.Fatal(err)
	}
	info := m.instanceInfo(context.Background(), in, nil, nil)
	return info.PlayStore, info.PlayStorePartial
}

// TestPlayStoreInstallFailed: an install the script took back leaves the record alone and shuts an instance that was
// off down again (also after a cancel); one it could not take back all of, or that stopped without saying, is
// recorded partial: InstanceInfo says so, bridge uninstall refuses, an uninstall clears it. A stop that fails after a
// failure is a warning, the error stays the install's; a boot that found another start under way stops nothing. A
// running instance is left running, with a warning when some of the undo waits for its next boot.
func TestPlayStoreInstallFailed(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	ctx := context.Background()
	var ps playStoreHooks
	ps.ready = yes()
	ps.install(t)

	ps.results = map[string][]string{"install": {playStoreFailure}}
	res, err := m.PlayStore(ctx, i, "install")
	if errs.CodeOf(err) != errs.LaunchFailed || !strings.Contains(errs.As(err).Advice, "已经撤回") || res == nil || !res.Stopped || res.State != "" {
		t.Fatalf("taken back: %+v %v", res, err)
	}
	if want := []string{"ready", "start", "script install", "stop"}; !slices.Equal(ps.calls, want) {
		t.Errorf("calls %v, want %v", ps.calls, want)
	}
	if meta := m.Root.InstanceMeta(i); meta == nil || meta.Get(playStoreMetaKey) != nil {
		t.Errorf("recorded after an install that was taken back: %v", meta)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if res, err := m.PlayStore(canceled, i, "install"); err == nil || !res.Stopped {
		t.Errorf("canceled: %+v %v", res, err)
	}

	for _, word := range []string{playStorePartial, "?"} {
		ps.results = map[string][]string{"install": {word}}
		res, err := m.PlayStore(ctx, i, "install")
		if err == nil || res.State != playStorePartial || !res.Stopped {
			t.Fatalf("%s: %+v %v", word, res, err)
		}
		if word == playStorePartial && !strings.Contains(errs.As(err).Advice, "playstore "+strconv.Itoa(i)+" uninstall") {
			t.Errorf("partial's advice: %v", err)
		}
		if on, partial := playStoreRecorded(t, m, i); on || !partial || !m.playStoreWanted(i) {
			t.Errorf("%s: recorded %v %v", word, on, partial)
		}
		if _, err := m.Bridge(ctx, i, "uninstall"); errs.CodeOf(err) != errs.NotReady || !strings.Contains(err.Error(), "Google Play") {
			t.Errorf("%s: bridge uninstall: %v", word, err)
		}
	}
	ps.results = map[string][]string{"uninstall": {playStoreGone}, "status": {playStoreAbsent}}
	if _, err := m.PlayStore(ctx, i, "uninstall"); err != nil {
		t.Fatalf("uninstall after a partial install: %v", err)
	}
	if on, partial := playStoreRecorded(t, m, i); on || partial {
		t.Errorf("after uninstall: %v %v", on, partial)
	}

	ps.results = map[string][]string{"install": {playStoreFailure}}
	ps.stopErr = errs.New(errs.Internal, "关不掉")
	res, err = m.PlayStore(ctx, i, "install")
	if errs.CodeOf(err) != errs.LaunchFailed || res.Stopped || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "关不掉") {
		t.Errorf("stop failed after the failure: %+v %v", res, err)
	}
	ps.stopErr = nil

	for _, c := range []struct {
		err  error
		want []string
	}{{errs.New(errs.Busy, "另一次开机"), []string{"start"}}, {errs.New(errs.Timeout, "开机超时"), []string{"start", "stop"}}} {
		ps.startErr, ps.calls = c.err, nil
		if _, err := m.PlayStore(ctx, i, "install"); errs.CodeOf(err) != errs.CodeOf(c.err) || !slices.Equal(ps.guestCalls(), c.want) {
			t.Errorf("start %v: %v, calls %v", c.err, err, ps.guestCalls())
		}
	}
	ps.startErr = nil

	ps.running, ps.calls = true, nil
	ps.lines = map[string][]string{"install": {"took back /product/a", playStoreUndoneAtBoot, "install failed: the store is partial after copying"}}
	res, err = m.PlayStore(ctx, i, "install")
	if err == nil || !strings.Contains(err.Error(), "install failed: the store is partial after copying") || res.Stopped ||
		len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "关机再开机") {
		t.Errorf("running: %+v %v", res, err)
	}
	if want := []string{"script install"}; !slices.Equal(ps.guestCalls(), want) {
		t.Errorf("running: calls %v", ps.guestCalls())
	}
}

// TestPlayStoreUninstallFailedStops: an uninstall that fails on an instance that was off shuts it down again and keeps
// the record (移除 is offered again).
func TestPlayStoreUninstallFailedStops(t *testing.T) {
	m, i := opsTestInstance(t)
	writeBridgeMarker(m.Root, i)
	if err := m.setPlayStoreWanted(i, true); err != nil {
		t.Fatal(err)
	}
	var ps playStoreHooks
	ps.install(t)
	ps.results = map[string][]string{"uninstall": {"failed"}}
	res, err := m.PlayStore(context.Background(), i, "uninstall")
	if errs.CodeOf(err) != errs.LaunchFailed || !res.Stopped {
		t.Fatalf("%+v %v", res, err)
	}
	if want := []string{"start", "script uninstall", "stop"}; !slices.Equal(ps.guestCalls(), want) {
		t.Errorf("calls %v", ps.guestCalls())
	}
	if on, _ := playStoreRecorded(t, m, i); !on {
		t.Error("the record went with a failed uninstall")
	}
}

// TestPlayStoreGSFID: gsfid reads the running instance's GSF ID (decimal as the guest says, hex worked out here, two's
// complement for a negative one) without a boot or a stop; no ID yet (the script's no-id, no decimal line, 0) is
// not_ready with its reason and advice; an instance that is off, a held bridge lock and a script that fails are
// refused. PlayStore itself does not take gsfid.
func TestPlayStoreGSFID(t *testing.T) {
	m, i := opsTestInstance(t)
	ctx := context.Background()
	var ps playStoreHooks
	ps.install(t)
	if _, err := m.PlayStoreGSFID(ctx, i); errs.CodeOf(err) != errs.Unsupported {
		t.Errorf("off: %v", err)
	}
	ps.running = true
	ps.results = map[string][]string{"gsfid": {playStoreOK}}
	for _, c := range [][2]string{{"3881793548573364239", "35dee6aa721e500f"}, {"-1", "ffffffffffffffff"}, {"0042", "2a"}} {
		ps.lines = map[string][]string{"gsfid": {"decimal " + c[0], "hex whatever the guest's printf said"}}
		res, err := m.PlayStoreGSFID(ctx, i)
		want, _ := strconv.ParseInt(c[0], 10, 64)
		if err != nil || res.Index != i || res.Decimal != strconv.FormatInt(want, 10) || res.Hex != c[1] {
			t.Errorf("%s: %+v %v", c[0], res, err)
		}
	}
	for _, c := range []struct {
		word  string
		lines []string
	}{{playStoreNoID, []string{"no GSF ID yet"}}, {playStoreNoID, []string{"decimal 5"}}, {playStoreOK, []string{"hex 1"}}, {playStoreOK, []string{"decimal 0"}}} {
		ps.results = map[string][]string{"gsfid": {c.word}}
		ps.lines = map[string][]string{"gsfid": c.lines}
		_, err := m.PlayStoreGSFID(ctx, i)
		if errs.CodeOf(err) != errs.NotReady || errs.DetailsOf(err)[errs.Reason] != PlayStoreReasonNoGSFID || !strings.Contains(errs.As(err).Advice, "等一两分钟") {
			t.Errorf("%s %q: %v", c.word, c.lines, err)
		}
	}
	for _, c := range []struct {
		word, line, says string
	}{{playStoreOK, "decimal 12a", "不是数字：12a"}, {"failed", "sqlite3: not found", "sqlite3: not found"}} {
		ps.results = map[string][]string{"gsfid": {c.word}}
		ps.lines = map[string][]string{"gsfid": {c.line}}
		if _, err := m.PlayStoreGSFID(ctx, i); errs.CodeOf(err) != errs.LaunchFailed || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s %q: %v", c.word, c.line, err)
		}
	}
	for _, c := range ps.guestCalls() {
		if c != "script gsfid" {
			t.Errorf("calls %v", ps.guestCalls())
			break
		}
	}
	if err := m.PlayStoreCheck(i, "gsfid"); err != nil {
		t.Errorf("check: %v", err)
	}
	if _, err := m.PlayStore(ctx, i, "gsfid"); errs.CodeOf(err) != errs.Usage {
		t.Errorf("PlayStore gsfid: %v", err)
	}
	l, ok, err := host.TryLock(m.lockPath(i, "bridge"))
	if err != nil || !ok {
		t.Fatalf("TryLock: %v %v", ok, err)
	}
	defer l.Unlock()
	if _, err := m.PlayStoreGSFID(ctx, i); errs.CodeOf(err) != errs.Busy {
		t.Errorf("bridge lock held: %v", err)
	}
}

// TestParsePlayStoreRun: the RESULT line is the result (the last wins), the rest are lines; fileLines keeps the file
// and placeholder lines.
func TestParsePlayStoreRun(t *testing.T) {
	run := parsePlayStoreRun("copied /product/a\r\nRESULT failed\n\nplay /product/a\nstaged /product/b.new\nstub hidden\npackage /x\nRESULT installed\n")
	if run.Result != playStoreInstalled || len(run.Lines) != 5 || run.Ran {
		t.Fatalf("%+v", run)
	}
	if got := fileLines(run.Lines); !slices.Equal(got, []string{"play /product/a", "staged /product/b.new", "stub hidden", "package /x"}) {
		t.Errorf("fileLines %q", got)
	}
	if parsePlayStoreRun("error: closed").Result != "" {
		t.Error("a result without RESULT")
	}
}
