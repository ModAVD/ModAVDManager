//go:build windows

package ui

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"modavd/internal/components"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// psFake is the store's side of fakeBackend and fakeDiag (their structs are shared with other tests): what was
// called, what the check refuses with, and the files each import got (read while the job ran).
var psFake struct {
	mu        sync.Mutex
	calls     []string // "check install 3", "run install 3", "gsfid 3"
	checkErr  error
	gsfidErr  error
	imported  [][]importedFile
	importErr error
	gate      chan struct{} // ImportPlayStore waits here when set
}

type importedFile struct{ name, path, content string }

func resetPSFake(t *testing.T) {
	t.Helper()
	reset := func() {
		psFake.mu.Lock()
		psFake.calls, psFake.checkErr, psFake.gsfidErr, psFake.imported, psFake.importErr, psFake.gate = nil, nil, nil, nil, nil, nil
		psFake.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func psCalls() []string {
	psFake.mu.Lock()
	defer psFake.mu.Unlock()
	return slices.Clone(psFake.calls)
}

func (b fakeBackend) PlayStoreCheck(i int, action string) error {
	psFake.mu.Lock()
	defer psFake.mu.Unlock()
	psFake.calls = append(psFake.calls, "check "+action+" "+strconv.Itoa(i))
	return psFake.checkErr
}

func (b fakeBackend) PlayStore(ctx context.Context, i int, action string) (*emu.PlayStoreResult, error) {
	psFake.mu.Lock()
	psFake.calls = append(psFake.calls, "run "+action+" "+strconv.Itoa(i))
	psFake.mu.Unlock()
	state := map[string]string{"install": "installed", "uninstall": "absent"}[action]
	return &emu.PlayStoreResult{Index: i, Action: action, State: state, Installed: state == "installed", Restarted: true,
		Files: []string{}, Warnings: []string{}}, nil
}

func (b fakeBackend) PlayStoreGSFID(ctx context.Context, i int) (*emu.PlayStoreGSFIDResult, error) {
	psFake.mu.Lock()
	defer psFake.mu.Unlock()
	psFake.calls = append(psFake.calls, "gsfid "+strconv.Itoa(i))
	if psFake.gsfidErr != nil {
		return nil, psFake.gsfidErr
	}
	return &emu.PlayStoreGSFIDResult{Index: i, Decimal: "3881793548573364239", Hex: "35dee6aa721e500f"}, nil
}

// TestPlayStoreGSFIDRequest: GET playstore-gsfid answers the running instance's GSF ID; not booted (per the recent
// list) or busy with a job is 409 without asking the guest; no ID yet keeps its reason and advice.
func TestPlayStoreGSFIDRequest(t *testing.T) {
	resetPSFake(t)
	h := newHarness(t, nil)
	h.f.put(3, emu.StatusOnline)
	expect(t, h.do("GET", "/api/instances", nil), http.StatusOK, nil) // the list a request may use

	var got emu.PlayStoreGSFIDResult
	expect(t, h.do("GET", "/api/instances/3/playstore-gsfid", nil), http.StatusOK, &got)
	if got.Index != 3 || got.Decimal != "3881793548573364239" || got.Hex != "35dee6aa721e500f" {
		t.Errorf("result %+v", got)
	}
	if e := expectError(t, h.do("GET", "/api/instances/4/playstore-gsfid", nil), http.StatusConflict, errs.Busy); !strings.Contains(e.Advice, "GSF ID") {
		t.Errorf("off: %+v", e)
	}

	psFake.mu.Lock()
	psFake.gsfidErr = errs.WithDetail(errs.New(errs.NotReady, "实例 3 还没有 GSF ID").WithAdvice("开着网等一两分钟"), errs.Reason, emu.PlayStoreReasonNoGSFID)
	psFake.mu.Unlock()
	if e := expectError(t, h.do("GET", "/api/instances/3/playstore-gsfid", nil), http.StatusUnprocessableEntity, errs.NotReady); e.Details[errs.Reason] != emu.PlayStoreReasonNoGSFID || e.Advice != "开着网等一两分钟" {
		t.Errorf("no ID: %+v", e)
	}
	if want := []string{"gsfid 3", "gsfid 3"}; !slices.Equal(psCalls(), want) {
		t.Errorf("calls %v, want %v", psCalls(), want)
	}

	// a job on the instance (a disk compact held at its gate) keeps the guest to itself
	h.f.mu.Lock()
	h.f.diskGate = make(chan struct{})
	h.f.mu.Unlock()
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/disk-compact", nil), http.StatusAccepted, &j)
	expectError(t, h.do("GET", "/api/instances/3/playstore-gsfid", nil), http.StatusConflict, errs.Busy)
	close(h.f.diskGate)
	h.waitJob(j.ID, func(v jobView) bool { return v.State != stateRunning })
	if n := len(psCalls()); n != 2 {
		t.Errorf("asked the guest while busy: %v", psCalls())
	}
}

func (f *fakeDiag) ImportPlayStore(ctx context.Context, files []components.ImportFile, progress func(DiagProgress)) (*components.PlayStoreImportResult, error) {
	var got []importedFile
	for _, in := range files {
		b, err := os.ReadFile(in.Path)
		if err != nil {
			return nil, err
		}
		got = append(got, importedFile{in.Name, in.Path, string(b)})
	}
	psFake.mu.Lock()
	psFake.imported = append(psFake.imported, got)
	gate, fail := psFake.gate, psFake.importErr
	psFake.mu.Unlock()
	progress(DiagProgress{Stage: "verifying", Done: 1, Total: 2})
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
	}
	if fail != nil {
		return nil, fail
	}
	return &components.PlayStoreImportResult{Root: fakeRoot.Root, From: "files", Used: []string{}, Kept: []string{},
		Status: components.PlayStoreStatus{Ready: true, Source: components.PlayStoreFromImport, Files: []components.PlayStoreFile{}}}, nil
}

// storeDiag is a diagnostic backend that records what each fetch asked about the Google Play Store.
type storeDiag struct {
	fakeDiag
	mu    sync.Mutex
	asked []bool
}

func (d *storeDiag) Fetch(ctx context.Context, accept, playStore bool, s components.SourceSettings, sha string, progress func(DiagProgress)) (*DiagFetchResult, error) {
	d.mu.Lock()
	d.asked = append(d.asked, playStore)
	d.mu.Unlock()
	return &DiagFetchResult{Root: fakeRoot.Root}, nil
}

// TestPlayStoreFetchAndCreate: play_store in a fetch body reaches the backend, without it the store is not asked
// for; a create body with play_store is refused (new instances never get the store).
func TestPlayStoreFetchAndCreate(t *testing.T) {
	d := &storeDiag{}
	h := newHarness(t, func(o *Options) { o.newDiagBackend = func(*logx.Logger) diagBackend { return d } })
	for _, extra := range []string{"", `,"play_store":true`} {
		var j jobView
		expect(t, h.do("POST", "/api/components/fetch", fetchBody(extra)), http.StatusAccepted, &j)
		h.waitJob(j.ID, stateIs(stateDone))
	}
	d.mu.Lock()
	if len(d.asked) != 2 || d.asked[0] || !d.asked[1] {
		t.Errorf("fetches asked %v", d.asked)
	}
	d.mu.Unlock()
	expectError(t, h.do("POST", "/api/instances", map[string]any{"index": 3, "play_store": true}), http.StatusBadRequest, errs.Usage)
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if len(h.f.created) != 0 {
		t.Errorf("created %+v", h.f.created)
	}
}

// TestPlayStoreInstanceJobs: playstore-install / playstore-uninstall check first, then run as the instance's job
// (kind, index, result); what the check refuses is answered at once with its reason and nothing runs.
func TestPlayStoreInstanceJobs(t *testing.T) {
	resetPSFake(t)
	h := newHarness(t, nil)
	for _, action := range []string{"install", "uninstall"} {
		var j jobView
		expect(t, h.do("POST", "/api/instances/3/playstore-"+action, nil), http.StatusAccepted, &j)
		if j.Kind != "playstore-"+action || j.Index != 3 {
			t.Fatalf("job %+v", j)
		}
		done := h.waitJob(j.ID, stateIs(stateDone))
		res, _ := done.Result.(map[string]any)
		if res["action"] != action || res["restarted"] != true {
			t.Errorf("result %v", done.Result)
		}
	}
	if want := []string{"check install 3", "run install 3", "check uninstall 3", "run uninstall 3"}; !slices.Equal(psCalls(), want) {
		t.Errorf("calls %v, want %v", psCalls(), want)
	}

	for _, reason := range []string{emu.PlayStoreReasonFiles, emu.PlayStoreReasonNoBridge} {
		resetPSFake(t)
		psFake.mu.Lock()
		psFake.checkErr = errs.WithDetail(errs.New(errs.NotReady, "不行").WithAdvice("先补全"), errs.Reason, reason)
		psFake.mu.Unlock()
		e := expectError(t, h.do("POST", "/api/instances/3/playstore-install", nil), http.StatusUnprocessableEntity, errs.NotReady)
		if e.Details[errs.Reason] != reason || e.Advice != "先补全" {
			t.Errorf("%s: %+v", reason, e)
		}
		if want := []string{"check install 3"}; !slices.Equal(psCalls(), want) {
			t.Errorf("%s: calls %v", reason, psCalls())
		}
	}
	expectError(t, h.do("POST", "/api/instances/x/playstore-install", nil), http.StatusBadRequest, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances/3/playstore-install", `{"force":true}`), http.StatusBadRequest, errs.Usage)
}

// importBody is a multipart body with one "file" part per entry (name, content).
func importBody(t *testing.T, files ...[2]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("note", "ignored")
	for _, f := range files {
		fw, err := mw.CreateFormFile("file", f[0])
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(fw, f[1])
	}
	mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

// TestImportPlayStoreUpload: the uploaded files reach the import under their names (two of the same name both),
// as a root job; the upload folder is gone after it. Not multipart, no file, an empty one, too many and one larger
// than the zip are refused before any job. A refusal of the import fails the job with its reason.
func TestImportPlayStoreUpload(t *testing.T) {
	resetPSFake(t)
	tmp := testutil.TempDir(t)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	h := diagHarness(t, &fakeDiag{}, nil)

	body, ct := importBody(t, [2]string{"Phonesky.apk", "store"}, [2]string{`..\..\cert.der`, "cert"}, [2]string{"Phonesky.apk", "again"})
	var j jobView
	expect(t, h.do("POST", "/api/components/import-playstore", body, withHeader("Content-Type", ct)), http.StatusAccepted, &j)
	if j.Kind != kindPlayStoreImport || j.Index != rootJobIndex {
		t.Fatalf("job %+v", j)
	}
	done := h.waitJob(j.ID, stateIs(stateDone))
	if res, _ := done.Result.(map[string]any); res["from"] != "files" {
		t.Errorf("result %v", done.Result)
	}
	psFake.mu.Lock()
	got := psFake.imported
	psFake.mu.Unlock()
	if len(got) != 1 || len(got[0]) != 3 {
		t.Fatalf("imported %+v", got)
	}
	for k, want := range []importedFile{{name: "Phonesky.apk", content: "store"}, {name: "cert.der", content: "cert"}, {name: "Phonesky.apk", content: "again"}} {
		if g := got[0][k]; g.name != want.name || g.content != want.content || !strings.HasPrefix(g.path, uploadRoot()+`\`) {
			t.Errorf("file %d: %+v", k, g)
		}
	}
	if _, err := os.Stat(filepath.Dir(got[0][0].path)); !os.IsNotExist(err) {
		t.Errorf("upload folder left: %v", err)
	}

	expectError(t, h.do("POST", "/api/components/import-playstore", `{"file":"C:\\x.zip"}`), http.StatusBadRequest, errs.Usage)
	for _, c := range []struct {
		files [][2]string
		code  string
	}{
		{nil, errs.Usage},
		{[][2]string{{"a.apk", ""}}, errs.InvalidArgument},
		{[][2]string{{"1", "a"}, {"2", "a"}, {"3", "a"}, {"4", "a"}, {"5", "a"}, {"6", "a"}, {"7", "a"}, {"8", "a"}, {"9", "a"}}, errs.InvalidArgument},
	} {
		body, ct := importBody(t, c.files...)
		expectError(t, h.do("POST", "/api/components/import-playstore", body, withHeader("Content-Type", ct)), http.StatusBadRequest, c.code)
	}
	old := maxImportPart
	maxImportPart = 4
	body, ct = importBody(t, [2]string{"big.zip", "12345"})
	expectError(t, h.do("POST", "/api/components/import-playstore", body, withHeader("Content-Type", ct)), http.StatusBadRequest, errs.InvalidArgument)
	maxImportPart = old
	psFake.mu.Lock()
	n := len(psFake.imported)
	psFake.mu.Unlock()
	if n != 1 {
		t.Errorf("imports after the refusals: %d", n)
	}
	if left, _ := os.ReadDir(uploadRoot()); len(left) != 0 {
		t.Errorf("upload folders left: %v", left)
	}

	psFake.mu.Lock()
	psFake.importErr = errs.WithDetail(errs.New(errs.ComponentsRejected, "SHA-256 对不上"), errs.Reason, components.ImportReasonMismatch)
	psFake.mu.Unlock()
	body, ct = importBody(t, [2]string{"Phonesky.apk", "forged"})
	expect(t, h.do("POST", "/api/components/import-playstore", body, withHeader("Content-Type", ct)), http.StatusAccepted, &j)
	failed := h.waitJob(j.ID, stateIs(stateFailed))
	if failed.Error == nil || failed.Error.Code != errs.ComponentsRejected || failed.Error.Details[errs.Reason] != components.ImportReasonMismatch {
		t.Errorf("refused import: %+v", failed.Error)
	}
}

// TestPlayStoreImportExcludesBoots: while an import runs, the store's install (a boot), a start, another import and a
// components fetch are busy; once it is done the install runs.
func TestPlayStoreImportExcludesBoots(t *testing.T) {
	resetPSFake(t)
	tmp := testutil.TempDir(t)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	gate := make(chan struct{})
	psFake.mu.Lock()
	psFake.gate = gate
	psFake.mu.Unlock()
	h := diagHarness(t, &fakeDiag{}, nil)
	body, ct := importBody(t, [2]string{"Phonesky.apk", "store"})
	var j jobView
	expect(t, h.do("POST", "/api/components/import-playstore", body, withHeader("Content-Type", ct)), http.StatusAccepted, &j)
	h.waitJob(j.ID, func(v jobView) bool { return v.Progress != nil })
	expectError(t, h.do("POST", "/api/instances/3/playstore-install", nil), http.StatusConflict, errs.Busy)
	expectError(t, h.do("POST", "/api/instances/3/start", nil), http.StatusConflict, errs.Busy)
	expectError(t, h.do("POST", "/api/components/import-playstore", body, withHeader("Content-Type", ct)), http.StatusConflict, errs.Busy)
	expectError(t, h.do("POST", "/api/components/fetch", fetchBody("")), http.StatusConflict, errs.Busy)
	close(gate)
	h.waitJob(j.ID, stateIs(stateDone))
	var k jobView
	expect(t, h.do("POST", "/api/instances/3/playstore-install", nil), http.StatusAccepted, &k)
	h.waitJob(k.ID, stateIs(stateDone))
}

// TestRealDiagBackendPlayStore: the real adapter hands components.Fetch the play_store choice and the import its
// files with the job's log; the component list ends with the store (optional) and play_store has its five files.
func TestRealDiagBackendPlayStore(t *testing.T) {
	oldFetch, oldImport := diagFetch, diagImport
	t.Cleanup(func() { diagFetch, diagImport = oldFetch, oldImport })
	var got []components.Options
	diagFetch = func(_ context.Context, r *root.Root, o components.Options) (*components.Result, error) {
		got = append(got, o)
		return &components.Result{Root: r.Dir}, nil
	}
	var imported []components.ImportFile
	diagImport = func(_ context.Context, r *root.Root, files []components.ImportFile, o components.Options) (*components.PlayStoreImportResult, error) {
		imported = append(imported, files...)
		if o.Log == nil || o.Progress == nil {
			t.Error("import without log or progress")
		}
		o.Progress(components.Progress{Stage: "verifying", Done: 1, Total: 2})
		return &components.PlayStoreImportResult{Root: r.Dir}, nil
	}
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	b := newRealDiagBackend(r, logx.Nop())
	for _, store := range []bool{false, true} {
		if _, err := b.Fetch(context.Background(), true, store, components.SourceSettings{}, fakeHash, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 2 || got[0].PlayStore || !got[1].PlayStore {
		t.Fatalf("options %+v", got)
	}
	var progress []DiagProgress
	files := []components.ImportFile{{Path: `C:\u\1-Phonesky.apk`, Name: "Phonesky.apk"}}
	if _, err := b.ImportPlayStore(context.Background(), files, func(p DiagProgress) { progress = append(progress, p) }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(imported, files) || len(progress) != 1 || progress[0].Stage != "verifying" {
		t.Errorf("imported %+v, progress %+v", imported, progress)
	}

	res, err := b.Components(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	last := res.Components[len(res.Components)-1]
	if last.ID != "play-store" || !last.Optional || last.Installed || res.PlayStore.Ready || len(res.PlayStore.Files) != 5 {
		t.Errorf("components %+v, play_store %+v", res.Components, res.PlayStore)
	}
}
