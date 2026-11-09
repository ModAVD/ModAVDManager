//go:build darwin && arm64 && cgo

package emu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

func nativeStoreManager(t *testing.T) (*Manager, *avd.Instance) {
	t.Helper()
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	in, err := avd.Create(r, 2, avd.CreateOptions{MemoryMB: 3072, CPU: 2, DataPartitionGB: 16, FPS: 30})
	if err != nil {
		t.Fatal(err)
	}
	return New(r, nil), in
}

type nativeStoreHooks struct {
	running    bool
	window     bool
	ready      bool
	results    map[string][]string
	calls      []string
	starts     []StartOptions
	onScript   func(string)
	startError error
}

func (h *nativeStoreHooks) install(t *testing.T) {
	t.Helper()
	a, b, c, d, e := playStoreFilesReady, playStoreFindQemu, playStoreStart, playStoreStop, playStoreScript
	t.Cleanup(func() {
		playStoreFilesReady, playStoreFindQemu, playStoreStart, playStoreStop, playStoreScript = a, b, c, d, e
	})
	playStoreFilesReady = func(string) bool { return h.ready }
	playStoreFindQemu = func(*Manager, int) (host.Process, bool, error) {
		p := host.Process{Name: "qemu-system-aarch64-headless"}
		if h.window {
			var found bool
			p, found = host.FindProcess(uint32(os.Getpid()))
			if !found {
				t.Fatal("cannot identify test process")
			}
			p.Name = "qemu-system-aarch64"
		}
		return p, h.running, nil
	}
	playStoreStart = func(_ *Manager, _ context.Context, _ int, opt StartOptions) (*StartResult, error) {
		h.calls = append(h.calls, "start")
		h.starts = append(h.starts, opt)
		if h.startError != nil {
			return nil, h.startError
		}
		return &StartResult{}, nil
	}
	playStoreStop = func(_ *Manager, ctx context.Context, _ int) ([]string, error) {
		h.calls = append(h.calls, "stop")
		if ctx.Err() != nil {
			t.Fatal("cleanup used a canceled context")
		}
		return nil, nil
	}
	playStoreScript = func(_ *Manager, _ context.Context, _ int, action string) (playStoreRun, error) {
		h.calls = append(h.calls, action)
		if h.onScript != nil {
			h.onScript(action)
		}
		words := h.results[action]
		if len(words) == 0 {
			t.Fatalf("unexpected action %s", action)
		}
		word := words[0]
		if len(words) > 1 {
			h.results[action] = words[1:]
		}
		if word == "?" {
			return playStoreRun{Ran: true}, errs.New(errs.LaunchFailed, "interrupted")
		}
		return playStoreRun{Ran: true, Result: word}, nil
	}
}

func TestNativePlayStoreInstallAndRemoval(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "offline", true: "windowed"}[running], func(t *testing.T) {
			m, in := nativeStoreManager(t)
			h := &nativeStoreHooks{ready: true, running: running, window: running, results: map[string][]string{
				"install": {playStoreNotWritable, playStoreInstalled}, "status": {playStoreInstalled},
				"uninstall": {playStoreReboot, playStoreGone},
			}}
			h.install(t)
			if err := m.PlayStoreCheck(in.Index, "install"); err != nil {
				t.Fatal(err)
			}
			if regularFile(m.playStoreMarker(in.Index)) {
				t.Fatal("precheck wrote the marker")
			}
			r, err := m.PlayStore(context.Background(), in.Index, "install")
			if err != nil || !r.Installed || !r.Restarted || r.Stopped == running {
				t.Fatalf("install: %+v %v", r, err)
			}
			if !regularFile(m.playStoreMarker(in.Index)) || regularFile(bridgeMarker(m.Root, in.Index)) {
				t.Fatal("native store did not keep its own boot request")
			}
			if got := playStoreRecord(m.Root.InstanceMeta(in.Index)); got != playStoreInstalled {
				t.Fatal(got)
			}
			h.results["status"] = []string{playStoreAbsent}
			h.calls = nil
			r, err = m.PlayStore(context.Background(), in.Index, "uninstall")
			if err != nil || r.State != playStoreAbsent || r.Stopped == running {
				t.Fatalf("uninstall: %+v %v", r, err)
			}
			if regularFile(m.playStoreMarker(in.Index)) || m.playStoreWanted(in.Index) {
				t.Fatal("confirmed removal left a writable request")
			}
			for _, opt := range h.starts {
				if opt.Window != running || !opt.bridgeLocked {
					t.Fatalf("mode was not preserved: %+v", opt)
				}
			}
			if !running && h.calls[len(h.calls)-1] != "stop" {
				t.Fatalf("originally offline instance was not stopped: %v", h.calls)
			}
		})
	}
}

func TestNativePlayStoreInterruptionKeepsRecovery(t *testing.T) {
	for _, action := range []string{"install", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			m, in := nativeStoreManager(t)
			h := &nativeStoreHooks{ready: true, results: map[string][]string{action: {"?"}}}
			h.install(t)
			r, err := m.PlayStore(context.Background(), in.Index, action)
			if err == nil || !r.Stopped || !regularFile(m.playStoreMarker(in.Index)) {
				t.Fatalf("%+v %v", r, err)
			}
			if got := playStoreRecord(m.Root.InstanceMeta(in.Index)); got != playStorePartial {
				t.Fatalf("lost pending state: %s", got)
			}
			args, err := m.playStoreLaunchArgs(in, []string{"-avd", in.Name, "-qemu"})
			if err != nil || !slices.Contains(args, "-writable-system") {
				t.Fatalf("recovery boot: %v %v", args, err)
			}
		})
	}
}

