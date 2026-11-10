//go:build !windows

package update

import (
	"context"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

func TestWriteOperationsUnsupportedWithoutWrites(t *testing.T) {
	if nativeDarwinSettings && root.CurrentPlatform().Supported() {
		t.Skip("native macOS supports update transactions")
	}
	dir := testutil.TempDir(t)
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	u := New(r, nil, Options{})
	ctx := context.Background()
	before := updateTree(t, dir)
	for name, call := range map[string]func() error{
		"prepare": func() error { _, err := u.Prepare(ctx); return err },
		"install": func() error { _, err := u.Install(ctx); return err },
		"recover": func() error { _, err := u.Recover(ctx); return err },
	} {
		if got := errs.CodeOf(call()); got != errs.Unsupported {
			t.Errorf("%s: got %s", name, got)
		}
	}
	if after := updateTree(t, dir); !reflect.DeepEqual(after, before) {
		t.Fatalf("root changed: before %v, after %v", before, after)
	}
}

func updateTree(t *testing.T, dir string) []string {
	t.Helper()
	var paths []string
	if err := filepath.WalkDir(dir, func(path string, _ fs.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(paths)
	return paths
}

// --cdk-dpapi is Windows DPAPI: unsupported elsewhere, "" included, before looking at the value.
func TestCallerCDKUnsupported(t *testing.T) {
	for _, in := range []string{"", "AQID", "%%%"} {
		if got, err := CallerCDK(in); errs.CodeOf(err) != errs.Unsupported || got != "" {
			t.Errorf("CallerCDK(%q) = %q %v", in, got, err)
		}
	}
}

func TestSaveSettingsUnsupported(t *testing.T) {
	if nativeDarwinSettings && root.CurrentPlatform().Supported() {
		t.Skip("native macOS supports per-user settings")
	}
	if _, err := SaveSettings(SourceGitHub, nil); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("SaveSettings: %v", err)
	}
}
