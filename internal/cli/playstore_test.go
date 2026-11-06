//go:build windows

package cli

import (
	"context"
	"slices"
	"testing"

	"modavd/internal/components"
	"modavd/internal/emu"
	"modavd/internal/root"
)

func (f *fakeManager) PlayStore(ctx context.Context, i int, action string) (*emu.PlayStoreResult, error) {
	f.record("playstore " + itoa(i) + " " + action)
	state := map[string]string{"install": "installed", "uninstall": "absent", "status": "installed"}[action]
	return &emu.PlayStoreResult{Index: i, Action: action, State: state, Installed: state == "installed",
		Files: []string{}, Warnings: []string{}}, nil
}

// TestCreatePlayStoreFlag: create asks for the store only with --play-store; --resume takes no --play-store.
func TestCreatePlayStoreFlag(t *testing.T) {
	h := newHarness()
	var got []emu.CreateRequest
	h.fake.create = func(req emu.CreateRequest) (*emu.InstanceInfo, error) {
		got = append(got, req)
		in := sampleInstance(1)
		return &in, nil
	}
	if r := h.run(t, nil, "create", "--json"); r.code != 0 || len(got) != 1 || got[0].PlayStore {
		t.Fatalf("without --play-store: exit %d, %+v", r.code, got)
	}
	if r := h.run(t, nil, "create", "--play-store", "--json"); r.code != 0 || len(got) != 2 || !got[1].PlayStore {
		t.Fatalf("--play-store: exit %d, %+v", r.code, got)
	}
	if r := h.run(t, nil, "create", "--resume", "6", "--play-store"); r.code != 2 || len(got) != 2 {
		t.Fatalf("--resume --play-store: exit %d, %d creates", r.code, len(got))
	}
}

// TestComponentsFetchPlayStoreFlag: components fetch passes --play-store; status does not take it.
func TestComponentsFetchPlayStoreFlag(t *testing.T) {
	r := componentsRoot(t)
	h := newHarness()
	h.useRoot(r)
	var got []components.Options
	h.fetch = func(ctx context.Context, rr *root.Root, o components.Options) (*components.Result, error) {
		got = append(got, o)
		return &components.Result{Root: rr.Dir, Components: []components.ComponentResult{}, Downloads: []string{}}, nil
	}
	h.run(t, nil, "--json", "components", "fetch", "--accept-license")
	h.run(t, nil, "--json", "components", "fetch", "--accept-license", "--play-store")
	if len(got) != 2 || got[0].PlayStore || !got[1].PlayStore {
		t.Fatalf("options %+v", got)
	}
	if res := h.run(t, nil, "components", "status", "--play-store"); res.code != 2 {
		t.Fatalf("status --play-store: exit %d", res.code)
	}
}

// TestPlayStoreCommand: playstore <i> install|uninstall|status reaches the manager; another action is a usage error;
// without --json the exit code says whether the store ended up as asked.
func TestPlayStoreCommand(t *testing.T) {
	h := newHarness()
	for _, a := range []string{"install", "uninstall", "status"} {
		if r := h.run(t, nil, "playstore", "3", a); r.code != 0 {
			t.Errorf("%s: exit %d %s", a, r.code, r.stderr)
		}
	}
	if r := h.run(t, nil, "playstore", "3", "remove"); r.code != 2 {
		t.Errorf("remove: exit %d", r.code)
	}
	if want := []string{"playstore 3 install", "playstore 3 uninstall", "playstore 3 status"}; !slices.Equal(h.fake.calls, want) {
		t.Errorf("calls %v", h.fake.calls)
	}
	if exitPlayStore(&emu.PlayStoreResult{Action: "install", State: "partial"}, false) != 1 ||
		exitPlayStore(&emu.PlayStoreResult{Action: "uninstall", State: "installed"}, false) != 1 ||
		exitPlayStore(&emu.PlayStoreResult{Action: "install", State: "partial"}, true) != 0 {
		t.Error("exit codes")
	}
}
