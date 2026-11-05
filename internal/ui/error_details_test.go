//go:build windows

package ui

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

type errorDetailsBackend struct {
	backend
	failure error
}

func (b errorDetailsBackend) Start(context.Context, int, emu.StartOptions) (*emu.StartResult, error) {
	return nil, b.failure
}

func (b errorDetailsBackend) Create(context.Context, emu.CreateRequest) (*emu.InstanceInfo, error) {
	return nil, b.failure
}

func TestLifecycleJobErrorDetails(t *testing.T) {
	want := map[string]string{errs.BootDiagDir: "saved-guest-state", errs.InstanceExists: "true", errs.InstanceStatus: "create_failed", errs.QemuRunning: "false"}
	inner := errs.WithDetail(errs.New(errs.Timeout, "boot timeout"), errs.BootDiagDir, "saved-guest-state")
	failure := errs.WithDetails(errs.Wrap(errs.Timeout, inner, "create failed").WithAdvice("resume or delete"), map[string]string{
		errs.InstanceExists: "true", errs.InstanceStatus: "create_failed", errs.QemuRunning: "false",
	})
	h := newHarness(t, func(opt *Options) {
		newBackend := opt.newBackend
		opt.newBackend = func(log *logx.Logger) backend { return errorDetailsBackend{backend: newBackend(log), failure: failure} }
	})
	for _, path := range []string{"/api/instances/3/start", "/api/instances"} {
		var job jobView
		body := `{}`
		if path == "/api/instances" {
			body = `{"index":3}`
		}
		expect(t, h.do("POST", path, body), http.StatusAccepted, &job)
		failed := h.waitJob(job.ID, stateIs(stateFailed))
		if failed.Error == nil || failed.Error.Code != "timeout" || failed.Error.Message != "create failed" || failed.Error.Advice != "resume or delete" || !reflect.DeepEqual(failed.Error.Details, want) {
			t.Fatalf("%s: %+v", path, failed)
		}
	}
}
