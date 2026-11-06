//go:build windows

package ui

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"modavd/internal/components"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

type fakeDiag struct {
	mu          sync.Mutex
	fetchCalls  int
	fetchStart  chan struct{}
	fetchReturn chan struct{}
	canceled    chan struct{}
	zipPath     string // what Collect returns (C:\tmp\diag.zip when empty)
}

// the license fakeDiag shows, and the license_sha256 a fetch after reading it carries
const fakeTerms = "terms"

var fakeTermsSHA = components.LicenseSHA256(fakeTerms)

// fetchBody is a POST /api/components/fetch body that agreed to fakeTerms.
func fetchBody(extra string) string {
	return `{"accept_license":true,"license_sha256":"` + fakeTermsSHA + `"` + extra + `}`
}

func (f *fakeDiag) Components(context.Context) (*DiagComponentsResult, error) {
	return &DiagComponentsResult{Root: fakeRoot.Root, Components: []DiagComponent{{ID: "adb", Name: "adb", Installed: false}}}, nil
}
func (f *fakeDiag) License(context.Context, components.SourceSettings) (*DiagLicenseResult, error) {
	return &DiagLicenseResult{Text: fakeTerms, Source: "mirror", SHA256: fakeTermsSHA}, nil
}
func (f *fakeDiag) Fetch(ctx context.Context, accept, _ bool, _ components.SourceSettings, _ string, progress func(DiagProgress)) (*DiagFetchResult, error) {
	if !accept {
		return nil, errs.New(errs.Usage, "必须同意")
	}
	f.mu.Lock()
	f.fetchCalls++
	f.mu.Unlock()
	if f.fetchStart != nil {
		select {
		case <-f.fetchStart:
		default:
			close(f.fetchStart)
		}
	}
	progress(DiagProgress{Stage: "download", Done: 1, Total: 10})
	select {
	case <-f.fetchReturn:
	case <-ctx.Done():
		if f.canceled != nil {
			close(f.canceled)
		}
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
	progress(DiagProgress{Stage: "done", Done: 10, Total: 10})
	return &DiagFetchResult{Root: fakeRoot.Root}, nil
}
func (f *fakeDiag) Collect(context.Context, DiagCollectOptions, func(DiagProgress)) (*DiagCollectResult, error) {
	zip := f.zipPath
	if zip == "" {
		zip = `C:\tmp\diag.zip`
	}
	return &DiagCollectResult{ZipPath: zip, Sections: []DiagSectionResult{{Name: "目录"}, {Name: "日志", Error: "缺少"}}, Errors: []string{"日志：缺少"}}, nil
}
func (f *fakeDiag) ReportConfig(context.Context) (string, error) { return "config", nil }

func diagHarness(t *testing.T, d *fakeDiag, mod func(*Options)) *harness {
	t.Helper()
	return newHarness(t, func(o *Options) {
		o.newDiagBackend = func(*logx.Logger) diagBackend { return d }
		if mod != nil {
			mod(o)
		}
	})
}

func TestDiagnosticsEndpointsAndGuards(t *testing.T) {
	d := &fakeDiag{fetchStart: make(chan struct{}), fetchReturn: make(chan struct{}), canceled: make(chan struct{})}
	h := diagHarness(t, d, nil)
	expectError(t, h.do("GET", "/api/components", nil, noCookie), http.StatusUnauthorized, "unauthorized")
	expectError(t, h.do("POST", "/api/components/fetch", `{"accept_license":false,"license_sha256":"`+fakeTermsSHA+`"}`), http.StatusBadRequest, errs.Usage)
	// agreeing without the hash of the text that was read, or with something that is not one, is not consent either
	expectError(t, h.do("POST", "/api/components/fetch", `{"accept_license":true}`), http.StatusBadRequest, errs.Usage)
	expectError(t, h.do("POST", "/api/components/fetch", `{"accept_license":true,"license_sha256":"abc"}`), http.StatusBadRequest, errs.Usage)
	d.mu.Lock()
	if d.fetchCalls != 0 {
		t.Fatalf("fetch called for rejected license")
	}
	d.mu.Unlock()
	var j jobView
	expect(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusAccepted, &j)
	if j.Kind != kindComponents || j.Index != rootJobIndex {
		t.Fatalf("root job %+v", j)
	}
	<-d.fetchStart
	expectError(t, h.do("POST", "/api/instances/0/start", `{}`), http.StatusConflict, errs.Busy)
	expectError(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusConflict, errs.Busy)
	expect(t, h.do("POST", "/api/jobs/"+j.ID+"/cancel", `{}`), http.StatusOK, nil)
	select {
	case <-d.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not reach diagnostic backend")
	}
	h.waitJob(j.ID, stateIs(stateCanceled))
}

func TestDiagnosticsCollectSectionsAndPlaceholder(t *testing.T) {
	d := &fakeDiag{}
	h := diagHarness(t, d, nil)
	var j jobView
	expect(t, h.do("POST", "/api/diag/collect", `{"keep_stage":true}`), http.StatusAccepted, &j)
	v := h.waitJob(j.ID, stateIs(stateDone))
	res, ok := v.Result.(map[string]any)
	if !ok || len(res["sections"].([]any)) != 2 {
		t.Fatalf("collect result %+v", v.Result)
	}
	h2 := diagHarness(t, &fakeDiag{}, nil)
	var cfg struct {
		Text string `json:"text"`
	}
	expect(t, h2.do("GET", "/api/diag/report-config", nil), http.StatusOK, &cfg)
	if cfg.Text != "config" {
		t.Fatalf("report config %q", cfg.Text)
	}
}

func TestDiagnosticsProgressThrottled(t *testing.T) {
	d := &fakeDiag{fetchStart: make(chan struct{}), fetchReturn: make(chan struct{})}
	h := diagHarness(t, d, nil)
	stream := h.events()
	var j jobView
	expect(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusAccepted, &j)
	<-d.fetchStart
	first := waitFor[jobView](stream, "job", func(v jobView) bool { return v.ID == j.ID && v.Progress != nil })
	// A second callback immediately after the first is dropped by the 500 ms limiter.
	if first.Progress == nil {
		t.Fatal("missing first progress")
	}
	close(d.fetchReturn)
	h.waitJob(j.ID, stateIs(stateDone))
}

func TestComponentsRejectedWhileStarting(t *testing.T) {
	d := &fakeDiag{fetchStart: make(chan struct{}), fetchReturn: make(chan struct{})}
	h := diagHarness(t, d, nil)
	gate := make(chan struct{})
	h.f.mu.Lock()
	h.f.startGate = gate
	h.f.mu.Unlock()
	var sj jobView
	expect(t, h.do("POST", "/api/instances/3/start", `{}`), http.StatusAccepted, &sj)
	deadline := time.Now().Add(time.Second)
	for {
		h.f.mu.Lock()
		started := len(h.f.starts) > 0
		h.f.mu.Unlock()
		if started || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	expectError(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusConflict, errs.Busy)
	close(gate)
	h.waitJob(sj.ID, stateIs(stateDone))
}
