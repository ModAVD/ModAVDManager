//go:build windows

// Copyright 2026 Ji Mao Cai
package cli

import (
	"strings"
	"testing"
	"time"

	"modavd/internal/emu"
)

func TestDiagnosticsSetDispatchesBooleanAndReportsCleanup(t *testing.T) {
	for _, value := range []string{"on", "off"} {
		t.Run(value, func(t *testing.T) {
			h := newHarness()
			enabled := value == "on"
			h.fake.update = func(i int, req emu.UpdateRequest) (*emu.UpdateResult, error) {
				if i != 3 || req.Diagnostics == nil || *req.Diagnostics != enabled || req.MemoryMB != nil || req.CPU != nil || req.DataPartitionGB != nil {
					t.Fatalf("request = %d/%+v", i, req)
				}
				in := sampleInstance(i)
				in.Diagnostics = enabled
				return &emu.UpdateResult{Instance: in, Applied: map[string]any{"diagnostics": enabled}, DiagnosticsCleanup: &emu.DiagnosticsCleanup{Files: 2, Bytes: 128}}, nil
			}
			result := h.run(t, nil, "set", "3", "--diagnostics", value, "--json")
			if result.code != 0 {
				t.Fatalf("CLI = %d/%s", result.code, result.stdout)
			}
			data := envelope(t, result.stdout)["data"].(map[string]any)
			if data["instance"].(map[string]any)["diagnostics"] != enabled {
				t.Fatal("JSON diagnostics flag lost")
			}
			human := h.run(t, nil, "set", "3", "--diagnostics", value)
			if human.code != 0 || !strings.Contains(human.stdout, "诊断模式") {
				t.Fatalf("human = %+v", human)
			}
			if !enabled && !strings.Contains(human.stdout, "清理日志 2 份，释放 128 字节") {
				t.Fatalf("actual cleanup omitted: %s", human.stdout)
			}
		})
	}
}

func TestDiagnosticInfoHumanTimeIsLocalAndJSONRemainsUTC(t *testing.T) {
	saved := time.Local
	time.Local = time.FixedZone("CST", 8*60*60)
	t.Cleanup(func() { time.Local = saved })
	h := newHarness()
	in := sampleInstance(3)
	in.Diagnostics = true
	in.DiagnosticsSince = "2026-11-05T12:00:00Z"
	h.fake.info = func(int) (*emu.InstanceInfo, error) { return &in, nil }
	human := h.run(t, nil, "info", "3")
	if !strings.Contains(human.stdout, "2026-11-05 20:00:00 CST") {
		t.Fatal(human.stdout)
	}
	encoded := h.run(t, nil, "info", "3", "--json")
	if !strings.Contains(encoded.stdout, `"diagnostics_since":"2026-11-05T12:00:00Z"`) {
		t.Fatal(encoded.stdout)
	}
}

func TestInvalidDiagnosticsNeverReachesManager(t *testing.T) {
	for _, value := range []string{"true", "false", "maybe", ""} {
		h := newHarness()
		result := h.run(t, nil, "set", "3", "--diagnostics", value, "--json")
		if result.code != 2 || len(h.fake.callList()) != 0 {
			t.Fatalf("%q = %+v / %q", value, result, h.fake.callList())
		}
	}
}

func TestDiagnosticsVisibleInInfoAndList(t *testing.T) {
	h := newHarness()
	in := sampleInstance(3)
	in.Diagnostics = true
	in.DiagnosticsSince = "2026-11-05T12:00:00Z"
	h.fake.info = func(int) (*emu.InstanceInfo, error) { return &in, nil }
	h.fake.list = func() (*emu.ListResult, error) { return &emu.ListResult{Instances: []emu.InstanceInfo{in}}, nil }
	for _, args := range [][]string{{"info", "3"}, {"list"}, {"info", "3", "--json"}, {"list", "--json"}} {
		result := h.run(t, nil, args...)
		if result.code != 0 || !(strings.Contains(result.stdout, "诊断模式") || strings.Contains(result.stdout, `"diagnostics":true`)) {
			t.Fatalf("%q = %+v", args, result)
		}
	}
}
