//go:build windows

package importer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverFakeLDPlayerAndMuMu(t *testing.T) {
	d := t.TempDir()
	ld := filepath.Join(d, "ld")
	if err := os.MkdirAll(filepath.Join(ld, "vms", "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	ldCfg := map[string]any{"statusSettings": map[string]any{"playerName": "auto1"}, "advancedSettings": map[string]any{"resolution": "1920x1080"}}
	b, _ := json.Marshal(ldCfg)
	if err := os.WriteFile(filepath.Join(ld, "vms", "config", "leidian1.config"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	mu := filepath.Join(d, "mumu")
	if err := os.MkdirAll(filepath.Join(mu, "vms", "MuMuPlayer-15.0-0", "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	muCfg := []byte(`{"vm":{"nat":{"port_forward":{"adb":{"host_port":16384}}}}}`)
	if err := os.WriteFile(filepath.Join(mu, "vms", "MuMuPlayer-15.0-0", "configs", "vm_config.json"), muCfg, 0o644); err != nil {
		t.Fatal(err)
	}
	got := Discover(Snapshot{Roots: []string{ld, mu}})
	if len(got) != 2 {
		t.Fatalf("providers = %#v", got)
	}
	if got[0].Kind != KindLDPlayer || got[0].Instances[0].ADBPort != 5557 {
		t.Fatalf("ldplayer = %#v", got[0])
	}
	if got[1].Kind != KindMuMu || got[1].Instances[0].ADBPort != 16384 {
		t.Fatalf("mumu = %#v", got[1])
	}
}

func TestParseRegistryOutput(t *testing.T) {
	v := ParseRegistryOutput("InstallLocation    REG_SZ    C:\\\\Program Files\\\\Netease\\\\MuMu\r\nDisplayVersion REG_SZ 14.0")
	if v["InstallLocation"] == "" || v["DisplayVersion"] != "14.0" {
		t.Fatalf("values = %#v", v)
	}
}
