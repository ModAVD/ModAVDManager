//go:build windows

package ui

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"modavd/internal/components"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// storeDiag is a diagnostic backend that records what each fetch asked about the Google Play Store.
type storeDiag struct {
	fakeDiag
	mu    sync.Mutex
	asked []bool
}

func (d *storeDiag) Fetch(ctx context.Context, accept, playStore bool, s components.SourceSettings, sha string, progress func(DiagProgress)) (*DiagFetchResult, error) {
	d.mu.Lock()
	d.asked = append(d.asked, playStore)
	d.mu.Unlock()
	return &DiagFetchResult{Root: fakeRoot.Root}, nil
}

// TestPlayStoreRequests: play_store in a create body reaches the create request, and in a fetch body the backend;
// without it, neither asks for the store.
func TestPlayStoreRequests(t *testing.T) {
	d := &storeDiag{}
	h := newHarness(t, func(o *Options) { o.newDiagBackend = func(*logx.Logger) diagBackend { return d } })
	for _, body := range []any{map[string]any{"index": 2}, map[string]any{"index": 3, "play_store": true}} {
		var v jobView
		expect(t, h.do("POST", "/api/instances", body), http.StatusAccepted, &v)
		h.waitJob(v.ID, stateIs(stateDone))
	}
	for _, extra := range []string{"", `,"play_store":true`} {
		var j jobView
		expect(t, h.do("POST", "/api/components/fetch", fetchBody(extra)), http.StatusAccepted, &j)
		h.waitJob(j.ID, stateIs(stateDone))
	}
	h.f.mu.Lock()
	c := h.f.created
	h.f.mu.Unlock()
	if len(c) != 2 || c[0].PlayStore || !c[1].PlayStore {
		t.Fatalf("create requests %+v", c)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.asked) != 2 || d.asked[0] || !d.asked[1] {
		t.Fatalf("fetches asked %v", d.asked)
	}
}

// TestRealDiagBackendPassesPlayStore: the real adapter hands components.Fetch the play_store choice.
func TestRealDiagBackendPassesPlayStore(t *testing.T) {
	old := diagFetch
	t.Cleanup(func() { diagFetch = old })
	var got []components.Options
	diagFetch = func(_ context.Context, r *root.Root, o components.Options) (*components.Result, error) {
		got = append(got, o)
		return &components.Result{Root: r.Dir}, nil
	}
	b := newRealDiagBackend(&root.Root{Dir: `C:\root`}, logx.Nop())
	for _, store := range []bool{false, true} {
		if _, err := b.Fetch(context.Background(), true, store, components.SourceSettings{}, fakeHash, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0].PlayStore || !got[1].PlayStore {
		t.Fatalf("options %+v", got)
	}
}
