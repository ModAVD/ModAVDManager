//go:build windows

package ui

import (
	"context"
	"sync"
	"testing"

	"modavd/internal/components"
	"modavd/internal/logx"
)

type sourceRecordingDiag struct {
	fakeDiag
	mu  sync.Mutex
	got []components.SourceSettings
}

func (d *sourceRecordingDiag) Fetch(ctx context.Context, accept bool, source components.SourceSettings, hash string, p func(DiagProgress)) (*DiagFetchResult, error) {
	d.mu.Lock()
	d.got = append(d.got, source)
	d.mu.Unlock()
	return &DiagFetchResult{Root: fakeRoot.Root, License: hash}, nil
}

func TestComponentsFetchPassesRequestSource(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	d := &sourceRecordingDiag{}
	h := newHarness(t, func(o *Options) { o.newDiagBackend = func(*logx.Logger) diagBackend { return d } })
	var j jobView
	expect(t, h.do("POST", "/api/components/fetch", `{"accept_license":true,"source":"tencent","license_sha256":"abc"}`), 202, &j)
	h.waitJob(j.ID, stateIs(stateDone))
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.got) != 1 || d.got[0].Mirror != "tencent" {
		t.Fatalf("source reaching backend: %+v", d.got)
	}
}
