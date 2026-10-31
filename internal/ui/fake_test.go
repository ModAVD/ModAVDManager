//go:build windows

package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
)

const testToken = "test-token-0123456789"

// fake is the emu side of the tests: instances in memory, calls recorded, Start / Stop optionally held at a gate.
type fake struct {
	mu        sync.Mutex
	instances map[int]emu.InstanceInfo
	infoErr   map[int]error
	created   []emu.CreateRequest
	updated   []emu.UpdateRequest
	deleted   []int
	starts    []emu.StartOptions
	stops     []emu.StopOptions
	shells    []string
	shellCode int
	installs  []fakeInstall
	startGate chan struct{} // Start waits here (after its first progress) when set
	stopGate  chan struct{}

	shot      adb.Result
	shotDelay time.Duration
	adbCalls  atomic.Int32
	listCalls atomic.Int32
	checks    atomic.Int32
}

type fakeInstall struct {
	path    string
	content string
}

func newFake() *fake {
	f := &fake{instances: map[int]emu.InstanceInfo{}, infoErr: map[int]error{}}
	f.put(3, emu.StatusOffline)
	f.put(4, emu.StatusOffline)
	return f
}

func (f *fake) put(i int, st emu.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.instances[i] = emu.InstanceInfo{
		Index: i, Name: root.AvdName(i), Title: root.AvdName(i), Status: st,
		Running: st != emu.StatusOffline, Booted: st == emu.StatusOnline, Serial: root.Serial(i),
		Config: emu.InstanceConfig{MemoryMB: 6144, CPU: 6, FPS: 60, Width: 1280, Height: 720, DPI: 240},
	}
}

func (f *fake) get(i int) (emu.InstanceInfo, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in, ok := f.instances[i]
	return in, ok
}

var fakeRoot = emu.RootInfo{Root: `C:\fake-root`, SDK: `C:\fake-root\sdk`, AdbServerPort: 20050,
	ScriptAdbServerPort: 20049, Version: "0.35.0", Build: 35}

// fakeBackend is one backend built by the server (per job, per request kind) over the shared fake.
type fakeBackend struct {
	f   *fake
	log *logx.Logger
}

func canceledErr(ctx context.Context) error { return errs.Wrap(errs.Canceled, ctx.Err(), "已取消") }

func (b fakeBackend) List(ctx context.Context) (*emu.ListResult, error) {
	b.f.listCalls.Add(1)
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	res := &emu.ListResult{RootInfo: fakeRoot, Instances: []emu.InstanceInfo{}}
	for _, in := range b.f.instances {
		res.Instances = append(res.Instances, in)
	}
	sort.Slice(res.Instances, func(a, c int) bool { return res.Instances[a].Index < res.Instances[c].Index })
	return res, nil
}

func (b fakeBackend) Info(ctx context.Context, i int) (*emu.InstanceInfo, error) {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	if err := b.f.infoErr[i]; err != nil {
		return nil, err
	}
	in, ok := b.f.instances[i]
	if !ok {
		return nil, errs.Newf(errs.NotFound, "魔改 AVD 实例 %d 不存在", i)
	}
	return &in, nil
}

func (b fakeBackend) Create(ctx context.Context, req emu.CreateRequest) (*emu.InstanceInfo, error) {
	b.f.mu.Lock()
	b.f.created = append(b.f.created, req)
	b.f.mu.Unlock()
	i := 2
	if req.Index != nil {
		i = *req.Index
	}
	b.f.put(i, emu.StatusOffline)
	in, _ := b.f.get(i)
	in.Title = req.Title
	return &in, nil
}

// FreeIndex is the fake's "smallest free index": 2 when nothing is there, as Create without an index assumes.
func (b fakeBackend) FreeIndex() (int, error) {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	for i := 2; i <= 9; i++ {
		if _, ok := b.f.instances[i]; !ok && i != 5 {
			return i, nil
		}
	}
	return 0, errs.New(errs.AlreadyExists, "没有空闲编号")
}

func (b fakeBackend) Update(ctx context.Context, i int, req emu.UpdateRequest) (*emu.UpdateResult, error) {
	b.f.mu.Lock()
	b.f.updated = append(b.f.updated, req)
	b.f.mu.Unlock()
	in, ok := b.f.get(i)
	if !ok {
		return nil, errs.Newf(errs.NotFound, "魔改 AVD 实例 %d 不存在", i)
	}
	applied := map[string]any{}
	if req.FPS != nil {
		applied["fps"] = *req.FPS
	}
	return &emu.UpdateResult{Instance: in, Applied: applied}, nil
}

