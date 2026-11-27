//go:build windows

package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/logx"
)

type compactResultBackend struct {
	backend
	result *emu.DiskCompactResult
}

func (b compactResultBackend) DiskCompact(context.Context, int) (*emu.DiskCompactResult, error) {
	return b.result, nil
}

func TestCompactResultMCP(t *testing.T) {
	for _, skip := range []bool{false, true} {
		delta := int64(4096)
		if skip {
			delta = 0
		}
		estimate := int64(0)
		r := &emu.DiskCompactResult{Index: 2, BeforeOnDiskBytes: 1 << 30, AfterOnDiskBytes: 1<<30 + delta, FreedBytes: 0, OnDiskDeltaBytes: delta, Skipped: skip, EstimatedReclaimableBytes: &estimate, ReclaimThresholdBytes: 16 << 20}
		h := newHarness(t, nil, func(s *server) {
			s.newBackend = func(*logx.Logger) (backend, error) { return compactResultBackend{&fake{}, r}, nil }
		})
		var schema any
		for _, tool := range h.toolsList(true) {
			if tool["name"] == "compact_disk" {
				schema = tool["outputSchema"]
			}
		}
		text, isErr, sc := toolText(t, h.call(1, "compact_disk", map[string]any{"index": 2}, nil))
		if isErr {
			t.Fatal(text)
		}
		if err := validate(schema, sc, "compact"); err != nil {
			t.Fatal(err)
		}
		if sc["on_disk_delta_bytes"] != float64(delta) || sc["freed_bytes"] != float64(0) || sc["skipped"] != skip || sc["before_on_disk_bytes"] != float64(r.BeforeOnDiskBytes) || sc["after_on_disk_bytes"] != float64(r.AfterOnDiskBytes) {
			t.Fatalf("%v", sc)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(text), &decoded); err != nil || decoded["on_disk_delta_bytes"] != sc["on_disk_delta_bytes"] {
			t.Fatalf("text/structuredContent disagree: %s %v", text, err)
		}
	}
}
