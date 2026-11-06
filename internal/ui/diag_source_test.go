//go:build windows

package ui

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"modavd/internal/components"
	"modavd/internal/logx"
)

// sourceRecordingDiag records the source and license hash each call got, and logs through the logger its factory
// was given (one per job: the job's logger).
type sourceRecordingDiag struct {
	fakeDiag
	log      *logx.Logger
	mu       sync.Mutex
	fetched  []components.SourceSettings
	hashes   []string
	licensed []components.SourceSettings
}

type sourceRecorder struct {
	mu   sync.Mutex
	made []*sourceRecordingDiag
}

func (r *sourceRecorder) backend(l *logx.Logger) diagBackend {
	r.mu.Lock()
	defer r.mu.Unlock()
	d := &sourceRecordingDiag{log: l}
	r.made = append(r.made, d)
	return d
}

func (r *sourceRecorder) all() (fetched []components.SourceSettings, hashes []string, licensed []components.SourceSettings) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.made {
		d.mu.Lock()
		fetched, hashes, licensed = append(fetched, d.fetched...), append(hashes, d.hashes...), append(licensed, d.licensed...)
		d.mu.Unlock()
	}
	return
}

func (d *sourceRecordingDiag) License(_ context.Context, s components.SourceSettings) (*DiagLicenseResult, error) {
	d.mu.Lock()
	d.licensed = append(d.licensed, s)
	d.mu.Unlock()
	return &DiagLicenseResult{Text: fakeTerms, Source: "mirror", SHA256: fakeTermsSHA}, nil
}

func (d *sourceRecordingDiag) Fetch(_ context.Context, _ bool, s components.SourceSettings, hash string, _ func(DiagProgress)) (*DiagFetchResult, error) {
	d.mu.Lock()
	d.fetched = append(d.fetched, s)
	d.hashes = append(d.hashes, hash)
	d.mu.Unlock()
	d.log.Debugf("许可协议全文")
	d.log.Infof("下载源：测试")
	return &DiagFetchResult{Root: fakeRoot.Root}, nil
}

// The source picked in the dialog reaches the fetch (and the hash of the license read from it); without one in the
// request the saved source is used.
func TestComponentsFetchUsesRequestOrSavedSource(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	rec := &sourceRecorder{}
	h := newHarness(t, func(o *Options) { o.newDiagBackend = rec.backend })
	var j jobView
	expect(t, h.do("POST", "/api/components/fetch", fetchBody(`,"source":"tencent"`)), http.StatusAccepted, &j)
	h.waitJob(j.ID, stateIs(stateDone))
	expect(t, h.do("PUT", "/api/components/source", `{"source":"custom","mirror_urls":["https://m.example/sdk"]}`), http.StatusOK, nil)
	expect(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusAccepted, &j)
	h.waitJob(j.ID, stateIs(stateDone))
	fetched, hashes, _ := rec.all()
	if len(fetched) != 2 || fetched[0].Mirror != "tencent" || len(fetched[0].MirrorURLs) != 0 ||
		fetched[1].Mirror != "" || len(fetched[1].MirrorURLs) != 1 || fetched[1].MirrorURLs[0] != "https://m.example/sdk/" {
		t.Fatalf("sources reaching the backend: %+v", fetched)
	}
	if len(hashes) != 2 || hashes[0] != fakeTermsSHA || hashes[1] != fakeTermsSHA {
		t.Fatalf("license hashes reaching the backend: %q", hashes)
	}
}

