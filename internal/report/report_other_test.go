//go:build !windows && (!darwin || !arm64 || !cgo)

package report

import (
	"context"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// Only Windows is implemented: everything else says so with errs.Unsupported (exit code 4), even with a host given.
func TestUnsupportedOffWindows(t *testing.T) {
	r := &root.Root{Dir: testutil.TempDir(t)}
	ctx := context.Background()
	_, err := BuildConfig(ctx, r, ConfigOptions{NoClipboard: true, Host: &fakeHost{sc: &scenario{}}})
	check := func(what string, err error) {
		t.Helper()
		if code := errs.CodeOf(err); code != errs.Unsupported || errs.ExitCode(err) != 4 {
			t.Errorf("%s: code %q (%v), want %q", what, code, err, errs.Unsupported)
		}
	}
	check("BuildConfig", err)
	_, err = Collect(ctx, r, CollectOptions{OutDir: testutil.TempDir(t), StageDir: testutil.TempDir(t), NoExplorer: true})
	check("Collect", err)
	_, err = newHost()
	check("newHost", err)
	_, err = desktopDir()
	check("desktopDir", err)
	check("copyClipboard", copyClipboard("x"))
	check("openExplorer", openExplorer("x"))
}
