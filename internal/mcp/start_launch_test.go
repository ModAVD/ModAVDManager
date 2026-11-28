//go:build windows

package mcp

import (
	"context"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

// start_instance's launch reaches emu as a checked AppTarget; the result's app comes back through the output schema;
// a bad name is refused before anything starts.
func TestStartInstanceLaunch(t *testing.T) {
	app := &emu.AppLaunch{Package: "com.example.missing", Code: errs.AppNotInstalled, Message: "m", Advice: "a"}
	f := &fake{start: func(_ context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		r := &emu.StartResult{Launched: true, Warnings: []string{}}
		if opt.Launch != nil {
			r.App = app
		}
		return r, nil
	}}
	h := newHarness(t, f, nil)
	tools := map[string]map[string]any{}
	for _, tool := range h.toolsList(true) {
		tools[tool["name"].(string)] = tool
	}
	tool := tools["start_instance"]
	for n, launch := range []string{"com.example.missing", "com.example.app/.Main"} {
		args := map[string]any{"index": 1, "launch": launch}
		if err := validate(jsonRoundTrip(t, tool["inputSchema"]), jsonRoundTrip(t, args), "args"); err != nil {
			t.Fatalf("%s rejected by the input schema: %v", launch, err)
		}
		text, isErr, sc := toolText(t, h.call(n, "start_instance", args, nil))
		if isErr {
			t.Fatalf("%s: %s", launch, text)
		}
		if err := validate(tool["outputSchema"], sc, "result"); err != nil {
			t.Fatalf("%s: result does not match the output schema: %v\n%s", launch, err, text)
		}
		a, _ := sc["app"].(map[string]any)
		if a["code"] != "app_not_installed" || a["ok"] != false || a["package"] != "com.example.missing" {
			t.Errorf("app in the result: %v", sc["app"])
		}
	}
	if len(f.starts) != 2 || f.starts[0].Launch == nil || *f.starts[0].Launch != (emu.AppTarget{Package: "com.example.missing"}) ||
		*f.starts[1].Launch != (emu.AppTarget{Package: "com.example.app", Activity: ".Main"}) {
		t.Fatalf("start options %+v", f.starts)
	}
	// without launch: nothing asked for, no app in the result
	_, _, sc := toolText(t, h.call(5, "start_instance", map[string]any{"index": 1}, nil))
	if _, has := sc["app"]; has || f.starts[2].Launch != nil {
		t.Errorf("app without launch: %v %+v", sc["app"], f.starts[2])
	}

	for n, c := range []struct {
		launch any
		code   string
	}{
		{"com.x;reboot", errs.InvalidArgument}, {"app", errs.InvalidArgument}, {"com.x/.A$B", errs.InvalidArgument},
		{"$(id).x", errs.InvalidArgument}, {"", errs.InvalidArgument}, {7, errs.Usage},
	} {
		text, isErr, _ := toolText(t, h.call(10+n, "start_instance", map[string]any{"index": 1, "launch": c.launch}, nil))
		if !isErr || len(f.starts) != 3 {
			t.Errorf("%v: isError %v, %d starts: %s", c.launch, isErr, len(f.starts), text)
		}
	}
}
