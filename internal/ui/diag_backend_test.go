package ui

import (
	"context"
	"testing"

	"modavd/internal/components"
	"modavd/internal/logx"
	"modavd/internal/root"
)

func TestRealDiagBackendRejectsDeclinedLicense(t *testing.T) {
	old := diagFetch
	t.Cleanup(func() { diagFetch = old })
	calls := 0
	diagFetch = func(context.Context, *root.Root, components.Options) (*components.Result, error) {
		calls++
		return &components.Result{}, nil
	}
	b := newRealDiagBackend(&root.Root{Dir: `C:\root`}, logx.Nop())
	if _, err := b.Fetch(context.Background(), false, components.SourceSettings{}, "", nil); err == nil || calls != 0 {
		t.Fatalf("declined license: err=%v calls=%d", err, calls)
	}
}
func TestRealDiagBackendPassesSourceAndLicenseHash(t *testing.T) {
	old := diagFetch
	t.Cleanup(func() { diagFetch = old })
	var got components.Options
	diagFetch = func(_ context.Context, r *root.Root, o components.Options) (*components.Result, error) {
		got = o
		return &components.Result{Root: r.Dir}, nil
	}
	b := newRealDiagBackend(&root.Root{Dir: `C:\root`}, logx.Nop())
	if _, err := b.Fetch(context.Background(), true, components.SourceSettings{Mirror: "google", Set: true}, "abc", nil); err != nil {
		t.Fatal(err)
	}
	if got.Mirror != "google" || got.LicenseSHA256 != "abc" || got.SourceOrigin != "管理器" || !got.AcceptLicense {
		t.Fatalf("options=%+v", got)
	}
}
