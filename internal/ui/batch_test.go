//go:build windows

package ui

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/errs"
)

// TestCreateBatch: count > 1 takes the free indexes at once and starts a create job for each; they run one after
// another (the later ones queued), with the title numbered; low disk before one fails that one, the others are
// unaffected (ModAVD/ModAVD#240).
func TestCreateBatch(t *testing.T) {
	h := newHarness(t, nil)
	gate := make(chan struct{})
	var spaceCalls atomic.Int32
	h.f.mu.Lock()
	h.f.createGate = gate
	h.f.spaceErr = func() error {
		if spaceCalls.Add(1) == 3 {
			return errs.New(errs.PrecheckFailed, "实例所在盘 C:\\ 剩余 3.0 GB，低于开机所需的 8 GB")
		}
		return nil
	}
	h.f.mu.Unlock()

	var resp struct {
		Jobs []jobView `json:"jobs"`
	}
	expect(t, h.do("POST", "/api/instances", map[string]any{"count": 3, "title": "挂机", "memory_mb": 4096}), http.StatusAccepted, &resp)
	if len(resp.Jobs) != 3 || resp.Jobs[0].Index != 2 || resp.Jobs[1].Index != 6 || resp.Jobs[2].Index != 7 {
		t.Fatalf("jobs = %+v", resp.Jobs)
	}
	for _, j := range resp.Jobs {
		if j.Kind != kindCreate || j.State != stateRunning {
			t.Fatalf("job %+v", j)
		}
	}
	// the second waits for the first, which is held at the gate
	h.waitJob(resp.Jobs[1].ID, func(v jobView) bool { return v.Progress != nil && v.Progress.Step == "queued" })
	time.Sleep(50 * time.Millisecond)
	h.f.mu.Lock()
	n := len(h.f.created)
	h.f.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d creates while the first is not done", n)
	}
	close(gate)
	h.waitJob(resp.Jobs[0].ID, stateIs(stateDone))
	h.waitJob(resp.Jobs[1].ID, stateIs(stateDone))
	failed := h.waitJob(resp.Jobs[2].ID, stateIs(stateFailed))
	if failed.Error == nil || failed.Error.Code != errs.PrecheckFailed {
		t.Fatalf("third job %+v", failed)
	}
	h.f.mu.Lock()
	c := h.f.created
	h.f.mu.Unlock()
	if len(c) != 2 || *c[0].Index != 2 || c[0].Title != "挂机 1" || *c[1].Index != 6 || c[1].Title != "挂机 2" ||
		*c[1].MemoryMB != 4096 || c[1].Progress == nil {
		t.Fatalf("creates %+v", c)
	}

	// refused before any job: a bad count, an index with a count, too few free indexes
	expectError(t, h.do("POST", "/api/instances", `{"count":0}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances", `{"count":10}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances", `{"count":2,"index":8}`), 400, errs.Usage)
	expectError(t, h.do("POST", "/api/instances", `{"count":9}`), http.StatusConflict, errs.Busy)
	h.f.mu.Lock()
	n = len(h.f.created)
	h.f.mu.Unlock()
	if n != 2 {
		t.Fatalf("a refused batch created %d", n-2)
	}
}

// TestCreateBatchCancelQueued: a queued job can be cancelled; the ones after it still run.
func TestCreateBatchCancelQueued(t *testing.T) {
	h := newHarness(t, nil)
	gate := make(chan struct{})
	h.f.mu.Lock()
	h.f.createGate = gate
	h.f.mu.Unlock()
	var resp struct {
		Jobs []jobView `json:"jobs"`
	}
	expect(t, h.do("POST", "/api/instances", map[string]any{"count": 3}), http.StatusAccepted, &resp)
	h.waitJob(resp.Jobs[1].ID, func(v jobView) bool { return v.Progress != nil && v.Progress.Step == "queued" })
	expect(t, h.do("POST", "/api/jobs/"+resp.Jobs[1].ID+"/cancel", "{}"), http.StatusOK, nil)
	h.waitJob(resp.Jobs[1].ID, stateIs(stateCanceled))
	close(gate)
	h.waitJob(resp.Jobs[0].ID, stateIs(stateDone))
	h.waitJob(resp.Jobs[2].ID, stateIs(stateDone))
	h.f.mu.Lock()
	c := h.f.created
	h.f.mu.Unlock()
	if len(c) != 2 || *c[0].Index != 2 || *c[1].Index != 7 || c[0].Title != "" {
		t.Fatalf("creates %+v", c)
	}
}
