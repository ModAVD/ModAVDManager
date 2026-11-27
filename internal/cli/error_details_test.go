//go:build windows

package cli

import (
	"context"
	"reflect"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestLifecycleErrorDetailsOutput(t *testing.T) {
	want := map[string]any{"boot_diag_dir": "saved-guest-state", "instance_exists": "true", "instance_status": "create_failed", "qemu_running": "false"}
	inner := errs.WithDetail(errs.New(errs.Timeout, "boot timeout"), errs.BootDiagDir, "saved-guest-state")
	failure := errs.WithDetails(errs.Wrap(errs.Timeout, inner, "create failed").WithAdvice("resume or delete"), map[string]string{
		errs.InstanceExists: "true", errs.InstanceStatus: "create_failed", errs.QemuRunning: "false",
	})
	for _, command := range []string{"create", "start"} {
		t.Run(command, func(t *testing.T) {
			h := newHarness()
			h.fake.create = func(emu.CreateRequest) (*emu.InstanceInfo, error) { return nil, failure }
			h.fake.start = func(context.Context, int, emu.StartOptions) (*emu.StartResult, error) { return nil, failure }
			args := []string{command, "3", "--json"}
			if command == "create" {
				args = []string{"create", "--index", "3", "--json"}
			}
			r := h.run(t, nil, args...)
			e := envelope(t, r.stdout)["error"].(map[string]any)
			if r.code != 6 || e["code"] != "timeout" || e["message"] != "create failed" || e["advice"] != "resume or delete" || !reflect.DeepEqual(e["details"], want) {
				t.Fatalf("CLI output: %+v, error %v", r, e)
			}
		})
	}
}
