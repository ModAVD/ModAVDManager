//go:build windows

// Copyright 2026 Ji Mao Cai
package mcp

import "testing"

func TestDiagnosticsMCPReadScopeAndWriteRejection(t *testing.T) {
	h := newHarness(t, &fake{diagnostics: true}, nil)
	_, failed, list := toolText(t, h.call(1, "list_instances", map[string]any{}, nil))
	if failed {
		t.Fatal("list failed")
	}
	instances := list["instances"].([]any)
	if instances[1].(map[string]any)["diagnostics"] != true || instances[0].(map[string]any)["diagnostics"] != false {
		t.Fatalf("list flags = %v", instances)
	}
	_, failed, info := toolText(t, h.call(2, "get_instance", map[string]any{"index": 1}, nil))
	if failed {
		t.Fatal("get failed")
	}
	for _, key := range []string{"diagnostics", "diagnostics_since", "diagnostics_cleanup"} {
		if _, exists := info[key]; exists {
			t.Fatalf("get_instance exposed %s", key)
		}
	}
	text, failed, _ := toolText(t, h.call(3, "update_instance", map[string]any{"index": 1, "diagnostics": false}, nil))
	if !failed {
		t.Fatal("MCP accepted diagnostic deletion switch", text)
	}
	if len(h.f.updates) != 0 {
		t.Fatal("invalid diagnostic argument reached backend")
	}
	_, failed, update := toolText(t, h.call(4, "update_instance", map[string]any{"index": 1, "cpu": 4}, nil))
	if failed {
		t.Fatal("ordinary update failed")
	}
	if _, exists := update["instance"].(map[string]any)["diagnostics"]; exists {
		t.Fatal("nested instance leaked diagnostic fields")
	}
	for _, tool := range h.toolsList(true) {
		if tool["name"] == "get_instance" {
			properties := tool["outputSchema"].(map[string]any)["properties"].(map[string]any)
			if _, exists := properties["diagnostics"]; exists {
				t.Fatal("get schema still advertises diagnostics")
			}
		}
	}
}
