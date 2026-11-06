//go:build windows

package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"modavd/internal/components"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// playStoreErr, when set, is what the fake manager's PlayStore returns.
var playStoreErr error

func (f *fakeManager) PlayStore(ctx context.Context, i int, action string) (*emu.PlayStoreResult, error) {
	f.record("playstore " + itoa(i) + " " + action)
	if playStoreErr != nil {
		return nil, playStoreErr
	}
	state := map[string]string{"install": "installed", "uninstall": "absent", "status": "installed"}[action]
	return &emu.PlayStoreResult{Index: i, Action: action, State: state, Installed: state == "installed",
		Files: []string{}, Warnings: []string{}}, nil
}

// TestCreateHasNoPlayStore: new instances never get the store; create takes no --play-store.
func TestCreateHasNoPlayStore(t *testing.T) {
	h := newHarness()
	var got []emu.CreateRequest
	h.fake.create = func(req emu.CreateRequest) (*emu.InstanceInfo, error) {
		got = append(got, req)
		in := sampleInstance(1)
		return &in, nil
	}
	if r := h.run(t, nil, "create", "--json"); r.code != 0 || len(got) != 1 {
		t.Fatalf("create: exit %d, %+v", r.code, got)
	}
	if r := h.run(t, nil, "create", "--play-store"); r.code != 2 || len(got) != 1 {
		t.Fatalf("create --play-store: exit %d, %d creates", r.code, len(got))
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
	if res := h.run(t, nil, "components", "fetch", "x"); res.code != 2 {
		t.Fatalf("fetch with a file: exit %d", res.code)
	}
}

// TestComponentsStatusPlayStore: status reports the store's five files apart from the components (never missing).
func TestComponentsStatusPlayStore(t *testing.T) {
	r := componentsRoot(t)
	h := newHarness()
	h.useRoot(r)
	res := h.run(t, nil, "--json", "components", "status")
	data := envelope(t, res.stdout)["data"].(map[string]any)
	ps, ok := data["play_store"].(map[string]any)
	if res.code != 0 || !ok || ps["ready"] != false || len(ps["files"].([]any)) != 5 || ps["source"] != "" {
		t.Fatalf("exit %d, play_store %v", res.code, data["play_store"])
	}
	for _, c := range data["components"].([]any) {
		if c.(map[string]any)["id"] == "play-store" {
			t.Error("the store is one of the components")
		}
	}
	res = h.run(t, nil, "components")
	if !strings.Contains(res.stdout, "✗ Google Play 商店 未安装（可选") {
		t.Errorf("human: %s", res.stdout)
	}
}

// TestComponentsImportPlayStore: import-playstore hands the files (a folder: the store's files in it) to the import,
// with the root; it takes no fetch option and needs a file; a path that is not there is not found, a refusal keeps
// its code, reason and exit code.
func TestComponentsImportPlayStore(t *testing.T) {
	r := componentsRoot(t)
	h := newHarness()
	h.useRoot(r)
	old := importPlayStore
	t.Cleanup(func() { importPlayStore = old })
	var got [][]components.ImportFile
	var fail error
	importPlayStore = func(ctx context.Context, rr *root.Root, files []components.ImportFile, o components.Options) (*components.PlayStoreImportResult, error) {
		if rr != r || o.Log == nil {
			t.Errorf("root %v, log %v", rr, o.Log)
		}
		got = append(got, files)
		if fail != nil {
			return nil, fail
		}
		return &components.PlayStoreImportResult{Root: rr.Dir, From: "files", Used: []string{"a"}, Kept: []string{},
			Status: components.PlayStoreStatus{Ready: true, Source: components.PlayStoreFromImport, Files: []components.PlayStoreFile{}}}, nil
	}
	user := testutil.TempDir(t)
	zip := filepath.Join(user, "x86_64-34_r14.zip")
	dir := filepath.Join(user, "five")
	for p, body := range map[string]string{zip: "zip", filepath.Join(dir, "Phonesky.apk"): "s", filepath.Join(dir, "other.txt"): "x"} {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}

	res := h.run(t, nil, "--json", "components", "import-playstore", zip, dir)
	if res.code != 0 || len(got) != 1 {
		t.Fatalf("exit %d %s %s", res.code, res.stdout, res.stderr)
	}
	if len(got[0]) != 2 || !slices.Equal([]string{got[0][0].Path, got[0][1].Path}, []string{zip, filepath.Join(dir, "Phonesky.apk")}) {
		t.Errorf("files %+v", got[0])
	}
	if data := envelope(t, res.stdout)["data"].(map[string]any); data["from"] != "files" {
		t.Errorf("data %v", data)
	}
	if res = h.run(t, nil, "components", "import-playstore", zip); res.code != 0 || !strings.Contains(res.stdout, "已用商店的文件补全 Google Play 商店") {
		t.Errorf("human: %d %s", res.code, res.stdout)
	}

	for _, args := range [][]string{{"components", "import-playstore"}, {"components", "import-playstore", zip, "--play-store"},
		{"components", "import-playstore", zip, "--accept-license"}} {
		if res := h.run(t, nil, args...); res.code != 2 {
			t.Errorf("%v: exit %d", args, res.code)
		}
	}
	if res := h.run(t, nil, "components", "import-playstore", filepath.Join(user, "gone.zip")); res.code != 3 {
		t.Errorf("gone: exit %d", res.code)
	}
	n := len(got)
	fail = errs.WithDetail(errs.New(errs.ComponentsRejected, "SHA-256 对不上"), errs.Reason, components.ImportReasonMismatch)
	res = h.run(t, nil, "--json", "components", "import-playstore", zip)
	e := envelope(t, res.stdout)["error"].(map[string]any)
	if res.code != 9 || e["code"] != "components_rejected" || e["details"].(map[string]any)["reason"] != "mismatch" || len(got) != n+1 {
		t.Errorf("refused: exit %d, %v", res.code, e)
	}
}

// TestPlayStoreCommand: playstore <i> install|uninstall|status reaches the manager; another action is a usage error;
// without --json the exit code says whether the store ended up as asked; a refusal keeps its reason.
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

	playStoreErr = errs.WithDetail(errs.New(errs.NotReady, "还没有五个文件"), errs.Reason, emu.PlayStoreReasonFiles)
	t.Cleanup(func() { playStoreErr = nil })
	r := h.run(t, nil, "--json", "playstore", "3", "install")
	e := envelope(t, r.stdout)["error"].(map[string]any)
	if r.code != 4 || e["code"] != "not_ready" || e["details"].(map[string]any)["reason"] != "playstore_files_missing" {
		t.Errorf("refused: exit %d, %v", r.code, e)
	}
}
