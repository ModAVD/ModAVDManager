//go:build windows

package ui

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

type compactResultBackend struct {
	backend
	result *emu.DiskCompactResult
}

type compactCancelBackend struct {
	backend
	started chan struct{}
}

func (b compactCancelBackend) DiskCompact(ctx context.Context, _ int) (*emu.DiskCompactResult, error) {
	close(b.started)
	<-ctx.Done()
	return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消整理")
}

func TestCompactCancelGUI(t *testing.T) {
	started := make(chan struct{})
	h := newHarness(t, func(opt *Options) {
		opt.newBackend = func(l *logx.Logger) backend { return compactCancelBackend{fakeBackend{newFake(), l}, started} }
	})
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/disk-compact", `{}`), http.StatusAccepted, &j)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("compact did not start")
	}
	expect(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", `{}`), http.StatusOK, nil)
	v := h.waitJob(j.ID, stateIs(stateCanceled))
	if v.Error == nil || v.Error.Code != errs.Canceled {
		t.Fatalf("%+v", v)
	}
}

func (b compactResultBackend) DiskCompact(context.Context, int) (*emu.DiskCompactResult, error) {
	return b.result, nil
}

func TestCompactResultGUI(t *testing.T) {
	for _, skip := range []bool{false, true} {
		delta := int64(4096)
		want := "占用增加 4 KB"
		if skip {
			delta = 0
			want = "已跳过重写"
		}
		r := &emu.DiskCompactResult{Index: 3, BeforeOnDiskBytes: 1 << 30, AfterOnDiskBytes: 1<<30 + delta, FreedBytes: 0, OnDiskDeltaBytes: delta, Skipped: skip, ReclaimThresholdBytes: 16 << 20}
		h := newHarness(t, func(opt *Options) {
			opt.newBackend = func(l *logx.Logger) backend { return compactResultBackend{fakeBackend{newFake(), l}, r} }
		})
		var raw map[string]any
		j := diskJob(t, h, 3, kindDiskCompact, &raw)
		if j.State != stateDone || j.Progress == nil || !strings.Contains(j.Progress.Message, want) {
			t.Fatalf("%+v", j)
		}
		if raw["on_disk_delta_bytes"] != float64(delta) || raw["freed_bytes"] != float64(0) || raw["skipped"] != skip || raw["before_on_disk_bytes"] != float64(r.BeforeOnDiskBytes) || raw["after_on_disk_bytes"] != float64(r.AfterOnDiskBytes) {
			t.Fatalf("%v", raw)
		}
	}
}