func (b fakeBackend) Delete(ctx context.Context, i int) (*emu.DeleteResult, error) {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	if _, ok := b.f.instances[i]; !ok {
		return nil, errs.Newf(errs.NotFound, "魔改 AVD 实例 %d 不存在", i)
	}
	delete(b.f.instances, i)
	b.f.deleted = append(b.f.deleted, i)
	return &emu.DeleteResult{Index: i, Name: root.AvdName(i)}, nil
}

func (b fakeBackend) Start(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
	b.f.mu.Lock()
	b.f.starts = append(b.f.starts, opt)
	gate := b.f.startGate
	b.f.mu.Unlock()
	if _, ok := b.f.get(i); !ok {
		return nil, errs.Newf(errs.NotFound, "魔改 AVD 实例 %d 不存在", i)
	}
	opt.Progress(emu.Progress{Step: "launch", Message: "实例已拉起", Done: 50, Total: 1000})
	b.log.Warnf("测试警告 %d", i)
	b.f.put(i, emu.StatusStarting)
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, canceledErr(ctx)
		}
	}
	opt.Progress(emu.Progress{Step: "boot", Message: "安卓已开机", Done: 610, Total: 1000})
	b.f.put(i, emu.StatusOnline)
	in, _ := b.f.get(i)
	return &emu.StartResult{Instance: in, Launched: true, Warnings: []string{}}, nil
}

func (b fakeBackend) Stop(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
	b.f.mu.Lock()
	b.f.stops = append(b.f.stops, opt)
	gate := b.f.stopGate
	b.f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, canceledErr(ctx)
		}
	}
	b.f.put(i, emu.StatusOffline)
	return &emu.StopResult{Index: i, Name: root.AvdName(i), Method: "graceful", Synced: true, Seconds: 1.5,
		Warnings: []string{}}, nil
}

func (b fakeBackend) Check(ctx context.Context, opt emu.CheckOptions) (*emu.CheckResult, error) {
	b.f.checks.Add(1)
	return &emu.CheckResult{RootInfo: fakeRoot, Ready: true, Items: []emu.CheckItem{}, Components: []emu.ComponentStatus{}}, nil
}

func (b fakeBackend) SetWindowVisible(ctx context.Context, i int, visible bool) (*emu.WindowResult, error) {
	return &emu.WindowResult{Index: i, Visible: visible, Windows: 2}, nil
}

func (b fakeBackend) Shell(ctx context.Context, i int, command string, timeout time.Duration) (*emu.ShellResult, error) {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	b.f.shells = append(b.f.shells, command)
	return &emu.ShellResult{ExitCode: b.f.shellCode, Output: ""}, nil
}

func (b fakeBackend) InstallAPK(ctx context.Context, i int, apkPath string) (*emu.InstallResult, error) {
	data, err := os.ReadFile(apkPath)
	if err != nil {
		return nil, err
	}
	b.f.mu.Lock()
	b.f.installs = append(b.f.installs, fakeInstall{apkPath, string(data)})
	b.f.mu.Unlock()
	return &emu.InstallResult{Index: i, APK: apkPath, Package: "com.example", Output: "Success"}, nil
}

func (b fakeBackend) AdbRun(ctx context.Context, opt adb.RunOptions, args ...string) adb.Result {
	b.f.adbCalls.Add(1)
	b.f.mu.Lock()
	res, delay := b.f.shot, b.f.shotDelay
	b.f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return adb.Result{Code: -1, Out: "已取消"}
		}
	}
	return res
}

func (b fakeBackend) RootInfo() emu.RootInfo { return fakeRoot }

// ---- harness -----------------------------------------------------------------------------------------------------

type harness struct {
	t      *testing.T
	s      *server
	ts     *httptest.Server
	f      *fake
	client *http.Client

	mu     sync.Mutex
	opened []string
}

