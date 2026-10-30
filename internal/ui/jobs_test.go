//go:build windows

package ui

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

func listHas(i int, st emu.Status) func(emu.ListResult) bool {
	return func(l emu.ListResult) bool {
		for _, in := range l.Instances {
			if in.Index == i {
				return in.Status == st
			}
		}
		return false
	}
}

// startResultJob is a Job with its result decoded as StartResult.
type startResultJob struct {
	jobView
	Result *emu.StartResult `json:"result"`
}

func TestStartJobWithEvents(t *testing.T) {
	h := newHarness(t, nil)
	gate := make(chan struct{})
	h.f.startGate = gate

	es := h.events()
	waitFor(es, "instances", listHas(3, emu.StatusOffline))

	var j jobView
	expect(t, h.do("POST", "/api/instances/3/start", `{"window":true}`), http.StatusAccepted, &j)
	if j.ID == "" || j.Kind != kindStart || j.Index != 3 || j.State != stateRunning || j.CreatedAt == "" ||
		j.FinishedAt != "" || j.Result != nil || j.Error != nil || j.Warnings == nil {
		t.Fatalf("new job %+v", j)
	}
	if _, err := time.Parse(time.RFC3339, j.CreatedAt); err != nil {
		t.Fatalf("created_at %q: %v", j.CreatedAt, err)
	}

	// emu's progress and warnings reach the job, its messages the log events, and the step a fresh list (in the
	// order the fake emits them: progress, then the warning as a log line, then the job carrying it)
	waitFor(es, "job", func(v jobView) bool {
		return v.ID == j.ID && v.Progress != nil && v.Progress.Step == "launch" && v.Progress.Done == 50 &&
			v.Progress.Total == 1000
	})
	waitFor(es, "log", func(l logEvent) bool {
		return l.JobID == j.ID && l.Level == "warn" && l.Msg == "测试警告 3" && l.Time != ""
	})
	waitFor(es, "job", func(v jobView) bool { return v.ID == j.ID && slices.Contains(v.Warnings, "测试警告 3") })
	waitFor(es, "instances", listHas(3, emu.StatusStarting))

	close(gate)
	done := waitFor(es, "job", func(v startResultJob) bool { return v.ID == j.ID && v.State == stateDone })
	if done.Result == nil || done.Result.Instance.Status != emu.StatusOnline || !done.Result.Launched ||
		done.Error != nil || done.FinishedAt == "" || done.Progress == nil || done.Progress.Step != "boot" {
		t.Fatalf("finished job %+v (result %+v)", done.jobView, done.Result)
	}
	waitFor(es, "instances", listHas(3, emu.StatusOnline))

	var got startResultJob
	expect(t, h.do("GET", "/api/jobs/"+j.ID, nil), http.StatusOK, &got)
	if got.State != stateDone || got.Result == nil || got.Result.Instance.Index != 3 {
		t.Fatalf("GET job %+v", got)
	}
	var all struct {
		Jobs []jobView `json:"jobs"`
	}
	expect(t, h.do("GET", "/api/jobs", nil), http.StatusOK, &all)
	if len(all.Jobs) != 1 || all.Jobs[0].ID != j.ID {
		t.Fatalf("jobs %+v", all.Jobs)
	}
	h.f.mu.Lock()
	window := h.f.starts[0].Window
	h.f.mu.Unlock()
	if !window {
		t.Fatal("window option not passed to Start")
	}
	expectError(t, h.do("GET", "/api/jobs/j999", nil), 404, errs.NotFound)
}

func TestPingAndPollOnlyWithClients(t *testing.T) {
	h := newHarness(t, nil)
	h.s.pingEvery = 50 * time.Millisecond

	time.Sleep(200 * time.Millisecond) // four poll periods without a client
	if n := h.f.listCalls.Load(); n != 0 {
		t.Fatalf("listed %d times without any event stream", n)
	}
	es := h.events()
	waitFor[emu.ListResult](es, "instances", nil)
	waitFor[struct{}](es, "ping", nil)
	waitFor[emu.ListResult](es, "instances", nil) // and again on the poll interval
	es.close()
	time.Sleep(150 * time.Millisecond) // let the server notice the disconnect
	before := h.f.listCalls.Load()
	time.Sleep(250 * time.Millisecond)
	if after := h.f.listCalls.Load(); after != before {
		t.Fatalf("still polling after the last stream left: %d -> %d", before, after)
	}
}

func TestBusyAndStopWhileStart(t *testing.T) {
	h := newHarness(t, nil)
	gate := make(chan struct{})
	h.f.startGate = gate

	var start jobView
	expect(t, h.do("POST", "/api/instances/3/start", nil), http.StatusAccepted, &start)

	e := expectError(t, h.do("POST", "/api/instances/3/start", "{}"), http.StatusConflict, errs.Busy)
	t.Logf("busy message: %s", e.Message)
	expectError(t, h.do("POST", "/api/instances/3/restart", "{}"), http.StatusConflict, errs.Busy)
	expectError(t, h.do("POST", "/api/instances/3/install", nil, withHeader("Content-Type", "multipart/form-data; boundary=x")),
		http.StatusConflict, errs.Busy)
	expectError(t, h.do("DELETE", "/api/instances/3?confirm=1", nil), http.StatusConflict, errs.Busy)
	// another instance is not affected
	expect(t, h.do("POST", "/api/instances/4/start", nil), http.StatusAccepted, nil)

	// the exception: a stop while booting; the start job is cancelled (only its waiting stops) and the stop runs
	stopGate := make(chan struct{})
	h.f.mu.Lock()
	h.f.stopGate = stopGate
	h.f.mu.Unlock()
	var stop jobView
	expect(t, h.do("POST", "/api/instances/3/stop", `{"force":true}`), http.StatusAccepted, &stop)
	canceled := h.waitJob(start.ID, stateIs(stateCanceled))
	if canceled.Error == nil || canceled.Error.Code != errs.Canceled || canceled.FinishedAt == "" {
		t.Fatalf("cancelled start %+v", canceled)
	}
	// while the stop runs, nothing else starts on that instance, a second stop included
	expectError(t, h.do("POST", "/api/instances/3/stop", nil), http.StatusConflict, errs.Busy)
	expectError(t, h.do("POST", "/api/instances/3/start", nil), http.StatusConflict, errs.Busy)
	close(stopGate)
	stopped := h.waitJob(stop.ID, stateIs(stateDone))
	if stopped.Progress == nil || stopped.Progress.Step != "done" {
		t.Fatalf("stop job %+v", stopped)
	}
	h.f.mu.Lock()
	force := h.f.stops[0].Force
	h.f.mu.Unlock()
	if !force {
		t.Fatal("force option not passed to Stop")
	}
	close(gate)
}

