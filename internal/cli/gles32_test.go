//go:build windows

package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

// gles32Err, when set, is what the fake manager's GLES32 returns.
var gles32Err error

func (f *fakeManager) GLES32(ctx context.Context, i int, action string) (*emu.GLES32Result, error) {
	f.record("gles32 " + itoa(i) + " " + action)
	if gles32Err != nil {
		return nil, gles32Err
	}
	state := map[string]string{"on": "on", "off": "off", "status": "pending-on"}[action]
	prop := map[string]string{"on": "196610", "off": "196609", "status": "196609"}[action]
	return &emu.GLES32Result{Index: i, Action: action, State: state, Enabled: state == "on", Prop: prop,
		Notes: []string{}, Warnings: []string{}}, nil
}

// TestGLES32Command: gles32 <i> on|off|status reaches the manager (any case); another action is a usage error; without
// --json the exit code says whether the switch ended up as asked; a refusal keeps its reason.
func TestGLES32Command(t *testing.T) {
	h := newHarness()
	for _, a := range []string{"on", "OFF", "status"} {
		if r := h.run(t, nil, "gles32", "3", a); r.code != 0 {
			t.Errorf("%s: exit %d %s", a, r.code, r.stderr)
		}
	}
	if r := h.run(t, nil, "gles32", "3", "toggle"); r.code != 2 {
		t.Errorf("toggle: exit %d", r.code)
	}
	if r := h.run(t, nil, "gles32", "3"); r.code != 2 {
		t.Errorf("no action: exit %d", r.code)
	}
	if want := []string{"gles32 3 on", "gles32 3 off", "gles32 3 status"}; !slices.Equal(h.fake.calls, want) {
		t.Errorf("calls %v", h.fake.calls)
	}
	if exitGLES32(&emu.GLES32Result{Action: "on", State: "pending-on"}, false) != 1 ||
		exitGLES32(&emu.GLES32Result{Action: "off", State: "on"}, false) != 1 ||
		exitGLES32(&emu.GLES32Result{Action: "status", State: "pending-on"}, false) != 0 ||
		exitGLES32(&emu.GLES32Result{Action: "on", State: "pending-on"}, true) != 0 {
		t.Error("exit codes")
	}

	r := h.run(t, nil, "gles32", "3", "on")
	for _, want := range []string{"实例 3 报告 GLES 3.2 on：开", "ro.opengles.version=196610"} {
		if !strings.Contains(r.stdout, want) {
			t.Errorf("human: want %q in\n%s", want, r.stdout)
		}
	}
	r = h.run(t, nil, "--json", "gles32", "3", "on")
	data, _ := envelope(t, r.stdout)["data"].(map[string]any)
	if r.code != 0 || data["state"] != "on" || data["enabled"] != true || data["prop"] != "196610" {
		t.Errorf("json: exit %d, %s", r.code, r.stdout)
	}

	gles32Err = errs.WithDetail(errs.New(errs.NotReady, "实例 3 没有执行桥"), errs.Reason, emu.GLES32ReasonNoBridge)
	t.Cleanup(func() { gles32Err = nil })
	r = h.run(t, nil, "--json", "gles32", "3", "on")
	e := envelope(t, r.stdout)["error"].(map[string]any)
	if r.code != 4 || e["code"] != "not_ready" || e["details"].(map[string]any)["reason"] != "gles32_no_bridge" {
		t.Errorf("refused: exit %d, %v", r.code, e)
	}
}

// TestGLES32Help: the help says what the switch does and does not.
func TestGLES32Help(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "help", "gles32")
	for _, want := range []string{"实验", "实际渲染仍是 3.1", "执行桥", "冷启动"} {
		if !strings.Contains(r.stdout+r.stderr, want) {
			t.Errorf("help lacks %q:\n%s%s", want, r.stdout, r.stderr)
		}
	}
}

// TestInfoShowsGLES32: info has a line for the switch when it is on, and none when it is off.
func TestInfoShowsGLES32(t *testing.T) {
	h := newHarness()
	h.fake.info = func(i int) (*emu.InstanceInfo, error) {
		in := sampleInstance(i)
		in.GLES32 = i == 4
		return &in, nil
	}
	if r := h.run(t, nil, "info", "4"); r.code != 0 || !strings.Contains(r.stdout, "报告 GLES 3.2：开") {
		t.Errorf("on: exit %d\n%s", r.code, r.stdout)
	}
	if r := h.run(t, nil, "info", "3"); r.code != 0 || strings.Contains(r.stdout, "GLES") {
		t.Errorf("off: exit %d\n%s", r.code, r.stdout)
	}
	r := h.run(t, nil, "--json", "info", "4")
	if data, _ := envelope(t, r.stdout)["data"].(map[string]any); data["gles32"] != true {
		t.Errorf("json: %s", r.stdout)
	}
}