func newHarness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()
	r, err := root.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, f: newFake()}
	opt := Options{
		Root:       r,
		token:      testToken,
		newBackend: func(l *logx.Logger) backend { return fakeBackend{h.f, l} },
		openPath: func(p string) error {
			h.mu.Lock()
			h.opened = append(h.opened, p)
			h.mu.Unlock()
			return nil
		},
		launch: func(string) (*Window, error) { return nil, errors.New("no windows in tests") },
	}
	if mod != nil {
		mod(&opt)
	}
	s, err := newServer(opt)
	if err != nil {
		t.Fatal(err)
	}
	s.pollEvery = 50 * time.Millisecond
	s.stopWait = 2 * time.Second
	h.s = s
	h.ts = httptest.NewUnstartedServer(s)
	s.port = h.ts.Listener.Addr().(*net.TCPAddr).Port
	h.ts.Start()
	go s.pollLoop()
	h.client = &http.Client{
		Timeout:       15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	t.Cleanup(func() {
		s.cancel()
		s.hub.closeAll()
		s.jobCancel()
		h.ts.Close()
		s.jobs.waitIdle(5 * time.Second)
	})
	return h
}

type reqOpt func(*http.Request)

func noCookie(r *http.Request) { r.Header.Del("Cookie") }
func withHeader(k, v string) reqOpt {
	return func(r *http.Request) {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
}
func withHost(host string) reqOpt { return func(r *http.Request) { r.Host = host } }

// do sends a request with the login cookie and our Origin; body is nil, a raw JSON string, or a value to marshal.
func (h *harness) do(method, path string, body any, opts ...reqOpt) *http.Response {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	case []byte:
		rd = bytes.NewReader(b)
	default:
		data, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Cookie", h.s.cookieName()+"="+testToken)
	if method != http.MethodGet {
		req.Header.Set("Origin", h.ts.URL)
	}
	for _, o := range opts {
		o(req)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// expect checks the status and decodes the JSON body into out (when not nil).
func expect(t *testing.T, resp *http.Response, status int, out any) {
	t.Helper()
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d; body %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode,
			status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			t.Fatalf("%s %s: body is not JSON: %v: %s", resp.Request.Method, resp.Request.URL.Path, err, data)
		}
	}
}

type apiError struct {
	Error errorBody `json:"error"`
}

// expectError checks the status and the error code of a JSON error.
func expectError(t *testing.T, resp *http.Response, status int, code string) errorBody {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("error Content-Type %q", ct)
	}
	var e apiError
	expect(t, resp, status, &e)
	if e.Error.Code != code {
		t.Fatalf("error code %q (%s), want %q", e.Error.Code, e.Error.Message, code)
	}
	if e.Error.Message == "" {
		t.Fatalf("error without message: %+v", e.Error)
	}
	return e.Error
}

// waitJob polls GET /api/jobs/{id} until pred holds.
func (h *harness) waitJob(id string, pred func(jobView) bool) jobView {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v jobView
		expect(h.t, h.do("GET", "/api/jobs/"+id, nil), http.StatusOK, &v)
		if pred(v) {
			return v
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("job %s never reached the expected state; last %+v", id, v)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stateIs(state string) func(jobView) bool {
	return func(v jobView) bool { return v.State == state }
}

// ---- event stream reader -----------------------------------------------------------------------------------------

type sseEvent struct {
	name string
	data string
}

type sseStream struct {
	t      *testing.T
	resp   *http.Response
	events chan sseEvent
}

func (h *harness) events() *sseStream {
	h.t.Helper()
	resp := h.do("GET", "/api/events", nil)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("events: status %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		h.t.Fatalf("events Content-Type %q", ct)
	}
	st := &sseStream{t: h.t, resp: resp, events: make(chan sseEvent, 1024)}
	go func() {
		defer close(st.events)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" {
					st.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	h.t.Cleanup(func() { resp.Body.Close() })
	return st
}

func (st *sseStream) close() { st.resp.Body.Close() }

// waitFor returns the first event named name for which pred (given the decoded data) holds.
func waitFor[T any](st *sseStream, name string, pred func(T) bool) T {
	st.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-st.events:
			if !ok {
				st.t.Fatalf("event stream ended while waiting for %s", name)
			}
			if ev.name != name {
				continue
			}
			var v T
			if err := json.Unmarshal([]byte(ev.data), &v); err != nil {
				st.t.Fatalf("%s event is not JSON: %v: %s", name, err, ev.data)
			}
			if pred == nil || pred(v) {
				return v
			}
		case <-timeout:
			st.t.Fatalf("no matching %s event within 5 s", name)
		}
	}
}
