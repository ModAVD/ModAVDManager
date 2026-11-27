// Copyright 2026 Ji Mao Cai
package root

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestRuntimeSnapshotKeepsRoutingButNotPolicy(t *testing.T) {
	r, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.MetadataPath(), []byte(`{"portBase":21000,"adbServerPort":21050,"scriptAdbServerPort":21049,"sdkRoot":"alternate-sdk","instances":{"0":{"diagnostics":true}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned, err := r.SnapshotRuntime()
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pinned.RuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SDKRoot != filepath.Join(r.Dir, "alternate-sdk") || cfg.PortBase != 21000 || cfg.AdbServerPort != 21050 {
		t.Fatalf("snapshot=%+v", cfg)
	}
	if err := os.WriteFile(r.MetadataPath(), []byte(`{"instances":`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.SnapshotRuntime(); err == nil {
		t.Fatal("guessed a target from bad JSON")
	}
	if same, err := pinned.SnapshotRuntime(); err != nil || same != pinned {
		t.Fatal("lost existing trusted target", err)
	}
	if pinned.RuntimeSDK() != filepath.Join(r.Dir, "alternate-sdk") || pinned.AdbServerPort() != 21050 || pinned.Ports().Serial(0) != "emulator-21000" || pinned.CheckAdbPorts() != nil {
		t.Fatal("bad JSON retargeted runtime")
	}
	if _, err := pinned.ReadMetadata(); err == nil {
		t.Fatal("snapshot froze deletion/policy metadata")
	}
}

func TestRuntimeSnapshotPreservesDefaultSDKAndLegacyAdb(t *testing.T) {
	for _, base := range []int{20000, 5500, 49100} {
		t.Run(strconv.Itoa(base), func(t *testing.T) {
			r, err := New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			config := RuntimeConfig{PortBase: base, AdbServerPort: 34050, ScriptAdbServerPort: 34049}
			pinned, err := r.WithRuntime(config)
			if err != nil {
				t.Fatal(err)
			}
			if pinned.LocalSDKRoot() != "" || pinned.RuntimeSDK() != r.SDK() {
				t.Fatal("default SDK became an override")
			}
			if pinned.Ports().Base != base || pinned.CheckAdbPorts() != nil {
				t.Fatal("legacy adb target refused")
			}
			if base != 20000 && pinned.CheckPorts() == nil {
				t.Fatal("snapshot relaxed new launch policy")
			}
		})
	}
	r, _ := New(t.TempDir())
	for _, c := range []RuntimeConfig{
		{PortBase: 21000, AdbServerPort: 20050, ScriptAdbServerPort: 34049},
		{PortBase: 21000, AdbServerPort: 5037, ScriptAdbServerPort: 34049},
		{PortBase: 21000, AdbServerPort: 21000, ScriptAdbServerPort: 34049},
		{PortBase: 21000, AdbServerPort: 34050, ScriptAdbServerPort: 34050},
		{PortBase: 21000, AdbServerPort: 34050, ScriptAdbServerPort: 34049, SDKRoot: "relative-sdk"},
	} {
		if _, err := r.WithRuntime(c); err == nil {
			t.Fatalf("accepted unsafe target %+v", c)
		}
	}
}
