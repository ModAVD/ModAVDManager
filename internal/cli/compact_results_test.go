//go:build windows

package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/logx"
)

type compactResultManager struct {
	manager
	result *emu.DiskCompactResult
}

func (m compactResultManager) DiskCompact(context.Context, int) (*emu.DiskCompactResult, error) {
	return m.result, nil
}

func TestCompactResultCLI(t *testing.T) {
	for _, c := range []struct {
		name          string
		before, after int64
		skip          bool
		want          string
	}{
		{"shrink", 2 << 30, 1 << 30, false, "省下 1.0 GB"},
		{"equal", 1 << 30, 1 << 30, false, "省下 0.0 GB"},
		{"growth", 1 << 30, 1<<30 + 1, false, "增加 1 字节"},
		{"skip", 1 << 30, 1 << 30, true, "跳过重写"},
	} {
		t.Run(c.name, func(t *testing.T) {
			delta := c.after - c.before
			r := &emu.DiskCompactResult{Index: 3, BeforeOnDiskBytes: c.before, AfterOnDiskBytes: c.after, FreedBytes: max(0, -delta), OnDiskDeltaBytes: delta, Skipped: c.skip, ReclaimThresholdBytes: 16 << 20}
			for _, jsonMode := range []bool{false, true} {
				var out, se bytes.Buffer
				args := []string{"disk", "3", "compact"}
				if jsonMode {
					args = append([]string{"--json"}, args...)
				}
				code := run(context.Background(), args, strings.NewReader(""), &out, &se, deps{newManager: func(_ string, l *logx.Logger) (manager, error) {
					return compactResultManager{&fakeManager{log: l}, r}, nil
				}})
				if code != 0 {
					t.Fatalf("%d: %s", code, &se)
				}
				if !jsonMode {
					if !strings.Contains(out.String(), c.want) || (delta > 0 && strings.Contains(out.String(), "省下")) {
						t.Fatalf("%s", &out)
					}
				} else {
					m := envelope(t, out.String())
					data := m["data"].(map[string]any)
					if data["on_disk_delta_bytes"] != float64(delta) || data["freed_bytes"] != float64(max(0, -delta)) || data["skipped"] != c.skip || data["before_on_disk_bytes"] != float64(c.before) || data["after_on_disk_bytes"] != float64(c.after) {
						t.Fatalf("%v", data)
					}
				}
			}
		})
	}
}
