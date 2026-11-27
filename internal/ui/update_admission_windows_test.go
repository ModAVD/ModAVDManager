// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/update"
)

func TestUpdateExclusiveLeavesAlreadyAdmittedJobsAlone(t *testing.T) {
	for _, kind := range []string{kindStart, kindStop, kindCreate, kindInstall, kindDiskCompact} {
		t.Run(kind, func(t *testing.T) {
			a := &applyFakes{prepared: preparedStatus(), prepGate: make(chan struct{})}
			h := applyHarness(t, a)
			h.f.put(3, emu.StatusOnline)
			gate := make(chan struct{})
			v, err := h.s.startJob(kind, 4, func(ctx context.Context, _ *job, _ backend) (any, error) {
				select {
				case <-gate:
					return nil, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			j := h.apply()
			close(a.prepGate)
			done := h.waitJob(j.ID, stateIs(stateFailed))
			if done.Error == nil || done.Error.Code != errs.Busy || !strings.Contains(done.Error.Message, "一台实例都没关") || !strings.Contains(done.Error.Advice, "不用重新下载") {
				t.Fatalf("update: %+v", done)
			}
			if h.s.listJobs()[1].State != stateRunning || len(a.spawned()) != 0 {
				t.Fatal("update canceled admitted work or handed off")
			}
			h.f.mu.Lock()
			stops := len(h.f.stops)
			h.f.mu.Unlock()
			if stops != 0 {
				t.Fatal("instances were stopped before admission settled")
			}
			close(gate)
			h.waitJob(v.ID, stateIs(stateDone))
			// A failed transition releases admission; the next try can hand off.
			a.mu.Lock()
			a.prepGate = nil
			a.mu.Unlock()
			h.waitJob(h.apply().ID, stateIs(stateDone))
		})
	}
}

func TestUpdateExclusiveIncludesSynchronousMutations(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus(), prepGate: make(chan struct{})}
	h := applyHarness(t, a)
	release, err := h.s.beginInstanceAction(kindSettings, 3)
	if err != nil {
		t.Fatal(err)
	}
	j := h.apply()
	close(a.prepGate)
	done := h.waitJob(j.ID, stateIs(stateFailed))
	if done.Error == nil || !strings.Contains(done.Error.Message, "修改设置") {
		t.Fatalf("action not accounted for: %+v", done)
	}
	release()
	h.f.put(3, emu.StatusOnline)
	h.f.stopGate = make(chan struct{})
	j = h.apply()
	h.waitJob(j.ID, func(v jobView) bool { return v.Progress != nil && v.Progress.Step == "stopping" })
	for _, req := range []struct{ method, path, body string }{
		{"POST", "/api/instances/4/start", `{}`},
		{"POST", "/api/instances", `{"index":6}`},
		{"PATCH", "/api/instances/4", `{"title":"test"}`},
		{"DELETE", "/api/instances/4?confirm=1", ``},
	} {
		expectError(t, h.do(req.method, req.path, req.body), http.StatusConflict, errs.Busy)
	}
	expect(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", nil), http.StatusOK, nil)
	h.waitJob(j.ID, stateIs(stateCanceled))
	if len(a.spawned()) != 0 || h.s.handingOff.Load() {
		t.Fatal("cancelled exclusive update handed off")
	}
	if unlock, err := h.s.beginInstanceAction(kindSettings, 4); err != nil {
		t.Fatal(err)
	} else {
		unlock()
	}
}

func TestUpdateTransitionAndInstanceAdmissionAreAtomic(t *testing.T) {
	h := newHarness(t, nil)
	for range 40 {
		start, finish, work := make(chan struct{}), make(chan struct{}), make(chan struct{})
		transition := make(chan error, 1)
		admit := make(chan error, 1)
		v, err := h.s.startRootJob(kindUpdate, func(ctx context.Context, j *job, _ backend) (any, error) {
			<-start
			err := h.s.enterUpdateExclusive(ctx, j, "0.40.0")
			transition <- err
			<-finish
			return nil, err
		})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			<-start
			_, err := h.s.startJob(kindCreate, 3, func(context.Context, *job, backend) (any, error) { <-work; return nil, nil })
			admit <- err
		}()
		close(start)
		tr, ar := <-transition, <-admit
		if tr == nil && errs.CodeOf(ar) != errs.Busy || ar == nil && errs.CodeOf(tr) != errs.Busy || tr != nil && ar != nil {
			t.Fatalf("non-atomic admission: transition %v, instance %v", tr, ar)
		}
		close(finish)
		close(work)
		h.waitJob(v.ID, func(v jobView) bool { return v.State != stateRunning })
		h.s.jobs.waitIdle(time.Second)
	}
}

func TestUpdateHandoffRequiresOwnershipAndRemainsCancelable(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus()}
	h := applyHarness(t, a)
	h.s.handoffReady = func(update.HandoffID) bool { return false }
	j := h.apply()
	h.waitJob(j.ID, func(v jobView) bool { return v.Progress != nil && v.Progress.Step == "handoff" })
	// Surviving the initial liveness check is insufficient without owning the gate.
	time.Sleep(handoffAliveCheck + 100*time.Millisecond)
	if h.s.handingOff.Load() || h.exited(0) {
		t.Fatal("an unacknowledged CLI made the manager exit")
	}
	expect(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", nil), http.StatusOK, nil)
	h.waitJob(j.ID, stateIs(stateCanceled))
	spawns := a.spawned()
	if len(spawns) != 1 {
		t.Fatalf("spawns %v", spawns)
	}
	id := spawns[0][11]
	if _, err := os.Stat(filepath.Join(h.s.root.Dir, ".update", "manager-results", id+".json.cancel")); err != nil {
		t.Fatal("cancellation did not withdraw this handoff", err)
	}
	if unlock, err := h.s.beginInstanceAction(kindSettings, 4); err != nil {
		t.Fatal(err)
	} else {
		unlock()
	}
}

// An instance started while the new version downloads was never confirmed: nothing is stopped, the stage stays, and
// the next 更新 (which names it) goes through.
func TestUpdateStopsOnlyConfirmedInstances(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus(), prepGate: make(chan struct{})}
	h := applyHarness(t, a)
	h.f.put(3, emu.StatusOnline)
	var j jobView
	expect(t, h.do("POST", "/api/update/apply", `{"confirmed":[3]}`), http.StatusAccepted, &j)
	h.f.put(4, emu.StatusOnline)
	close(a.prepGate)
	done := h.waitJob(j.ID, stateIs(stateFailed))
	if done.Error == nil || done.Error.Code != errs.Busy || !strings.Contains(done.Error.Message, "又开了实例 4") ||
		!strings.Contains(done.Error.Message, "一台实例都没关") || !strings.Contains(done.Error.Advice, "不用重新下载") {
		t.Fatalf("update: %+v", done)
	}
	h.f.mu.Lock()
	stops := len(h.f.stops)
	h.f.mu.Unlock()
	if stops != 0 || len(a.spawned()) != 0 {
		t.Fatalf("unconfirmed instance: %d stops, %d hand-offs", stops, len(a.spawned()))
	}
	a.mu.Lock()
	a.prepGate = nil
	a.mu.Unlock()
	expect(t, h.do("POST", "/api/update/apply", `{"confirmed":[4,3]}`), http.StatusAccepted, &j)
	h.waitJob(j.ID, stateIs(stateDone))
	h.f.mu.Lock()
	stops = len(h.f.stops)
	h.f.mu.Unlock()
	if stops != 2 || len(a.spawned()) != 1 {
		t.Fatalf("confirmed instances: %d stops, %d hand-offs", stops, len(a.spawned()))
	}
}

// Without the list (the API before it existed, other callers) whatever runs is stopped, as before.
func TestUpdateWithoutConfirmedListStopsWhatRuns(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus()}
	h := applyHarness(t, a)
	h.f.put(3, emu.StatusOnline)
	h.waitJob(h.apply().ID, stateIs(stateDone))
	h.f.mu.Lock()
	stops := len(h.f.stops)
	h.f.mu.Unlock()
	if stops != 1 || len(a.spawned()) != 1 {
		t.Fatalf("%d stops, %d hand-offs", stops, len(a.spawned()))
	}
}

// One tap still on its way does not fail the update; new ones are refused once it is exclusive.
func TestUpdateExclusiveIgnoresInputInFlight(t *testing.T) {
	a := &applyFakes{prepared: preparedStatus(), prepGate: make(chan struct{})}
	h := applyHarness(t, a)
	release, err := h.s.beginInstanceAction(kindInput, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	j := h.apply()
	close(a.prepGate)
	h.waitJob(j.ID, stateIs(stateDone))
	if len(a.spawned()) != 1 {
		t.Fatal("an input in flight stopped the hand-off")
	}
	if _, err := h.s.beginInstanceAction(kindInput, 3); errs.CodeOf(err) != errs.Busy {
		t.Fatalf("input admitted during the hand-off: %v", err)
	}
}
