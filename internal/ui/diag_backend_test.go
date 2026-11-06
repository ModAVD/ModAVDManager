package ui

import (
	"context"
	"strings"
	"testing"

	"modavd/internal/components"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// The real adapter refuses a fetch nobody agreed to before calling components.Fetch.
func TestRealDiagBackendRejectsDeclinedLicense(t *testing.T) {
	old := diagFetch
	t.Cleanup(func() { diagFetch = old })
	calls := 0
	diagFetch = func(context.Context, *root.Root, components.Options) (*components.Result, error) {
		calls++
		return &components.Result{}, nil
	}
	b := newRealDiagBackend(&root.Root{Dir: `C:\root`}, logx.Nop())
	if _, err := b.Fetch(context.Background(), false, false, components.SourceSettings{}, fakeHash, nil); err == nil || calls != 0 {
		t.Fatalf("declined license: err=%v calls=%d", err, calls)
	}
}

const fakeHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// It hands components.Fetch the source as given (a built-in mirror, or the custom addresses), the license hash,
// where the source was chosen, and its logger: the license text the dialog already showed only at Debug.
func TestRealDiagBackendPassesSourceAndLicenseHash(t *testing.T) {
	old := diagFetch
	t.Cleanup(func() { diagFetch = old })
	var got []components.Options
	diagFetch = func(_ context.Context, r *root.Root, o components.Options) (*components.Result, error) {
		got = append(got, o)
		o.Notice("许可协议全文")
		o.Log.Infof("下载源：测试")
		return &components.Result{Root: r.Dir}, nil
	}
	var lines []string
	log := logx.New(func(level logx.Level, msg string) { lines = append(lines, level.String()+" "+msg) })
	b := newRealDiagBackend(&root.Root{Dir: `C:\root`}, log)
	for _, s := range []components.SourceSettings{
		{Mirror: "google", Set: true},
		{MirrorURLs: []string{"https://a.example/x/", "https://b.example/y/"}, Set: true},
	} {
		if _, err := b.Fetch(context.Background(), true, false, s, fakeHash, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0].Mirror != "google" || len(got[0].MirrorURLs) != 0 ||
		got[1].Mirror != "" || strings.Join(got[1].MirrorURLs, ",") != "https://a.example/x/,https://b.example/y/" {
		t.Fatalf("sources: %+v", got)
	}
	for _, o := range got {
		if !o.AcceptLicense || o.LicenseSHA256 != fakeHash || o.SourceOrigin != components.SourceFromManager || o.Input != nil {
			t.Fatalf("options %+v", o)
		}
	}
	if want := "debug 许可协议全文|info 下载源：测试"; !strings.HasPrefix(strings.Join(lines, "|"), want) {
		t.Fatalf("log lines %q, want them to start with %q", lines, want)
	}
}

// License answers the text read from the given source with its SHA-256.
func TestRealDiagBackendLicenseHash(t *testing.T) {
	old := diagLicense
	t.Cleanup(func() { diagLicense = old })
	var got components.SourceSettings
	diagLicense = func(_ context.Context, s components.SourceSettings) (string, string, error) {
		got = s
		return "terms", "mirror", nil
	}
	b := newRealDiagBackend(&root.Root{Dir: `C:\root`}, logx.Nop())
	res, err := b.License(context.Background(), components.SourceSettings{Mirror: "tencent", Set: true})
	if err != nil || got.Mirror != "tencent" || res.Text != "terms" || res.SHA256 != components.LicenseSHA256("terms") ||
		len(res.SHA256) != 64 {
		t.Fatalf("license %+v %v, source %+v", res, err, got)
	}
}
