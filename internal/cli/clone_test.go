//go:build windows

package cli

import (
	"strings"
	"testing"
)

func TestRunClone(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "clone", "mas_1", "--title", "副本")
	if r.code != 0 || !strings.Contains(r.stdout, "已把实例 1 复制成") || !strings.Contains(r.stdout, "复制了 3.0 GB") {
		t.Fatalf("clone: %+v", r)
	}
	if len(h.fake.calls) != 1 || h.fake.calls[0] != "clone 1 副本" {
		t.Fatalf("calls %v", h.fake.calls)
	}
	if !strings.Contains(r.stderr, "复制中 50%") {
		t.Errorf("progress not logged: %q", r.stderr)
	}
	r = h.run(t, nil, "clone", "1", "--json")
	env := envelope(t, r.stdout)
	data, _ := env["data"].(map[string]any)
	if env["ok"] != true || data["source"] != float64(1) || data["copied_bytes"] != float64(3<<30) {
		t.Fatalf("json %v", env)
	}
	if r := h.run(t, nil, "clone"); r.code == 0 {
		t.Fatal("no index accepted")
	}
}