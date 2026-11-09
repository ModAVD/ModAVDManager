//go:build windows

package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/update"
)

// handoffHarness: a root in a temp dir, and recording fakes for the three hand-off steps.
func handoffHarness(t *testing.T) (*harness, *root.Root, *[]string) {
	t.Helper()
	r := &root.Root{Dir: t.TempDir()}
	h := newHarness()
	h.resolve = func(string) (*root.Root, error) { return r, nil }
	var calls []string
	h.waitExit = func(pid int, timeout time.Duration) bool {
		calls = append(calls, "wait "+itoa(pid)+" "+timeout.String())
		return true
	}
	h.updateInstall = func(ctx context.Context, u *update.Updater) (*update.Status, error) {
		calls = append(calls, "install")
		return &update.Status{Updated: true, CurrentVersion: "v0.40.0", Message: "已更新到 0.40.0", Notices: []string{}}, nil
	}
	h.reopen = func(exe string, args []string) error {
		calls = append(calls, "reopen "+exe+" "+strings.Join(args, " "))
		return nil
	}
	return h, r, &calls
}

func TestUpdateHandoffOptionsOnlyWithInstall(t *testing.T) {
	for _, args := range [][]string{
		{"update", "check", "--after-pid", "5"},
		{"update", "--reopen", `C:\x\ModAVD.exe`},
		{"update", "prepare", "--reopen-arg", "--no-window"},
		{"update", "install", "--reopen-arg", "--no-window"},
		{"update", "install", "--reopen", `ModAVD.exe`},
		{"update", "install", "--after-pid", "-1"},
	} {
		h, _, calls := handoffHarness(t)
		res := h.run(t, nil, append(args, "--json")...)
		if res.code != 2 || len(*calls) != 0 {
			t.Errorf("%v: exit %d, calls %v, stderr %q", args, res.code, *calls, res.stderr)
		}
	}
}

func TestUpdateHandoffWaitsInstallsThenReopens(t *testing.T) {
	h, r, calls := handoffHarness(t)
	exe := `C:\ModAVD\ModAVD.exe`
	res := h.run(t, nil, "update", "install", "--after-pid", "4242", "--reopen", exe,
		"--reopen-arg", "--root", "--reopen-arg", `C:\ModAVD`, "--reopen-arg", "--no-window", "--json")
	if res.code != 0 {
		t.Fatalf("exit %d: %s %s", res.code, res.stdout, res.stderr)
	}
	want := []string{"wait 4242 " + update.AfterPIDWait.String(), "install", "reopen " + exe + ` --root C:\ModAVD --no-window`}
	if !slices.Equal(*calls, want) {
		t.Fatalf("calls %v, want %v", *calls, want)
	}
	if d := envelope(t, res.stdout)["data"].(map[string]any); d["updated"] != true || d["current_version"] != "v0.40.0" {
		t.Errorf("data %v", d)
	}
	got := update.TakeHandoff(r, time.Now())
	if got == nil || !got.OK || !got.Updated || got.Version != "v0.40.0" || got.Message != "已更新到 0.40.0" || got.Error != nil {
		t.Fatalf("handoff %+v", got)
	}
	if update.TakeHandoff(r, time.Now()) != nil {
		t.Error("the result was not removed when taken")
	}
}

// The manager does not exit: nothing is installed, the result says so and the manager is still started (a second
// launch only brings up the running one's window), so the user is never left without one.
func TestUpdateHandoffManagerStillRunning(t *testing.T) {
	h, r, calls := handoffHarness(t)
	h.waitExit = func(pid int, timeout time.Duration) bool {
		*calls = append(*calls, "wait")
		return false
	}
	res := h.run(t, nil, "update", "install", "--after-pid", "4242", "--reopen", `C:\ModAVD\ModAVD.exe`, "--json")
	if res.code != errs.ExitCode(errs.New(errs.Busy, "")) {
		t.Fatalf("exit %d: %s", res.code, res.stdout)
	}
	if !slices.Equal(*calls, []string{"wait", `reopen C:\ModAVD\ModAVD.exe `}) {
		t.Fatalf("calls %v", *calls)
	}
	got := update.TakeHandoff(r, time.Now())
	if got == nil || got.OK || got.Error == nil || got.Error.Code != errs.Busy || !strings.Contains(got.Message, "4242") {
		t.Fatalf("handoff %+v", got)
	}
}

func TestUpdateHandoffInstallFailsStillReopens(t *testing.T) {
	h, r, calls := handoffHarness(t)
	h.updateInstall = func(ctx context.Context, u *update.Updater) (*update.Status, error) {
		*calls = append(*calls, "install")
		return &update.Status{CurrentVersion: "v0.39.0", Notices: []string{}},
			errs.New(errs.Busy, "0.40.0 已下载并核对好，但现在不能换上").WithAdvice("关掉实例再装")
	}
	res := h.run(t, nil, "update", "install", "--after-pid", "7", "--reopen", `C:\ModAVD\ModAVD.exe`, "--json")
	if res.code != 5 {
		t.Fatalf("exit %d", res.code)
	}
	if len(*calls) != 3 || (*calls)[2] != `reopen C:\ModAVD\ModAVD.exe ` {
		t.Fatalf("calls %v", *calls)
	}
	got := update.TakeHandoff(r, time.Now())
	if got == nil || got.OK || got.Updated || got.Version != "v0.39.0" || got.Error.Advice != "关掉实例再装" ||
		got.Message != "0.40.0 已下载并核对好，但现在不能换上" {
		t.Fatalf("handoff %+v", got)
	}
}

// Without --reopen nothing is left for a manager and nothing is started; plain install neither waits.
func TestUpdateInstallWithoutReopen(t *testing.T) {
	h, r, calls := handoffHarness(t)
	if res := h.run(t, nil, "update", "install", "--after-pid", "9", "--json"); res.code != 0 {
		t.Fatalf("exit %d", res.code)
	}
	if !slices.Equal(*calls, []string{"wait 9 " + update.AfterPIDWait.String(), "install"}) {
		t.Fatalf("calls %v", *calls)
	}
	*calls = nil
	if res := h.run(t, nil, "update", "install", "--json"); res.code != 0 {
		t.Fatalf("exit %d", res.code)
	}
	if !slices.Equal(*calls, []string{"install"}) {
		t.Fatalf("calls %v", *calls)
	}
	if update.TakeHandoff(r, time.Now()) != nil {
		t.Error("a result was left without --reopen")
	}
}
