package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/avd"
	"modavd/internal/emu"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

func TestStopCreatingInstance(t *testing.T) {
	r, err := root.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.EnsureMetadata(); err != nil {
		t.Fatal(err)
	}
	if _, err := avd.Create(r, 3, avd.CreateOptions{MemoryMB: 3072, CPU: 4, DataPartitionGB: 64, FPS: 60, Creating: true}); err != nil {
		t.Fatal(err)
	}
	l, ok, err := win.TryLock(filepath.Join(r.AvdHome(), ".mas_3.create.lock"))
	if err != nil || !ok {
		t.Fatalf("create lock: %v %v", ok, err)
	}
	t.Cleanup(l.Unlock)
	h := newHarness(t, nil, func(s *server) {
		s.newBackend = func(log *logx.Logger) (backend, error) { return emu.New(r, log), nil }
	})
	for n, args := range []map[string]any{
		{"index": 3}, {"index": 3, "force": true}, {"index": 3, "cancel_create": true},
	} {
		text, isErr, sc := toolText(t, h.call(n, "stop_instance", args, nil))
		if n < 2 {
			if !isErr || !strings.Contains(text, "--cancel-create") || !strings.HasSuffix(text, "错误码: busy") {
				t.Fatalf("default stop: %s", text)
			}
		} else if isErr || sc["interrupted_create"] != true || sc["method"] != "already_offline" {
			t.Fatalf("explicit cancel: %s, %+v", text, sc)
		}
	}
}
