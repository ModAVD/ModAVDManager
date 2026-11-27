//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"modavd/internal/emu"
)

func TestDiskInfoUnavailableAllocation(t *testing.T) {
	r := &emu.DiskInfoResult{Index: 3, LogicalBytes: 8 << 30, Warnings: []string{"正在整理磁盘"}}
	var out bytes.Buffer
	formatDiskInfo(&out, r)
	if strings.Contains(out.String(), "宿主上占用") || !strings.Contains(out.String(), "正在整理磁盘") {
		t.Fatal(out.String())
	}
	b, err := json.Marshal(r)
	if err != nil || strings.Contains(string(b), "on_disk_bytes") {
		t.Fatalf("unknown: %s %v", b, err)
	}
	r.OnDiskBytes = new(int64(0))
	b, err = json.Marshal(r)
	if err != nil || !strings.Contains(string(b), `"on_disk_bytes":0`) {
		t.Fatalf("known zero: %s %v", b, err)
	}
}
