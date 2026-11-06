//go:build !windows

package emu

import (
	"context"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/testutil"
	"modavd/internal/win"
)

func gateTree(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

func TestUnsupportedOperationsLeaveRootUntouched(t *testing.T) {
	dir := testutil.TempDir(t)
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	ctx := context.Background()
	index := 1
	before := gateTree(t, dir)
	calls := map[string]error{}
	_, calls["start"] = m.Start(ctx, index, StartOptions{})
	_, calls["create"] = m.Create(ctx, CreateRequest{Index: &index})
	_, calls["set"] = m.Update(ctx, index, UpdateRequest{})
	_, calls["delete"] = m.Delete(ctx, index)
	_, calls["stop"] = m.Stop(ctx, index, StopOptions{})
	_, calls["bridge"] = m.Bridge(ctx, index, "uninstall")
	_, calls["running"] = m.StopTargets(ctx)
	_, calls["list"] = m.List(ctx)
	_, calls["info"] = m.Info(ctx, index)
	_, calls["window show"] = m.SetWindowVisible(ctx, index, true)
	_, calls["window hide"] = m.SetWindowVisible(ctx, index, false)
	for name, callErr := range calls {
		if got := errs.CodeOf(callErr); got != errs.Unsupported {
			t.Errorf("%s: %v (%s)", name, callErr, got)
		}
	}
	if after := gateTree(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("root changed: before %v, after %v", before, after)
	}
}

func TestQemuOfRejectsUnsupportedHost(t *testing.T) {
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := New(r, nil).qemuOf(1)
	if found || errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("qemuOf: found=%v error=%v, want unsupported", found, err)
	}
}

func TestQemuScanRejectsUnsupportedCommandLine(t *testing.T) {
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	saved := listQemuProcesses
	t.Cleanup(func() { listQemuProcesses = saved })
	listQemuProcesses = func() ([]win.Process, error) {
		return []win.Process{{PID: 4242, Name: "qemu-system-x86_64"}}, nil
	}
	if _, err := scanQemuOf([]*root.Root{r}, []int{1}); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("lost command-line unsupported: %v", err)
	}
}

func TestBridgeGuestArchInputs(t *testing.T) {
	cases := []struct {
		name, key, value string
		want             bool
	}{
		{"arm64", "abi.type", "arm64", false},
		{"arm64-v8a", "abi.type", "arm64-v8a", false},
		{"aarch64", "hw.cpu.arch", "AArch64", false},
		{"missing", "hw.cpu.ncore", "4", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := avd.NewIni()
			cfg.Set(tc.key, tc.value)
			if got := bridgeGuestSupported(cfg); got != tc.want {
				t.Fatalf("bridgeGuestSupported = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBridgeWantedUsesConfig(t *testing.T) {
	dir := testutil.TempDir(t)
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := New(r, nil)
	for _, tc := range []struct {
		name, value string
		want        bool
	}{
		{"x86_64", "x86_64", true},
		{"arm64", "arm64", false},
		{"arm64-v8a", "arm64-v8a", false},
		{"aarch64", "aarch64", false},
		{"missing", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := avd.NewIni()
			if tc.value != "" {
				cfg.Set("abi.type", tc.value)
			}
			if got := m.bridgeWanted(1, cfg); got != tc.want {
				t.Fatalf("bridgeWanted = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNilBridgeGuestConfig(t *testing.T) {
	if got := bridgeGuestSupported(nil); !got {
		t.Fatal("nil config should retain the historical x86 behaviour")
	}
}
