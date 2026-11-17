//go:build windows

package cli

import (
	"strings"
	"testing"

	"modavd/internal/emu"
)

// TestSoundSet: set --sound on|off reaches the manager as Sound alone and comes back in JSON and for people
// (ModAVD/ModAVD#226); info shows the switch.
func TestSoundSet(t *testing.T) {
	for _, value := range []string{"on", "off", "ON"} {
		h := newHarness()
		on := strings.EqualFold(value, "on")
		h.fake.update = func(i int, req emu.UpdateRequest) (*emu.UpdateResult, error) {
			if i != 3 || req.Sound == nil || *req.Sound != on || req.MemoryMB != nil || req.CPU != nil || req.Diagnostics != nil {
				t.Fatalf("request = %d/%+v", i, req)
			}
			in := sampleInstance(i)
			in.Config.Sound = on
			return &emu.UpdateResult{Instance: in, Applied: map[string]any{"sound": on}}, nil
		}
		result := h.run(t, nil, "set", "3", "--sound", value, "--json")
		if result.code != 0 {
			t.Fatalf("%s: CLI = %d/%s", value, result.code, result.stdout)
		}
		data := envelope(t, result.stdout)["data"].(map[string]any)
		if data["instance"].(map[string]any)["config"].(map[string]any)["sound"] != on || data["applied"].(map[string]any)["sound"] != on {
			t.Fatalf("%s: JSON = %s", value, result.stdout)
		}
		want := "声音 关"
		if on {
			want = "声音 开"
		}
		if human := h.run(t, nil, "set", "3", "--sound", value); human.code != 0 || !strings.Contains(human.stdout, want) {
			t.Fatalf("%s: human = %+v", value, human)
		}
	}
	for _, value := range []string{"true", "1", "maybe", ""} {
		h := newHarness()
		if result := h.run(t, nil, "set", "3", "--sound", value, "--json"); result.code != 2 || len(h.fake.callList()) != 0 {
			t.Fatalf("%q = %+v / %q", value, result, h.fake.callList())
		}
	}
	h := newHarness()
	in := sampleInstance(3)
	in.Config.Sound = true
	h.fake.info = func(int) (*emu.InstanceInfo, error) { return &in, nil }
	if result := h.run(t, nil, "info", "3"); !strings.Contains(result.stdout, "声音：开") {
		t.Fatalf("info: %s", result.stdout)
	}
}
