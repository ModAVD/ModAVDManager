//go:build windows

package ui

import (
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

// POST /api/instances/{i}/start with "launch" hands the app to emu; a name that is not one is refused before a job
// exists, and a start without it opens nothing.
func TestStartWithLaunch(t *testing.T) {
	h := newHarness(t, nil)
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/start", `{"launch":"com.example.app/.Main"}`), http.StatusAccepted, &j)
	h.waitJob(j.ID, stateIs(stateDone))
	expect(t, h.do("POST", "/api/instances/3/start", `{}`), http.StatusAccepted, &j)
	h.waitJob(j.ID, stateIs(stateDone))

	for _, bad := range []string{"app", "com.x;reboot", "com.x/.A$B", "com.x/$(id)", "$(id).x", "com.x -n y"} {
		e := expectError(t, h.do("POST", "/api/instances/3/start", `{"launch":"`+bad+`"}`), http.StatusBadRequest, errs.InvalidArgument)
		if e.Message == "" {
			t.Errorf("%q: no message", bad)
		}
	}
	expectError(t, h.do("POST", "/api/instances/3/start", `{"launch":5}`), http.StatusBadRequest, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances/3/start", `{"package":"com.example.app"}`), http.StatusBadRequest, errs.Usage)

	h.f.mu.Lock()
	starts := slices.Clone(h.f.starts)
	h.f.mu.Unlock()
	if len(starts) != 2 || starts[0].Launch == nil || *starts[0].Launch != (emu.AppTarget{Package: "com.example.app", Activity: ".Main"}) ||
		!starts[0].Window || starts[1].Launch != nil {
		t.Fatalf("start options %+v", starts)
	}
}

// ModAVD.exe --start: Options.Start is the same job as the request, begun once the server listens.
func TestRunStartsTheRequestedInstance(t *testing.T) {
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	done := make(chan error, 1)
	go func() {
		done <- Run(t.Context(), Options{Root: r, IdleExit: 100 * time.Millisecond, token: testToken,
			newBackend: func(l *logx.Logger) backend { return fakeBackend{f, l} },
			removeOld:  func(*logx.Logger) {},
			Start:      &StartRequest{Index: 3, Launch: "com.example.app"}})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not exit")
	}
	f.mu.Lock()
	starts := slices.Clone(f.starts)
	f.mu.Unlock()
	if len(starts) != 1 || !starts[0].Window || starts[0].Launch == nil || starts[0].Launch.Package != "com.example.app" {
		t.Fatalf("start options %+v", starts)
	}
}

// An explicit empty name is a bad name (400, no job), as for the CLI and MCP; an absent or null field is no app.
func TestStartWithEmptyLaunchIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	e := expectError(t, h.do("POST", "/api/instances/3/start", `{"launch":""}`), http.StatusBadRequest, errs.InvalidArgument)
	if e.Message == "" {
		t.Error("no message")
	}
	h.f.mu.Lock()
	n := len(h.f.starts)
	h.f.mu.Unlock()
	var jobs struct{ Jobs []jobView }
	expect(t, h.do("GET", "/api/jobs", nil), http.StatusOK, &jobs)
	if n != 0 || len(jobs.Jobs) != 0 {
		t.Fatalf("a job began: %d starts, %d jobs", n, len(jobs.Jobs))
	}
	for _, body := range []string{`{}`, `{"launch":null}`} {
		var j jobView
		expect(t, h.do("POST", "/api/instances/3/start", body), http.StatusAccepted, &j)
		h.waitJob(j.ID, stateIs(stateDone))
	}
	h.f.mu.Lock()
	defer h.f.mu.Unlock()
	if len(h.f.starts) != 2 || h.f.starts[0].Launch != nil || h.f.starts[1].Launch != nil {
		t.Fatalf("start options %+v", h.f.starts)
	}
}

// ModAVD.exe --start that the manager does not accept ends Run with an error that says so, without a job and without
// a manager left running; one that is accepted ends with no error even when its app cannot be opened.
func TestRunStartNotAccepted(t *testing.T) {
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	f := newFake()
	run := func(start *StartRequest) error {
		done := make(chan error, 1)
		go func() {
			done <- Run(t.Context(), Options{Root: r, IdleExit: 100 * time.Millisecond, token: testToken,
				newBackend: func(l *logx.Logger) backend { return fakeBackend{f, l} },
				removeOld:  func(*logx.Logger) {}, Start: start})
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(15 * time.Second):
			t.Fatal("Run did not exit")
			return nil
		}
	}
	for _, launch := range []string{"com.x;reboot", "app"} {
		err := run(&StartRequest{Index: 3, Launch: launch})
		var refused *StartRefusedError
		if !errors.As(err, &refused) || refused.Index != 3 || errs.CodeOf(err) != errs.InvalidArgument {
			t.Fatalf("%q: %v", launch, err)
		}
	}
	f.mu.Lock()
	n := len(f.starts)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d starts for refused requests", n)
	}
	if err := run(&StartRequest{Index: 3, Launch: "com.example.missing"}); err != nil {
		t.Fatalf("accepted request: %v", err)
	}
}
