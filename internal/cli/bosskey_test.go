//go:build windows

package cli

import (
	"strings"
	"testing"

	"modavd/internal/emu"
)

// set --boss-key passes the key through (the manager validates it), none included; info shows the key.
func TestSetBossKey(t *testing.T) {
	for _, c := range []struct{ arg, stored, human string }{
		{"ctrl+alt+shift+7", "Ctrl+Alt+Shift+7", "老板键 Ctrl+Alt+Shift+7"},
		{"none", "", "老板键 无"},
	} {
		h := newHarness()
		h.fake.update = func(i int, req emu.UpdateRequest) (*emu.UpdateResult, error) {
			if i != 3 || req.BossKey == nil || *req.BossKey != c.arg || req.Title != nil || req.Diagnostics != nil {
				t.Fatalf("request = %d/%+v", i, req)
			}
			in := sampleInstance(i)
			in.BossKey = c.stored
			return &emu.UpdateResult{Instance: in, Applied: map[string]any{"boss_key": c.stored}}, nil
		}
		r := h.run(t, nil, "set", "3", "--boss-key", c.arg)
		if r.code != 0 || !strings.Contains(r.stdout, c.human) || strings.Contains(r.stdout, "下次开机") {
			t.Fatalf("%s: %+v", c.arg, r)
		}
		r = h.run(t, nil, "set", "3", "--boss-key", c.arg, "--json")
		if r.code != 0 {
			t.Fatalf("%s --json: %+v", c.arg, r)
		}
		data := envelope(t, r.stdout)["data"].(map[string]any)
		if data["instance"].(map[string]any)["boss_key"] != c.stored || data["applied"].(map[string]any)["boss_key"] != c.stored {
			t.Fatalf("%s JSON: %s", c.arg, r.stdout)
		}
	}
}

func TestBossKeyInInfo(t *testing.T) {
	h := newHarness()
	in := sampleInstance(3)
	in.BossKey = "Ctrl+Alt+Shift+3"
	h.fake.info = func(int) (*emu.InstanceInfo, error) { return &in, nil }
	if r := h.run(t, nil, "info", "3"); !strings.Contains(r.stdout, "老板键：Ctrl+Alt+Shift+3") {
		t.Fatal(r.stdout)
	}
	if r := h.run(t, nil, "info", "3", "--json"); !strings.Contains(r.stdout, `"boss_key":"Ctrl+Alt+Shift+3"`) {
		t.Fatal(r.stdout)
	}
	in.BossKey = ""
	if r := h.run(t, nil, "info", "3"); !strings.Contains(r.stdout, "老板键：无") {
		t.Fatal(r.stdout)
	}
	if r := h.run(t, nil, "help", "set"); !strings.Contains(r.stdout, "--boss-key") {
		t.Fatal(r.stdout)
	}
}