func TestCancelJob(t *testing.T) {
	h := newHarness(t, nil)
	h.f.startGate = make(chan struct{})
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/start", nil), http.StatusAccepted, &j)
	var c jobView
	expect(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", nil), http.StatusOK, &c)
	if c.ID != j.ID {
		t.Fatalf("cancel answered %+v", c)
	}
	got := h.waitJob(j.ID, stateIs(stateCanceled))
	if got.Error == nil || got.Error.Code != errs.Canceled || got.Result != nil {
		t.Fatalf("cancelled job %+v", got)
	}
	// cancelling a finished job changes nothing
	expect(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", nil), http.StatusOK, &c)
	if c.State != stateCanceled {
		t.Fatalf("second cancel %+v", c)
	}
	expectError(t, h.do("POST", "/api/jobs/j77/cancel", nil), 404, errs.NotFound)
	expectError(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", nil, withHeader("Origin", "http://evil.example")),
		http.StatusForbidden, "forbidden")
}

func TestRestartAndFailedJob(t *testing.T) {
	h := newHarness(t, nil)
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/restart", `{"window":true,"force":true}`), http.StatusAccepted, &j)
	got := h.waitJob(j.ID, stateIs(stateDone))
	if got.Kind != kindRestart {
		t.Fatalf("restart job %+v", got)
	}
	h.f.mu.Lock()
	stops, starts := len(h.f.stops), len(h.f.starts)
	h.f.mu.Unlock()
	if stops != 1 || starts != 1 {
		t.Fatalf("restart ran %d stops, %d starts", stops, starts)
	}

	// emu's error becomes the job's error
	expect(t, h.do("POST", "/api/instances/8/start", nil), http.StatusAccepted, &j)
	failed := h.waitJob(j.ID, stateIs(stateFailed))
	if failed.Error == nil || failed.Error.Code != errs.NotFound || failed.Result != nil {
		t.Fatalf("failed job %+v", failed)
	}
}

func TestJobsKeepLast50(t *testing.T) {
	h := newHarness(t, nil)
	var last jobView
	for k := 0; k < 60; k++ {
		expect(t, h.do("POST", "/api/instances/4/stop", nil), http.StatusAccepted, &last)
		h.waitJob(last.ID, stateIs(stateDone))
	}
	var all struct {
		Jobs []jobView `json:"jobs"`
	}
	expect(t, h.do("GET", "/api/jobs", nil), http.StatusOK, &all)
	if len(all.Jobs) != maxJobs || all.Jobs[0].ID != last.ID || all.Jobs[maxJobs-1].ID != fmt.Sprintf("j%d", 60-maxJobs+1) {
		t.Fatalf("%d jobs, first %s, last %s", len(all.Jobs), all.Jobs[0].ID, all.Jobs[len(all.Jobs)-1].ID)
	}
}

func TestLifetime(t *testing.T) {
	h := newHarness(t, nil)
	s := h.s
	s.exitEvery = 10 * time.Millisecond
	s.idleExit = 300 * time.Millisecond
	s.closeGrace = 50 * time.Millisecond

	returned := func(w *Window) chan struct{} {
		done := make(chan struct{})
		go func() {
			s.waitForExit(t.Context(), w)
			close(done)
		}()
		return done
	}
	within := func(ch chan struct{}, d time.Duration) bool {
		select {
		case <-ch:
			return true
		case <-time.After(d):
			return false
		}
	}

	// no stream ever connected: the idle rule ends it
	s.hub.mu.Lock()
	s.hub.changed = time.Now()
	s.hub.mu.Unlock()
	if !within(returned(nil), 2*time.Second) {
		t.Fatal("did not exit after the idle time")
	}

	// a connected stream keeps it; a running job keeps it after the stream left
	h.f.startGate = make(chan struct{})
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/start", nil), http.StatusAccepted, &j)
	es := h.events()
	done := returned(nil)
	if within(done, 600*time.Millisecond) {
		t.Fatal("exited with a stream connected")
	}
	es.close()
	if within(done, 800*time.Millisecond) {
		t.Fatal("exited with a job running")
	}
	close(h.f.startGate)
	if !within(done, 2*time.Second) {
		t.Fatal("did not exit after the job finished")
	}

	// the Edge window closing ends it after the short grace
	s.idleExit = time.Hour
	closed := make(chan struct{})
	close(closed)
	if !within(returned(&Window{Edge: true, Started: time.Now().Add(-time.Minute), Done: closed}), 2*time.Second) {
		t.Fatal("did not exit after the window closed")
	}
	// an Edge that exits at once handed its window over: not a close
	if within(returned(&Window{Edge: true, Started: time.Now(), Done: closed}), 500*time.Millisecond) {
		t.Fatal("took an Edge hand-off for a closed window")
	}
}