func TestNativePlayStoreDoesNotClearUnconfirmedRemoval(t *testing.T) {
	m, in := nativeStoreManager(t)
	h := &nativeStoreHooks{ready: true, results: map[string][]string{"uninstall": {playStoreGone}, "status": {playStorePartial}}}
	h.install(t)
	if err := m.setPlayStoreWanted(in.Index, true); err != nil {
		t.Fatal(err)
	}
	r, err := m.PlayStore(context.Background(), in.Index, "uninstall")
	if err == nil || !r.Stopped || !m.playStoreWanted(in.Index) || !regularFile(m.playStoreMarker(in.Index)) {
		t.Fatalf("unconfirmed removal lost recovery: %+v %v", r, err)
	}
}

func TestNativePlayStoreRetryIsBoundedAndLocksLifecycle(t *testing.T) {
	m, in := nativeStoreManager(t)
	h := &nativeStoreHooks{ready: true, results: map[string][]string{"install": {playStoreNotWritable}}}
	h.onScript = func(string) {
		if _, err := m.Start(context.Background(), in.Index, StartOptions{}); errs.CodeOf(err) != errs.Busy {
			t.Fatalf("concurrent start: %v", err)
		}
		if _, err := m.Stop(context.Background(), in.Index, StopOptions{}); errs.CodeOf(err) != errs.Busy {
			t.Fatalf("concurrent stop: %v", err)
		}
		if _, err := m.PlayStore(context.Background(), in.Index, "uninstall"); errs.CodeOf(err) != errs.Busy {
			t.Fatalf("concurrent removal: %v", err)
		}
	}
	h.install(t)
	r, err := m.PlayStore(context.Background(), in.Index, "install")
	if errs.CodeOf(err) != errs.NotReady || !r.Stopped {
		t.Fatalf("%+v %v", r, err)
	}
	if want := []string{"start", "install", "stop", "start", "install", "stop"}; !slices.Equal(h.calls, want) {
		t.Fatalf("reboot loop: %v", h.calls)
	}
}

func TestNativePlayStorePrechecksAndBootMarker(t *testing.T) {
	m, in := nativeStoreManager(t)
	h := &nativeStoreHooks{ready: false}
	h.install(t)
	if err := m.PlayStoreCheck(in.Index, "install"); errs.DetailsOf(err)[errs.Reason] != PlayStoreReasonFiles {
		t.Fatalf("files: %v", err)
	}
	if len(h.calls) != 0 || regularFile(m.playStoreMarker(in.Index)) {
		t.Fatal("precheck touched the guest")
	}
	for key, value := range map[string]string{"abi.type": "x86_64", "hw.cpu.arch": "x86_64", "image.sysdir.1": "system-images/android-34/google_apis/arm64-v8a/"} {
		old := in.Config.Value(key)
		in.Config.Set(key, value)
		if err := playStoreGuestError(in.Config); errs.CodeOf(err) != errs.Unsupported {
			t.Fatalf("%s: %v", key, err)
		}
		in.Config.Set(key, old)
	}
	args := []string{"-avd", in.Name, "-qemu", "-global", "x"}
	if got, err := m.playStoreLaunchArgs(in, slices.Clone(args)); err != nil || !slices.Equal(got, args) {
		t.Fatalf("ordinary boot changed: %v %v", got, err)
	}
	if err := m.setPlayStoreRecord(in.Index, jsonord.NewString(playStorePartial)); err != nil {
		t.Fatal(err)
	}
	got, err := m.playStoreLaunchArgs(in, slices.Clone(args))
	if err != nil || slices.Index(got, "-writable-system")+1 != slices.Index(got, "-qemu") {
		t.Fatalf("writable flag after -qemu: %v %v", got, err)
	}
	if err := os.Remove(m.playStoreMarker(in.Index)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(testutil.TempDir(t), "untouched")
	if err := os.WriteFile(outside, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, m.playStoreMarker(in.Index)); err != nil {
		t.Fatal(err)
	}
	if _, err := m.playStoreLaunchArgs(in, args); errs.CodeOf(err) != errs.PrecheckFailed {
		t.Fatalf("marker link: %v", err)
	}
	if b, err := os.ReadFile(outside); err != nil || strings.TrimSpace(string(b)) != "sentinel" {
		t.Fatal("marker link target changed")
	}
}

func TestNativePlayStoreEnrollsWritableBackings(t *testing.T) {
	m, in := nativeStoreManager(t)
	if err := m.writePlayStoreMarker(in.Index); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(m.Root.RuntimeSDK(), root.CurrentPlatform().SystemImageDir)
	if err := os.MkdirAll(image, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"system.img", "vendor.img"} {
		if err := os.WriteFile(filepath.Join(image, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.prepareBridgeBackings(t.Context(), in); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(in.Dir, bridgeBackingFile))
	if err != nil {
		t.Fatal(err)
	}
	var proofs map[string]backingProof
	if err := json.Unmarshal(b, &proofs); err != nil {
		t.Fatal(err)
	}
	if len(proofs) != 2 || proofs["system.img"].Path != filepath.Join(image, "system.img") || len(proofs["system.img"].SHA256) != 64 {
		t.Fatalf("native backing proof: %s", b)
	}
}