// The dialog's license is read from the source it shows (a changed choice reads it again); the saved one without a
// source in the query; a bad source is refused before going online.
func TestComponentLicenseFollowsSource(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	rec := &sourceRecorder{}
	h := newHarness(t, func(o *Options) { o.newDiagBackend = rec.backend })
	var lic DiagLicenseResult
	expect(t, h.do("GET", "/api/components/license?source=custom&mirror_urls=https://a.example/x&mirror_urls=https://b.example/y/", nil), http.StatusOK, &lic)
	if lic.SHA256 != fakeTermsSHA || lic.Text != fakeTerms {
		t.Fatalf("license %+v", lic)
	}
	expect(t, h.do("GET", "/api/components/license?source=google", nil), http.StatusOK, nil)
	expect(t, h.do("PUT", "/api/components/source", `{"source":"tencent"}`), http.StatusOK, nil)
	expect(t, h.do("GET", "/api/components/license", nil), http.StatusOK, nil)
	expectError(t, h.do("GET", "/api/components/license?source=custom", nil), http.StatusBadRequest, "usage")
	expectError(t, h.do("GET", "/api/components/license?source=aliyun", nil), http.StatusBadRequest, "invalid_argument")
	_, _, licensed := rec.all()
	if len(licensed) != 3 || len(licensed[0].MirrorURLs) != 2 || licensed[0].MirrorURLs[1] != "https://b.example/y/" ||
		licensed[1].Mirror != "google" || licensed[2].Mirror != "tencent" {
		t.Fatalf("license sources: %+v", licensed)
	}
}

// A component fetch logs as its job: messages carry the job id in the event stream, the license text (Debug) stays
// out of it.
func TestComponentsFetchLogsAsItsJob(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	rec := &sourceRecorder{}
	h := newHarness(t, func(o *Options) { o.newDiagBackend = rec.backend })
	stream := h.events()
	var j jobView
	expect(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusAccepted, &j)
	// the Debug line is logged first: had it reached the stream, it would come before the Info one
	waitFor[logEvent](stream, "log", func(l logEvent) bool {
		if l.Msg == "许可协议全文" {
			t.Fatalf("debug line in the event stream: %+v", l)
		}
		return l.Msg == "下载源：测试" && l.JobID == j.ID
	})
	h.waitJob(j.ID, stateIs(stateDone))
}

// 在资源管理器里显示 opens the folder of a package a collect-info job made, named by the job; never a path from the
// page.
func TestOpenCollectInfoFolder(t *testing.T) {
	dir := t.TempDir()
	zip := filepath.Join(dir, "问题信息.zip")
	if err := os.WriteFile(zip, []byte("PK"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := &fakeDiag{zipPath: zip, fetchReturn: make(chan struct{})}
	close(d.fetchReturn)
	h := diagHarness(t, d, nil)
	var cj, fj jobView
	expect(t, h.do("POST", "/api/diag/collect", `{}`), http.StatusAccepted, &cj)
	h.waitJob(cj.ID, stateIs(stateDone))
	expect(t, h.do("POST", "/api/open", `{"what":"collect-info","job":"`+cj.ID+`"}`), http.StatusOK, nil)
	expect(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusAccepted, &fj)
	h.waitJob(fj.ID, stateIs(stateDone))
	expectError(t, h.do("POST", "/api/open", `{"what":"collect-info","job":"`+fj.ID+`"}`), http.StatusNotFound, "not_found")
	expectError(t, h.do("POST", "/api/open", `{"what":"collect-info","job":"j999"}`), http.StatusNotFound, "not_found")
	expectError(t, h.do("POST", "/api/open", `{"what":"collect-info"}`), http.StatusBadRequest, "usage")
	// a path from the page is not taken any more (unknown field), and the what values say collect-info
	expectError(t, h.do("POST", "/api/open", `{"what":"collect-info","job":"`+cj.ID+`","path":"C:\\Windows"}`), http.StatusBadRequest, "usage")
	if e := expectError(t, h.do("POST", "/api/open", `{"what":"path"}`), http.StatusBadRequest, "invalid_argument"); !strings.Contains(e.Message, "collect-info") {
		t.Errorf("what error %q", e.Message)
	}
	if err := os.Remove(zip); err != nil {
		t.Fatal(err)
	}
	expectError(t, h.do("POST", "/api/open", `{"what":"collect-info","job":"`+cj.ID+`"}`), http.StatusNotFound, "not_found")
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.opened) != 1 || h.opened[0] != dir {
		t.Fatalf("opened %q, want only %q", h.opened, dir)
	}
}
