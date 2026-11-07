//go:build windows

package ui

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

// glFake is the switch's side of fakeBackend (its struct is shared with other tests): what was called and what the
// check refuses with.
var glFake struct {
	mu       sync.Mutex
	calls    []string // "check on 3", "run on 3"
	checkErr error
}

func resetGLFake(t *testing.T) {
	t.Helper()
	reset := func() {
		glFake.mu.Lock()
		glFake.calls, glFake.checkErr = nil, nil
		glFake.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func glCalls() []string {
	glFake.mu.Lock()
	defer glFake.mu.Unlock()
	return slices.Clone(glFake.calls)
}

func (b fakeBackend) GLES32Check(i int, action string) error {
	glFake.mu.Lock()
	defer glFake.mu.Unlock()
	glFake.calls = append(glFake.calls, "check "+action+" "+strconv.Itoa(i))
	return glFake.checkErr
}

func (b fakeBackend) GLES32(ctx context.Context, i int, action string) (*emu.GLES32Result, error) {
	glFake.mu.Lock()
	glFake.calls = append(glFake.calls, "run "+action+" "+strconv.Itoa(i))
	glFake.mu.Unlock()
	prop := map[string]string{"on": "196610", "off": "196609"}[action]
	return &emu.GLES32Result{Index: i, Action: action, State: action, Enabled: action == "on", Prop: prop, Restarted: true,
		Notes: []string{}, Warnings: []string{}}, nil
}

// TestGLES32InstanceJobs: gles32-on / gles32-off check first, then run as the instance's job (kind, index, result);
// what the check refuses is answered at once with its reason and nothing runs.
func TestGLES32InstanceJobs(t *testing.T) {
	resetGLFake(t)
	h := newHarness(t, nil)
	for _, action := range []string{"on", "off"} {
		var j jobView
		expect(t, h.do("POST", "/api/instances/3/gles32-"+action, nil), http.StatusAccepted, &j)
		if j.Kind != "gles32-"+action || j.Index != 3 {
			t.Fatalf("job %+v", j)
		}
		done := h.waitJob(j.ID, stateIs(stateDone))
		res, _ := done.Result.(map[string]any)
		if res["action"] != action || res["state"] != action || res["restarted"] != true {
			t.Errorf("result %v", done.Result)
		}
	}
	if want := []string{"check on 3", "run on 3", "check off 3", "run off 3"}; !slices.Equal(glCalls(), want) {
		t.Errorf("calls %v, want %v", glCalls(), want)
	}

	resetGLFake(t)
	glFake.mu.Lock()
	glFake.checkErr = errs.WithDetail(errs.New(errs.NotReady, "没有执行桥").WithAdvice("先装执行桥"), errs.Reason, emu.GLES32ReasonNoBridge)
	glFake.mu.Unlock()
	e := expectError(t, h.do("POST", "/api/instances/3/gles32-on", nil), http.StatusUnprocessableEntity, errs.NotReady)
	if e.Details[errs.Reason] != emu.GLES32ReasonNoBridge || e.Advice != "先装执行桥" {
		t.Errorf("refused: %+v", e)
	}
	if want := []string{"check on 3"}; !slices.Equal(glCalls(), want) {
		t.Errorf("calls %v", glCalls())
	}
	expectError(t, h.do("POST", "/api/instances/x/gles32-on", nil), http.StatusBadRequest, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances/3/gles32-on", `{"force":true}`), http.StatusBadRequest, errs.Usage)
}

// TestGLES32JobLabels: the jobs have their words in the activity list.
func TestGLES32JobLabels(t *testing.T) {
	if kindLabel(kindGLES32On) != "开启报告 GLES 3.2" || kindLabel(kindGLES32Off) != "关闭报告 GLES 3.2" {
		t.Error("labels")
	}
	if !bootJob(kindGLES32On) || !bootJob(kindGLES32Off) {
		t.Error("the switch's jobs boot the instance (they must wait for the sdk jobs)")
	}
}
