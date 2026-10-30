//go:build windows

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

// ---- fake backend ------------------------------------------------------------------------------------------------

type fake struct {
	mu      sync.Mutex
	calls   []string
	creates []emu.CreateRequest
	updates []emu.UpdateRequest
	starts  []emu.StartOptions
	stops   []emu.StopOptions
	shells  []string
	timeout time.Duration

	start func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error)
	del   func(ctx context.Context, i int) (*emu.DeleteResult, error)
	wait  func(timeout time.Duration)
	waits []time.Duration
}

func (f *fake) Wait(timeout time.Duration) {
	f.mu.Lock()
	f.waits = append(f.waits, timeout)
	wait := f.wait
	f.mu.Unlock()
	if wait != nil {
		wait(timeout)
	}
}

func (f *fake) waitCalls() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

func (f *fake) record(format string, a ...any) {
	f.mu.Lock()
	f.calls = append(f.calls, fmt.Sprintf(format, a...))
	f.mu.Unlock()
}

func (f *fake) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func sampleInstance(i int) emu.InstanceInfo {
	return emu.InstanceInfo{
		Index: i, Name: fmt.Sprintf("mas_%d", i), Title: fmt.Sprintf("实例 %d", i), Status: emu.StatusOffline,
		Serial: fmt.Sprintf("emulator-%d", 20000+10*i), AdbAddress: fmt.Sprintf("127.0.0.1:%d", 20001+10*i),
		Ports:     emu.Ports{Console: 20000 + 10*i, Adb: 20001 + 10*i, Grpc: 20002 + 10*i},
		Screencap: emu.Screencap{Shm: fmt.Sprintf("SHM_videmulator%d", 20000+10*i)},
		Config:    emu.InstanceConfig{MemoryMB: 6144, CPU: 6, FPS: 60, DataPartitionGB: 64, Width: 1280, Height: 720, DPI: 240},
		Path:      fmt.Sprintf(`D:\r1\avd\mas_%d.avd`, i),
	}
}

func sampleRoot() emu.RootInfo {
	return emu.RootInfo{Root: `D:\r1`, SDK: `D:\r1\sdk`, AdbExe: `D:\r1\sdk\platform-tools\adb.exe`,
		AdbServerPort: 20050, ScriptAdbServerPort: 20049}
}

func (f *fake) List(ctx context.Context) (*emu.ListResult, error) {
	f.record("list")
	running := sampleInstance(1)
	running.Status, running.Running, running.Booted, running.PID = emu.StatusOnline, true, true, 4242
	running.StartedAt = "2026-10-28T15:04:05+01:00"
	return &emu.ListResult{RootInfo: sampleRoot(), Instances: []emu.InstanceInfo{sampleInstance(0), running}}, nil
}

func (f *fake) Info(ctx context.Context, i int) (*emu.InstanceInfo, error) {
	f.record("info %d", i)
	if i == 9 {
		return nil, errs.Newf(errs.NotFound, "魔改 AVD 实例 %d 不存在", i).WithAdvice("先用 list_instances 看有哪些实例")
	}
	in := sampleInstance(i)
	return &in, nil
}

func (f *fake) Check(ctx context.Context, opt emu.CheckOptions) (*emu.CheckResult, error) {
	f.record("check %d", opt.MemoryMB)
	ok := true
	return &emu.CheckResult{RootInfo: sampleRoot(), Ready: true, Items: []emu.CheckItem{
		{ID: "path", Title: "路径", OK: &ok, Blocking: true, Reason: "路径可用"},
		{ID: "vulkan", Title: "Vulkan", OK: nil, Reason: "现在查不了"},
	}}, nil // Components left nil on purpose: the schema must allow null
}

func (f *fake) Create(ctx context.Context, req emu.CreateRequest) (*emu.InstanceInfo, error) {
	f.mu.Lock()
	f.creates = append(f.creates, req)
	f.mu.Unlock()
	f.record("create")
	i := 2
	if req.Index != nil {
		i = *req.Index
	}
	in := sampleInstance(i)
	return &in, nil
}

func (f *fake) Update(ctx context.Context, i int, req emu.UpdateRequest) (*emu.UpdateResult, error) {
	f.mu.Lock()
	f.updates = append(f.updates, req)
	f.mu.Unlock()
	f.record("update %d", i)
	applied := map[string]any{}
	if req.MemoryMB != nil {
		applied["memory_mb"] = *req.MemoryMB
	}
	return &emu.UpdateResult{Instance: sampleInstance(i), Applied: applied}, nil
}

func (f *fake) Delete(ctx context.Context, i int) (*emu.DeleteResult, error) {
	f.record("delete %d", i)
	f.mu.Lock()
	del := f.del
	f.mu.Unlock()
	if del != nil {
		return del(ctx, i)
	}
	return &emu.DeleteResult{Index: i, Name: fmt.Sprintf("mas_%d", i), Path: `D:\x`}, nil
}

func (f *fake) Start(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
	f.mu.Lock()
	f.starts = append(f.starts, opt)
	start := f.start
	f.mu.Unlock()
	f.record("start %d", i)
	if start != nil {
		return start(ctx, i, opt)
	}
	in := sampleInstance(i)
	in.Status = emu.StatusOnline
	return &emu.StartResult{Instance: in, Launched: true, BootSeconds: 41.3, TotalSeconds: 50, Warnings: []string{}}, nil
}

func (f *fake) Stop(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
	f.mu.Lock()
	f.stops = append(f.stops, opt)
	f.mu.Unlock()
	f.record("stop %d", i)
	return &emu.StopResult{Index: i, Name: fmt.Sprintf("mas_%d", i), Method: "graceful", Synced: true, Seconds: 9.5,
		Warnings: []string{}}, nil
}

func (f *fake) SetWindowVisible(ctx context.Context, i int, visible bool) (*emu.WindowResult, error) {
	f.record("window %d %v", i, visible)
	return &emu.WindowResult{Index: i, Visible: visible, Windows: 1}, nil
}

func (f *fake) Shell(ctx context.Context, i int, command string, timeout time.Duration) (*emu.ShellResult, error) {
	f.mu.Lock()
	f.shells = append(f.shells, command)
	f.timeout = timeout
	f.mu.Unlock()
	f.record("shell %d", i)
	if command == "panic" {
		panic("boom")
	}
	if command == "nil" {
		return nil, nil
	}
	return &emu.ShellResult{ExitCode: 1, Output: "line1\nline2 <b>&\"x\"\r\n"}, nil
}

func (f *fake) InstallAPK(ctx context.Context, i int, apkPath string) (*emu.InstallResult, error) {
	f.record("install %d %s", i, apkPath)
	return &emu.InstallResult{Index: i, APK: apkPath, Package: "com.example", Output: "Success"}, nil
}

// fakeConn is what one tool call gets: the shared fake plus that call's logger.
type fakeConn struct {
	*fake
	log *logx.Logger
}

func (c fakeConn) Info(ctx context.Context, i int) (*emu.InstanceInfo, error) {
	c.log.Infof("查询实例 %d", i)
	return c.fake.Info(ctx, i)
}

// ---- harness -----------------------------------------------------------------------------------------------------

type logBuf struct {
	mu    sync.Mutex
	lines []string
}

func (l *logBuf) sink(level logx.Level, msg string) {
	l.mu.Lock()
	l.lines = append(l.lines, level.String()+" "+msg)
	l.mu.Unlock()
}

func (l *logBuf) contains(s string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			return true
		}
	}
	return false
}

type harness struct {
	t      *testing.T
	s      *server
	in     *io.PipeWriter
	lines  chan string
	done   chan error
	cancel context.CancelFunc
	logs   *logBuf
	f      *fake
}

func newHarness(t *testing.T, f *fake, tune func(*server)) *harness {
	t.Helper()
	if f == nil {
		f = &fake{}
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	logs := &logBuf{}
	s := newServer(Options{Version: "9.9.9", Log: logx.New(logs.sink)}, func(log *logx.Logger) (backend, error) {
		return fakeConn{f, log}, nil
	})
	if tune != nil {
		tune(s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{t: t, s: s, in: inW, lines: make(chan string, 100), done: make(chan error, 1), cancel: cancel,
		logs: logs, f: f}
	go func() {
		err := s.serve(ctx, inR, outW)
		outW.Close()
		h.done <- err
	}()
	go func() {
		br := bufio.NewReader(outR)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				h.lines <- line
			}
			if err != nil {
				close(h.lines)
				return
			}
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		cancel()
	})
	return h
}

func (h *harness) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.in, line+"\n"); err != nil {
		h.t.Fatalf("write to server: %v", err)
	}
}

func (h *harness) sendJSON(v any) {
	h.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		h.t.Fatal(err)
	}
	h.send(string(b))
}

// recv returns the next message, checking the framing: exactly one newline, at the end, and valid JSON-RPC 2.0.
func (h *harness) recv() map[string]any {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			h.t.Fatal("server closed its output")
		}
		if !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
			h.t.Fatalf("bad framing: %q", line)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			h.t.Fatalf("server wrote invalid JSON %q: %v", line, err)
		}
		if m["jsonrpc"] != "2.0" {
			h.t.Fatalf("missing jsonrpc 2.0: %s", line)
		}
		return m
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for a message from the server")
	}
	return nil
}

// quiet asserts that nothing arrives within d.
func (h *harness) quiet(d time.Duration) {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if ok {
			h.t.Fatalf("unexpected message: %s", line)
		}
	case <-time.After(d):
	}
}

func (h *harness) request(id any, method string, params any) map[string]any {
	h.t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	h.sendJSON(msg)
	return h.recv()
}

func modernMeta(extra map[string]any) map[string]any {
	m := map[string]any{
		metaProtocolVersion:    modernVersion,
		metaClientCapabilities: map[string]any{},
		metaClientInfo:         map[string]any{"name": "test", "version": "1"},
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func modernParams(params map[string]any, meta map[string]any) map[string]any {
	if params == nil {
		params = map[string]any{}
	}
	params["_meta"] = modernMeta(meta)
	return params
}

func (h *harness) initialize(version string) map[string]any {
	h.t.Helper()
	res := h.request(1, "initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "legacy-client", "version": "1.0"}})
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return res
}

func result(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	r, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got %v", m)
	}
	return r
}

func errorCode(t *testing.T, m map[string]any) (int, string) {
	t.Helper()
	e, ok := m["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error, got %v", m)
	}
	return int(e["code"].(float64)), e["message"].(string)
}

func wantError(t *testing.T, m map[string]any, id any, code int) {
	t.Helper()
	got, msg := errorCode(t, m)
	if got != code {
		t.Fatalf("error code %d (%s), want %d", got, msg, code)
	}
	if !reflect.DeepEqual(m["id"], id) {
		t.Fatalf("error id %v, want %v", m["id"], id)
	}
}

// toolText returns the text block and isError of a tools/call result, and checks text == structuredContent.
func toolText(t *testing.T, m map[string]any) (string, bool, map[string]any) {
	t.Helper()
	r := result(t, m)
	content := r["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content blocks: %v", content)
	}
	block := content[0].(map[string]any)
	if block["type"] != "text" {
		t.Fatalf("content type %v", block["type"])
	}
	text := block["text"].(string)
	isError := r["isError"].(bool)
	sc, _ := r["structuredContent"].(map[string]any)
	if isError {
		if _, has := r["structuredContent"]; has {
			t.Fatalf("error result with structuredContent: %v", r)
		}
		return text, true, nil
	}
	if sc == nil {
		t.Fatalf("success without an object structuredContent: %v", r)
	}
	var fromText map[string]any
	if err := json.Unmarshal([]byte(text), &fromText); err != nil || !reflect.DeepEqual(fromText, sc) {
		t.Fatalf("text block %q differs from structuredContent %v (%v)", text, sc, err)
	}
	return text, false, sc
}

func (h *harness) toolsList(modern bool) []map[string]any {
	h.t.Helper()
	var params any
	if modern {
		params = modernParams(nil, nil)
	}
	r := result(h.t, h.request("list", "tools/list", params))
	var tools []map[string]any
	for _, x := range r["tools"].([]any) {
		tools = append(tools, x.(map[string]any))
	}
	return tools
}

// ---- transcripts -------------------------------------------------------------------------------------------------

func TestLegacySession(t *testing.T) {
	h := newHarness(t, nil, nil)

	ir := result(t, h.initialize("2025-11-25"))
	if ir["protocolVersion"] != "2025-11-25" {
		t.Fatalf("protocolVersion %v", ir["protocolVersion"])
	}
	caps := ir["capabilities"].(map[string]any)
	if _, ok := caps["tools"].(map[string]any); !ok || len(caps) != 1 {
		t.Fatalf("capabilities %v", caps)
	}
	info := ir["serverInfo"].(map[string]any)
	if info["name"] != "modavd" || info["version"] != "9.9.9" {
		t.Fatalf("serverInfo %v", info)
	}
	if s, _ := ir["instructions"].(string); !strings.Contains(s, "stop_instance") || !strings.Contains(s, "--root") ||
		!strings.Contains(s, "0-9 except 5") {
		t.Fatalf("instructions %q", s)
	}
	if _, has := ir["resultType"]; has {
		t.Fatal("legacy initialize has resultType")
	}

	r := result(t, h.request(2, "tools/list", nil))
	for _, k := range []string{"resultType", "ttlMs", "cacheScope", "_meta", "nextCursor"} {
		if _, has := r[k]; has {
			t.Fatalf("legacy tools/list has %s", k)
		}
	}
	if n := len(r["tools"].([]any)); n != 11 {
		t.Fatalf("%d tools", n)
	}

	m := h.request(3, "tools/call", map[string]any{"name": "list_instances", "arguments": map[string]any{},
		"_meta": map[string]any{"progressToken": 3}})
	if m["id"] != float64(3) {
		t.Fatalf("id %v", m["id"])
	}
	_, isErr, sc := toolText(t, m)
	if isErr || sc["root"] != `D:\r1` || len(sc["instances"].([]any)) != 2 {
		t.Fatalf("list_instances: %v", sc)
	}
	if _, has := result(t, m)["resultType"]; has {
		t.Fatal("legacy tools/call has resultType")
	}

	if r := result(t, h.request(4, "ping", nil)); len(r) != 0 {
		t.Fatalf("ping result %v", r)
	}
	// string ids are echoed as strings
	if m := h.request("abc", "ping", nil); m["id"] != "abc" {
		t.Fatalf("id %v", m["id"])
	}
}

func TestLegacyVersionNegotiation(t *testing.T) {
	for requested, want := range map[string]string{
		"2025-11-25": "2025-11-25",
		"2025-06-18": "2025-06-18",
		"2024-11-05": "2025-11-25",
		"2026-07-28": "2025-11-25",
	} {
		h := newHarness(t, nil, nil)
		if got := result(t, h.initialize(requested))["protocolVersion"]; got != want {
			t.Errorf("initialize %s -> %v, want %s", requested, got, want)
		}
	}
	h := newHarness(t, nil, nil)
	wantError(t, h.request(1, "initialize", map[string]any{"capabilities": map[string]any{}}), float64(1), codeInvalidParams)
}

func TestModernSession(t *testing.T) {
	h := newHarness(t, nil, nil)

	d := result(t, h.request(0, "server/discover", modernParams(nil, nil)))
	if d["resultType"] != "complete" || d["ttlMs"] != float64(listTTLMs) || d["cacheScope"] != "public" {
		t.Fatalf("discover %v", d)
	}
	if !reflect.DeepEqual(d["supportedVersions"], []any{modernVersion}) {
		t.Fatalf("supportedVersions %v", d["supportedVersions"])
	}
	if _, ok := d["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("capabilities %v", d["capabilities"])
	}
	si := d["_meta"].(map[string]any)[metaServerInfo].(map[string]any)
	if si["name"] != "modavd" || si["version"] != "9.9.9" {
		t.Fatalf("serverInfo %v", si)
	}
	if d["instructions"] == "" {
		t.Fatal("no instructions")
	}

	r := result(t, h.request(1, "tools/list", modernParams(nil, nil)))
	if r["resultType"] != "complete" || r["ttlMs"] != float64(listTTLMs) || r["cacheScope"] != "public" ||
		r["_meta"] == nil || len(r["tools"].([]any)) != 11 {
		t.Fatalf("tools/list %v", r)
	}

	m := h.request(2, "tools/call", modernParams(map[string]any{"name": "get_instance",
		"arguments": map[string]any{"index": 3}}, map[string]any{"progressToken": "t2"}))
	_, isErr, sc := toolText(t, m)
	if isErr || sc["name"] != "mas_3" {
		t.Fatalf("get_instance %v", sc)
	}
	res := result(t, m)
	if res["resultType"] != "complete" || res["_meta"].(map[string]any)[metaServerInfo] == nil {
		t.Fatalf("modern tools/call result %v", res)
	}
	// the call's own logger goes to Options.Log, prefixed with the tool and request id
	if !h.logs.contains("[get_instance 2] 查询实例 3") {
		t.Fatalf("call log missing: %v", h.logs.lines)
	}

	// no initialize needed, and initialize does not change the modern path
	h.initialize("2025-11-25")
	if r := result(t, h.request(5, "tools/list", modernParams(nil, nil))); r["resultType"] != "complete" {
		t.Fatalf("modern after initialize %v", r)
	}
}

func TestModernErrors(t *testing.T) {
	h := newHarness(t, nil, nil)

	m := h.request(5, "tools/list", map[string]any{"_meta": map[string]any{
		metaProtocolVersion: "2027-01-01", metaClientCapabilities: map[string]any{}}})
	wantError(t, m, float64(5), codeUnsupportedProtocolVersion)
	data := m["error"].(map[string]any)["data"].(map[string]any)
	if !reflect.DeepEqual(data["supported"], []any{modernVersion}) || data["requested"] != "2027-01-01" {
		t.Fatalf("data %v", data)
	}

	wantError(t, h.request(6, "tools/list", map[string]any{"_meta": map[string]any{metaProtocolVersion: modernVersion}}),
		float64(6), codeInvalidParams)
	wantError(t, h.request(7, "tools/list", map[string]any{"_meta": map[string]any{metaProtocolVersion: modernVersion,
		metaClientCapabilities: "x"}}), float64(7), codeInvalidParams)
	wantError(t, h.request(8, "tools/list", map[string]any{"_meta": map[string]any{metaProtocolVersion: 20260728,
		metaClientCapabilities: map[string]any{}}}), float64(8), codeInvalidParams)
	wantError(t, h.request(9, "tools/list", map[string]any{"_meta": "x"}), float64(9), codeInvalidParams)
	// removed in 2026-07-28
	wantError(t, h.request(10, "ping", modernParams(nil, nil)), float64(10), codeMethodNotFound)
	wantError(t, h.request(11, "logging/setLevel", modernParams(map[string]any{"level": "info"}, nil)), float64(11),
		codeMethodNotFound)
	wantError(t, h.request(12, "initialize", modernParams(map[string]any{"protocolVersion": "2025-11-25"}, nil)),
		float64(12), codeMethodNotFound)
	wantError(t, h.request(13, "resources/list", modernParams(nil, nil)), float64(13), codeMethodNotFound)
}

func TestRequestsWithoutVersion(t *testing.T) {
	h := newHarness(t, nil, nil)
	wantError(t, h.request(1, "tools/list", nil), float64(1), codeInvalidParams)
	wantError(t, h.request(2, "tools/call", map[string]any{"name": "list_instances"}), float64(2), codeInvalidParams)
	wantError(t, h.request(3, "server/discover", nil), float64(3), codeInvalidParams)
	// ping works before initialize
	if r := result(t, h.request(4, "ping", nil)); len(r) != 0 {
		t.Fatalf("ping %v", r)
	}
	if len(h.f.called()) != 0 {
		t.Fatalf("backend called: %v", h.f.called())
	}
	h.initialize("2025-06-18")
	// discover still needs the modern _meta after a legacy handshake
	wantError(t, h.request(5, "server/discover", nil), float64(5), codeInvalidParams)
	result(t, h.request(6, "tools/list", nil))
}

func TestJSONRPCErrors(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.initialize("2025-11-25")

	h.send(`{"jsonrpc":"2.0","id":1,"method":"ping"`) // truncated
	wantError(t, h.recv(), nil, codeParseError)
	h.send(`not json at all`)
	wantError(t, h.recv(), nil, codeParseError)
	h.send(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`) // batches were removed
	wantError(t, h.recv(), nil, codeInvalidRequest)
	h.send(`"just a string"`)
	wantError(t, h.recv(), nil, codeInvalidRequest)
	h.send(`{"jsonrpc":"2.0","id":null,"method":"ping"}`)
	wantError(t, h.recv(), nil, codeInvalidRequest)
	h.send(`{"jsonrpc":"2.0","id":true,"method":"ping"}`)
	wantError(t, h.recv(), nil, codeInvalidRequest)
	h.send(`{"jsonrpc":"2.0","id":{"a":1},"method":"ping"}`)
	wantError(t, h.recv(), nil, codeInvalidRequest)
	h.send(`{"jsonrpc":"2.0","id":1.5,"method":"ping"}`)
	wantError(t, h.recv(), nil, codeInvalidRequest)
	h.send(`{"id":2,"method":"ping"}`)
	wantError(t, h.recv(), float64(2), codeInvalidRequest)
	h.send(`{"jsonrpc":"1.0","id":3,"method":"ping"}`)
	wantError(t, h.recv(), float64(3), codeInvalidRequest)
	h.send(`{"jsonrpc":"2.0","id":4,"method":7}`)
	wantError(t, h.recv(), float64(4), codeInvalidRequest)
	h.send(`{"jsonrpc":"2.0","id":5}`)
	wantError(t, h.recv(), float64(5), codeInvalidRequest)

	wantError(t, h.request(6, "no/such/method", nil), float64(6), codeMethodNotFound)
	wantError(t, h.request(7, "resources/list", nil), float64(7), codeMethodNotFound)
	wantError(t, h.request(8, "logging/setLevel", map[string]any{"level": "info"}), float64(8), codeMethodNotFound)
	wantError(t, h.request(9, "tools/list", []any{1, 2}), float64(9), codeInvalidParams)
	wantError(t, h.request(10, "tools/list", map[string]any{"cursor": "abc"}), float64(10), codeInvalidParams)
	wantError(t, h.request(11, "tools/call", map[string]any{"name": "boot_all"}), float64(11), codeInvalidParams)
	if _, msg := errorCode(t, h.request(12, "tools/call", map[string]any{"name": "boot_all"})); msg != "Unknown tool: boot_all" {
		t.Fatalf("message %q", msg)
	}
	wantError(t, h.request(13, "tools/call", map[string]any{}), float64(13), codeInvalidParams)
	wantError(t, h.request(14, "tools/call", map[string]any{"name": "get_instance", "arguments": []any{1}}),
		float64(14), codeInvalidParams)
	wantError(t, h.request(15, "tools/call", map[string]any{"name": 5}), float64(15), codeInvalidParams)

	// Responses from the client, unknown notifications and blank lines get no answer: the next message is the
	// ping's response.
	h.send(`{"jsonrpc":"2.0","id":99,"result":{}}`)
	h.send(`{"jsonrpc":"2.0","id":98,"error":{"code":-1,"message":"x"}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/roots/list_changed"}`)
	h.send(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"list_instances"}}`) // a notification: no reply
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":12345}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":"bad"}`)
	h.send(``)
	h.send("   \r")
	if m := h.request(16, "ping", nil); m["id"] != float64(16) || m["error"] != nil {
		t.Fatalf("expected ping response, got %v", m)
	}
	// a notification never reaches the backend
	if len(h.f.called()) != 0 {
		t.Fatalf("backend called: %v", h.f.called())
	}
}

func TestCRLFAndLongLines(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.send("\xEF\xBB\xBF" + `{"jsonrpc":"2.0","id":"bom","method":"ping"}`)
	if m := h.recv(); m["id"] != "bom" || m["error"] != nil {
		t.Fatalf("BOM line: %v", m)
	}
	h.initialize("2025-11-25")
	long := strings.Repeat("x", 200<<10)
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "shell", "arguments": map[string]any{"index": 0, "command": long}}})
	if _, err := io.WriteString(h.in, string(msg)+"\r\n"); err != nil {
		t.Fatal(err)
	}
	text, isErr, sc := toolText(t, h.recv())
	if isErr {
		t.Fatalf("shell failed: %s", text)
	}
	// HTML characters are not escaped, newlines in the output are (one message per line)
	if !strings.Contains(text, `<b>&\"x\"`) || sc["output"] != "line1\nline2 <b>&\"x\"\r\n" {
		t.Fatalf("shell text %q", text)
	}
	if got := h.f.shells[0]; got != long {
		t.Fatalf("command length %d", len(got))
	}
}

// ---- tools -------------------------------------------------------------------------------------------------------

func (h *harness) call(id any, name string, arguments map[string]any, meta map[string]any) map[string]any {
	h.t.Helper()
	params := map[string]any{"name": name}
	if arguments != nil {
		params["arguments"] = arguments
	}
	return h.request(id, "tools/call", modernParams(params, meta))
}

func TestToolCalls(t *testing.T) {
	h := newHarness(t, nil, nil)
	tools := map[string]map[string]any{}
	for _, tool := range h.toolsList(true) {
		tools[tool["name"].(string)] = tool
	}
	cases := []struct {
		name string
		args map[string]any
		want string // backend call
	}{
		{"list_instances", nil, "list"},
		{"get_instance", map[string]any{"index": 4}, "info 4"},
		{"check_host", map[string]any{"memory_mb": 4096}, "check 4096"},
		{"check_host", map[string]any{}, "check 0"},
		{"create_instance", map[string]any{"title": "星铁", "index": 3, "memory_mb": 4096, "cpu": 4,
			"data_partition_gb": 128, "fps": 30}, "create"},
		{"create_instance", map[string]any{}, "create"},
		{"update_instance", map[string]any{"index": 1, "memory_mb": 5120.0}, "update 1"},
		{"delete_instance", map[string]any{"index": 2, "confirm": true}, "delete 2"},
		{"start_instance", map[string]any{"index": 1, "window": true, "timeout_s": 120}, "start 1"},
		{"start_instance", map[string]any{"index": 1}, "start 1"},
		{"stop_instance", map[string]any{"index": 1, "force": true}, "stop 1"},
		{"stop_instance", map[string]any{"index": 1}, "stop 1"},
		{"set_window_visible", map[string]any{"index": 1, "visible": false}, "window 1 false"},
		{"shell", map[string]any{"index": 1, "command": "getprop sys.boot_completed", "timeout_s": 10}, "shell 1"},
		{"install_apk", map[string]any{"index": 1, "apk_path": `D:\apk\game.apk`}, "install 1 D:\\apk\\game.apk"},
	}
	for n, c := range cases {
		tool := tools[c.name]
		var argv any = map[string]any{}
		if c.args != nil {
			argv = c.args
		}
		if err := validate(jsonRoundTrip(t, tool["inputSchema"]), jsonRoundTrip(t, argv), "args"); err != nil {
			t.Fatalf("%s: arguments rejected by its own inputSchema: %v", c.name, err)
		}
		before := len(h.f.called())
		m := h.call(n, c.name, c.args, nil)
		text, isErr, sc := toolText(t, m)
		if isErr {
			t.Fatalf("%s failed: %s", c.name, text)
		}
		if out, ok := tool["outputSchema"]; ok {
			if err := validate(out, sc, "result"); err != nil {
				t.Fatalf("%s: structuredContent does not match outputSchema: %v\n%s", c.name, err, text)
			}
		}
		calls := h.f.called()
		if len(calls) != before+1 || calls[before] != c.want {
			t.Fatalf("%s: backend calls %v, want %q", c.name, calls[before:], c.want)
		}
	}

	f := h.f
	full := f.creates[0]
	if full.Title != "星铁" || *full.Index != 3 || *full.MemoryMB != 4096 || *full.CPU != 4 || *full.DataPartitionGB != 128 ||
		*full.FPS != 30 {
		t.Fatalf("create request %+v", full)
	}
	if empty := f.creates[1]; empty.Title != "" || empty.Index != nil || empty.MemoryMB != nil || empty.CPU != nil ||
		empty.DataPartitionGB != nil || empty.FPS != nil {
		t.Fatalf("defaults must stay nil: %+v", empty)
	}
	if u := f.updates[0]; *u.MemoryMB != 5120 || u.Title != nil || u.CPU != nil || u.FPS != nil {
		t.Fatalf("update request %+v", u)
	}
	if s := f.starts[0]; !s.Window || s.Timeout != 120*time.Second || s.Progress != nil {
		t.Fatalf("start options %+v (no progressToken: no Progress)", s)
	}
	if s := f.starts[1]; s.Window || s.Timeout != 0 {
		t.Fatalf("start defaults %+v", s)
	}
	if !f.stops[0].Force || f.stops[1].Force {
		t.Fatalf("stop options %+v", f.stops)
	}
	if f.timeout != 10*time.Second {
		t.Fatalf("shell timeout %v", f.timeout)
	}
}

func TestToolErrors(t *testing.T) {
	h := newHarness(t, nil, nil)
	cases := []struct {
		name string
		args map[string]any
		code string
		text string
	}{
		{"get_instance", map[string]any{}, errs.Usage, "缺少参数 index"},
		{"get_instance", map[string]any{"index": "3"}, errs.Usage, "参数 index 必须是整数"},
		{"get_instance", map[string]any{"index": 3.5}, errs.Usage, "参数 index 必须是整数"},
		{"get_instance", map[string]any{"index": 3, "root": `D:\ModAVD`}, errs.Usage, "不认识的参数: root"},
		{"list_instances", map[string]any{"all": true}, errs.Usage, "list_instances 不接受参数"},
		{"get_instance", map[string]any{"index": 9}, errs.NotFound, "魔改 AVD 实例 9 不存在。先用 list_instances 看有哪些实例"},
		{"delete_instance", map[string]any{"index": 2, "confirm": false}, errs.Usage, "confirm 设为 true"},
		{"delete_instance", map[string]any{"index": 2}, errs.Usage, "缺少参数 confirm"},
		{"update_instance", map[string]any{"index": 2}, errs.Usage, "至少给一项"},
		{"start_instance", map[string]any{"index": 1, "window": "yes"}, errs.Usage, "必须是 true 或 false"},
		{"start_instance", map[string]any{"index": 1, "timeout_s": 0}, errs.InvalidArgument, "timeout_s"},
		{"shell", map[string]any{"index": 1, "command": "  "}, errs.InvalidArgument, "command 不能为空"},
		{"shell", map[string]any{"index": 1, "command": 5}, errs.Usage, "参数 command 必须是字符串"},
		{"shell", map[string]any{"index": 1, "command": "nil"}, errs.Internal, "工具没有返回结果"},
		{"install_apk", map[string]any{"index": 1, "apk_path": `game.apk`}, errs.InvalidArgument, "绝对路径"},
		{"check_host", map[string]any{"memory_mb": -1}, errs.InvalidArgument, "memory_mb"},
	}
	for n, c := range cases {
		text, isErr, _ := toolText(t, h.call(n, c.name, c.args, nil))
		if !isErr {
			t.Fatalf("%s %v: expected a tool error, got %s", c.name, c.args, text)
		}
		if !strings.Contains(text, c.text) || !strings.HasSuffix(text, "错误码: "+c.code) {
			t.Fatalf("%s %v: text %q, want %q and code %s", c.name, c.args, text, c.text, c.code)
		}
	}
	for _, call := range h.f.called() {
		if strings.HasPrefix(call, "delete") || strings.HasPrefix(call, "update") || strings.HasPrefix(call, "install") {
			t.Fatalf("backend reached despite bad arguments: %v", h.f.called())
		}
	}
}

func TestNewManagerError(t *testing.T) {
	h := newHarness(t, nil, func(s *server) {
		s.newBackend = func(*logx.Logger) (backend, error) {
			return nil, errs.New(errs.NotReady, "缺少组件：模拟器").WithAdvice("运行 补全组件.bat")
		}
	})
	text, isErr, _ := toolText(t, h.call(1, "list_instances", nil, nil))
	if !isErr || text != "缺少组件：模拟器。运行 补全组件.bat\n错误码: not_ready" {
		t.Fatalf("text %q", text)
	}
}

// TestServePublic drives the exported Serve (the emu.Manager adapter) with managers that cannot be built.
func TestServePublic(t *testing.T) {
	calls := 0
	opt := Options{Version: "1.2.3", NewManager: func(log *logx.Logger) (*emu.Manager, error) {
		calls++
		if calls == 1 {
			return nil, errs.New(errs.NotFound, "找不到 ModAVD 根目录")
		}
		return nil, nil
	}}
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"list_instances"}}`,
	}, "\n") + "\n"
	inR, inW := io.Pipe()
	var out strings.Builder
	var outMu sync.Mutex
	w := writerFunc(func(p []byte) (int, error) {
		outMu.Lock()
		defer outMu.Unlock()
		return out.Write(p)
	})
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), inR, w, opt) }()
	io.WriteString(inW, in)
	waitOutput := func(n int) []string {
		deadline := time.Now().Add(5 * time.Second)
		for {
			outMu.Lock()
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			outMu.Unlock()
			if len(lines) >= n && lines[0] != "" {
				return lines
			}
			if time.Now().After(deadline) {
				t.Fatalf("output %q", lines)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	lines := waitOutput(2)
	if !strings.Contains(lines[0], `"version":"1.2.3"`) {
		t.Fatalf("initialize %s", lines[0])
	}
	if !strings.Contains(lines[1], `"isError":true`) || !strings.Contains(lines[1], `找不到 ModAVD 根目录\n错误码: not_found`) {
		t.Fatalf("tools/call %s", lines[1])
	}
	io.WriteString(inW, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_instances"}}`+"\n")
	if lines := waitOutput(3); !strings.Contains(lines[2], "实例管理器为空") {
		t.Fatalf("tools/call %s", lines[2])
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestIntegralFloatArgument(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.initialize("2025-11-25")
	h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"update_instance","arguments":{"index":1.0,"memory_mb":4096.0,"cpu":4e0}}}`)
	if text, isErr, _ := toolText(t, h.recv()); isErr {
		t.Fatalf("update failed: %s", text)
	}
	if u := h.f.updates[0]; *u.MemoryMB != 4096 || *u.CPU != 4 {
		t.Fatalf("update %+v", u)
	}
}

func TestServeNeedsManager(t *testing.T) {
	err := Serve(context.Background(), strings.NewReader(""), io.Discard, Options{})
	if errs.CodeOf(err) != errs.Internal {
		t.Fatalf("err %v", err)
	}
}

func TestPanicInTool(t *testing.T) {
	h := newHarness(t, nil, nil)
	wantError(t, h.call(1, "shell", map[string]any{"index": 1, "command": "panic"}, nil), float64(1), codeInternalError)
	// the server survives
	result(t, h.request(2, "tools/list", modernParams(nil, nil)))
}

// ---- progress, cancellation, shutdown ----------------------------------------------------------------------------

func scriptedStart(steps []emu.Progress) func(context.Context, int, emu.StartOptions) (*emu.StartResult, error) {
	return func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		for _, p := range steps {
			if opt.Progress != nil {
				opt.Progress(p)
			}
		}
		return &emu.StartResult{Instance: sampleInstance(i), Launched: true, Warnings: []string{}}, nil
	}
}

var bootSteps = []emu.Progress{
	{Step: "launch", Message: "emulator 已拉起", Done: 1, Total: 4},
	{Step: "boot", Message: "等待开机", Done: 2, Total: 4},
	{Step: "boot", Message: "等待开机，已 3 秒", Done: 2, Total: 4},
	{Step: "boot", Message: "等待开机，已 6 秒", Done: 2, Total: 4},
	{Step: "done", Message: "开机完成", Done: 4, Total: 4},
}

// progressUntilResult collects the progress notifications of one call up to its response.
func (h *harness) progressUntilResult(token any) ([]float64, map[string]any) {
	h.t.Helper()
	var values []float64
	for {
		m := h.recv()
		if m["method"] == "notifications/progress" {
			p := m["params"].(map[string]any)
			if p["progressToken"] != token {
				h.t.Fatalf("progressToken %v, want %v", p["progressToken"], token)
			}
			if p["total"] != float64(4) || p["message"] == "" {
				h.t.Fatalf("progress params %v", p)
			}
			values = append(values, p["progress"].(float64))
			continue
		}
		return values, m
	}
}

func TestStartProgress(t *testing.T) {
	f := &fake{start: scriptedStart(bootSteps)}
	h := newHarness(t, f, func(s *server) { s.progressEvery = 0 })
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": modernParams(
		map[string]any{"name": "start_instance", "arguments": map[string]any{"index": 1}},
		map[string]any{"progressToken": "boot-7"})})
	values, final := h.progressUntilResult("boot-7")
	if want := []float64{1, 2, 2.025, 2.049375, 4}; !reflect.DeepEqual(values, want) {
		t.Fatalf("progress %v, want %v", values, want)
	}
	if _, isErr, _ := toolText(t, final); isErr || final["id"] != float64(7) {
		t.Fatalf("final %v", final)
	}

	// default throttle: repeats within a step are dropped, step changes are not
	h2 := newHarness(t, &fake{start: scriptedStart(bootSteps)}, nil)
	h2.initialize("2025-11-25")
	h2.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 8, "method": "tools/call", "params": map[string]any{
		"name": "start_instance", "arguments": map[string]any{"index": 1}, "_meta": map[string]any{"progressToken": 8}}})
	values, _ = h2.progressUntilResult(float64(8))
	if want := []float64{1, 2, 4}; !reflect.DeepEqual(values, want) {
		t.Fatalf("throttled progress %v, want %v", values, want)
	}
}

func TestNextProgress(t *testing.T) {
	last, sent := 0.0, false
	var got []float64
	for _, done := range []int{0, 0, 0, 1, 1, 3, 2, 2, 3} {
		v := nextProgress(last, sent, done)
		if sent && v <= last {
			t.Fatalf("not increasing: %v after %v", v, last)
		}
		got = append(got, v)
		last, sent = v, true
	}
	// a step that goes backwards still moves forward
	if want := []float64{0, 0.025, 0.049375, 1, 1.025, 3, 3.000001, 3.000002, 3.025002}; !reflect.DeepEqual(got, want) {
		t.Fatalf("values %v, want %v", got, want)
	}
	// 300 s of boot polling at one value a second stays inside the step and keeps increasing
	last = 1
	for range 300 {
		v := nextProgress(last, true, 1)
		if v <= last || v >= 2 || math.IsNaN(v) {
			t.Fatalf("%v after %v", v, last)
		}
		last = v
	}
	// rounding stalls eventually; it still increases
	for range 2000 {
		v := nextProgress(last, true, 1)
		if v <= last {
			t.Fatalf("stalled at %v", last)
		}
		last = v
	}
}

func TestCancelStart(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan error, 1)
	f := &fake{start: func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		opt.Progress(emu.Progress{Step: "launch", Message: "emulator 已拉起", Done: 1, Total: 4})
		close(started)
		<-ctx.Done()
		opt.Progress(emu.Progress{Step: "boot", Message: "取消之后", Done: 2, Total: 4}) // must not go out
		exited <- ctx.Err()
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}}
	h := newHarness(t, f, func(s *server) { s.progressEvery = 0 })
	h.initialize("2025-11-25")
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{
		"name": "start_instance", "arguments": map[string]any{"index": 1}, "_meta": map[string]any{"progressToken": "boot-7"}}})
	if m := h.recv(); m["method"] != "notifications/progress" {
		t.Fatalf("expected progress, got %v", m)
	}
	<-started

	// the read loop is free while the instance boots
	if r := result(t, h.request(2, "tools/list", nil)); len(r["tools"].([]any)) != 11 {
		t.Fatal("tools/list during boot")
	}

	h.sendJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 7, "reason": "user aborted"}})
	select {
	case err := <-exited:
		if err != context.Canceled {
			t.Fatalf("ctx err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start was not canceled")
	}
	// nothing more for id 7: the next message is the ping's response
	if m := h.request(3, "ping", nil); m["id"] != float64(3) {
		t.Fatalf("expected the ping response, got %v", m)
	}
	h.quiet(100 * time.Millisecond)
	if !h.logs.contains("已被客户端取消：user aborted") {
		t.Fatalf("cancel not logged: %v", h.logs.lines)
	}
	// the id is free again
	f.mu.Lock()
	f.start = nil
	f.mu.Unlock()
	if _, isErr, _ := toolText(t, h.request(7, "tools/call", map[string]any{"name": "start_instance",
		"arguments": map[string]any{"index": 1}})); isErr {
		t.Fatal("reused id failed")
	}
}

func TestCancelIDsAreTyped(t *testing.T) {
	// "7" must not cancel request 7
	release := make(chan struct{})
	f := &fake{start: func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		select {
		case <-release:
			return &emu.StartResult{Instance: sampleInstance(i), Warnings: []string{}}, nil
		case <-ctx.Done():
			return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
	}}
	h := newHarness(t, f, nil)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": modernParams(
		map[string]any{"name": "start_instance", "arguments": map[string]any{"index": 1}}, nil)})
	waitCalls(t, f, 1)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": "7"}})
	h.quiet(100 * time.Millisecond)
	close(release)
	if m := h.recv(); m["id"] != float64(7) || m["result"] == nil {
		t.Fatalf("expected the result of 7, got %v", m)
	}
}

func waitCalls(t *testing.T, f *fake, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.called()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("backend calls %v, want %d", f.called(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDuplicateID(t *testing.T) {
	f := &fake{start: func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		<-ctx.Done()
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}}
	h := newHarness(t, f, nil)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": "a", "method": "tools/call", "params": modernParams(
		map[string]any{"name": "start_instance", "arguments": map[string]any{"index": 1}}, nil)})
	waitCalls(t, f, 1)
	wantError(t, h.call("a", "list_instances", nil, nil), "a", codeInvalidRequest)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": "a"}})
	h.quiet(100 * time.Millisecond)
}

func TestEOFCancelsCalls(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan error, 1)
	f := &fake{start: func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		close(started)
		<-ctx.Done()
		exited <- ctx.Err()
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}}
	h := newHarness(t, f, nil)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": modernParams(
		map[string]any{"name": "start_instance", "arguments": map[string]any{"index": 1}}, nil)})
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "subscriptions/listen", "params": modernParams(
		map[string]any{"notifications": map[string]any{"toolsListChanged": true}}, nil)})
	if m := h.recv(); m["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("expected the acknowledgement, got %v", m)
	}
	<-started
	h.in.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Serve after EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return at EOF")
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the running call was not canceled at EOF")
	}
	// no response for either request; the output is closed
	for line := range h.lines {
		t.Fatalf("output after EOF: %s", line)
	}
}

func TestServeContextCanceled(t *testing.T) {
	h := newHarness(t, nil, nil)
	result(t, h.request(1, "ping", nil))
	h.cancel()
	select {
	case err := <-h.done:
		if errs.CodeOf(err) != errs.Canceled {
			t.Fatalf("err %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
}

// TestEOFLetsWritesFinish: a delete that already started is not canceled at EOF, and Serve returns only once it is
// done (the process exits right after Serve, which would cut it half way); a start running beside it is canceled.
func TestEOFLetsWritesFinish(t *testing.T) {
	deleting := make(chan struct{})
	release := make(chan struct{})
	deleteCtx := make(chan error, 1)
	startCtx := make(chan error, 1)
	f := &fake{
		del: func(ctx context.Context, i int) (*emu.DeleteResult, error) {
			close(deleting)
			<-release
			deleteCtx <- ctx.Err()
			return &emu.DeleteResult{Index: i}, nil
		},
		start: func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
			<-ctx.Done()
			startCtx <- ctx.Err()
			return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		},
	}
	h := newHarness(t, f, nil)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": modernParams(
		map[string]any{"name": "start_instance", "arguments": map[string]any{"index": 1}}, nil)})
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": modernParams(
		map[string]any{"name": "delete_instance", "arguments": map[string]any{"index": 2, "confirm": true}}, nil)})
	<-deleting
	waitCalls(t, f, 2)
	h.in.Close()
	select {
	case err := <-startCtx:
		if err != context.Canceled {
			t.Fatalf("start ctx %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start was not canceled at EOF")
	}
	select {
	case err := <-h.done:
		t.Fatalf("Serve returned (%v) while the delete was still running", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-deleteCtx; err != nil {
		t.Fatalf("the delete was canceled: %v", err)
	}
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Serve after EOF: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return once the delete was done")
	}
	// the client is gone: no result for either
	for line := range h.lines {
		t.Fatalf("output after EOF: %s", line)
	}
	if !h.logs.contains("等 1 个正在写文件的请求做完再退出") || !h.logs.contains("取消 1 个还没完成的请求") {
		t.Fatalf("shutdown not logged: %v", h.logs.lines)
	}
}

// TestShutdownWaitIsBounded: a write that does not finish in time is canceled and Serve returns anyway.
func TestShutdownWaitIsBounded(t *testing.T) {
	deleting := make(chan struct{})
	f := &fake{del: func(ctx context.Context, i int) (*emu.DeleteResult, error) {
		close(deleting)
		<-ctx.Done()
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}}
	h := newHarness(t, f, func(s *server) { s.shutdownWait = 100 * time.Millisecond })
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": modernParams(
		map[string]any{"name": "delete_instance", "arguments": map[string]any{"index": 2, "confirm": true}}, nil)})
	<-deleting
	started := time.Now()
	h.cancel() // the server's own context ends (Ctrl+C on modavd mcp): same shutdown
	select {
	case err := <-h.done:
		if errs.CodeOf(err) != errs.Canceled {
			t.Fatalf("err %v", err)
		}
		if d := time.Since(started); d < 100*time.Millisecond || d > 3*time.Second {
			t.Fatalf("Serve returned after %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	if !h.logs.contains("秒内没做完，取消它们后退出") {
		t.Fatalf("timeout not logged: %v", h.logs.lines)
	}
}

// TestWaitsForManagerAfterCall: each call's manager is waited for after its result, and Serve waits for that too.
func TestWaitsForManagerAfterCall(t *testing.T) {
	waiting := make(chan struct{}, 1)
	release := make(chan struct{})
	f := &fake{}
	h := newHarness(t, f, func(s *server) { s.hookWait = 7 * time.Second })
	if _, isErr, _ := toolText(t, h.call(1, "list_instances", nil, nil)); isErr {
		t.Fatal("list failed")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(f.waitCalls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := f.waitCalls(); len(got) != 1 || got[0] != 7*time.Second {
		t.Fatalf("waits %v", got)
	}
	if hookWait != 10*time.Second || shutdownWait != 30*time.Second {
		t.Fatalf("bounds %v %v", hookWait, shutdownWait)
	}
	if _, isErr, _ := toolText(t, h.call(9, "get_instance", map[string]any{}, nil)); !isErr {
		t.Fatal("get_instance without index succeeded")
	}

	// The result is not held back by the wait; EOF is.
	f.mu.Lock()
	f.wait = func(time.Duration) {
		waiting <- struct{}{}
		<-release
	}
	f.mu.Unlock()
	if _, isErr, _ := toolText(t, h.call(2, "get_instance", map[string]any{"index": 3}, nil)); isErr {
		t.Fatal("get_instance failed")
	}
	<-waiting
	h.in.Close()
	select {
	case err := <-h.done:
		t.Fatalf("Serve returned (%v) during the manager's background work", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return")
	}
	// argument errors build no manager: nothing to wait for
	if n := len(f.waitCalls()); n != 2 {
		t.Fatalf("%d waits", n)
	}
}

func TestKeptTools(t *testing.T) {
	for _, tool := range toolDefs() {
		want := tool.Name == "create_instance" || tool.Name == "update_instance" || tool.Name == "delete_instance" ||
			tool.Name == "install_apk"
		if tool.keep != want {
			t.Errorf("%s: keep %v", tool.Name, tool.keep)
		}
	}
}

func TestStartDescriptionTellsWhatCancelDoes(t *testing.T) {
	for _, tool := range toolDefs() {
		if tool.Name != "start_instance" {
			continue
		}
		for _, s := range []string{"Cancelling the call or reaching timeout_s only stops waiting",
			"not killed and keeps booting", "Calling start_instance again adopts it and finishes the setup of that boot",
			"guest Vulkan fix", "setup_pending=true"} {
			if !strings.Contains(tool.Description, s) {
				t.Errorf("description lacks %q: %s", s, tool.Description)
			}
		}
		return
	}
	t.Fatal("no start_instance")
}

func TestSubscriptionsListen(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "id": "sub-1", "method": "subscriptions/listen", "params": modernParams(
		map[string]any{"notifications": map[string]any{"toolsListChanged": true}}, nil)})
	ack := h.recv()
	if ack["method"] != "notifications/subscriptions/acknowledged" || ack["id"] != nil {
		t.Fatalf("ack %v", ack)
	}
	p := ack["params"].(map[string]any)
	if p["_meta"].(map[string]any)[metaSubscriptionID] != "sub-1" || len(p["notifications"].(map[string]any)) != 0 {
		t.Fatalf("ack params %v", p)
	}
	// held open: no response
	h.quiet(100 * time.Millisecond)
	h.sendJSON(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": "sub-1"}})
	if m := h.request(2, "tools/list", modernParams(nil, nil)); m["id"] != float64(2) {
		t.Fatalf("expected tools/list response, got %v", m)
	}
	// legacy clients have no subscriptions/listen
	h.initialize("2025-11-25")
	wantError(t, h.request(3, "subscriptions/listen", nil), float64(3), codeInvalidParams)
}

// ---- definitions -------------------------------------------------------------------------------------------------

func TestToolDefinitions(t *testing.T) {
	h := newHarness(t, nil, nil)
	h.initialize("2025-11-25")
	tools := h.toolsList(false)
	again := h.toolsList(true)
	if !reflect.DeepEqual(tools, again) {
		t.Fatal("tool list differs between eras / calls")
	}
	want := []string{"list_instances", "get_instance", "check_host", "create_instance", "update_instance",
		"delete_instance", "start_instance", "stop_instance", "set_window_visible", "shell", "install_apk"}
	if len(tools) != len(want) {
		t.Fatalf("%d tools", len(tools))
	}
	for k, tool := range tools {
		name := tool["name"].(string)
		if name != want[k] {
			t.Fatalf("tool %d is %s, want %s", k, name, want[k])
		}
		if desc, _ := tool["description"].(string); len(desc) < 40 || len(desc) > 1000 {
			t.Fatalf("%s: description length %d", name, len(desc))
		}
		if tool["title"] == "" {
			t.Fatalf("%s: no title", name)
		}
		in := tool["inputSchema"].(map[string]any)
		if in["type"] != "object" || in["additionalProperties"] != false || in["properties"] == nil {
			t.Fatalf("%s: inputSchema %v", name, in)
		}
		for prop := range in["properties"].(map[string]any) {
			if len(prop) == 0 || len(prop) > 64 || strings.Trim(prop, "abcdefghijklmnopqrstuvwxyz_") != "" {
				t.Fatalf("%s: property name %q", name, prop)
			}
		}
		ann := tool["annotations"].(map[string]any)
		readOnly := ann["readOnlyHint"] == true
		if ann["openWorldHint"] != false {
			t.Fatalf("%s: openWorldHint %v", name, ann["openWorldHint"])
		}
		if readOnly {
			if _, ok := tool["outputSchema"]; !ok {
				t.Fatalf("%s: read-only tool without outputSchema", name)
			}
		} else if _, ok := ann["destructiveHint"].(bool); !ok {
			t.Fatalf("%s: no destructiveHint", name)
		}
		if out, ok := tool["outputSchema"].(map[string]any); ok && out["type"] != "object" {
			t.Fatalf("%s: outputSchema root %v", name, out["type"])
		}
		interaction := false
		if meta, ok := tool["_meta"].(map[string]any); ok {
			interaction = meta["anthropic/requiresUserInteraction"] == true
		}
		if interaction != (name == "delete_instance") {
			t.Fatalf("%s: requiresUserInteraction %v", name, interaction)
		}
	}
	byName := map[string]map[string]any{}
	for _, tool := range tools {
		byName[tool["name"].(string)] = tool
	}
	for name, hints := range map[string][3]any{ // readOnly, destructive, idempotent
		"list_instances":  {true, nil, nil},
		"create_instance": {false, false, false},
		"update_instance": {false, true, true},
		"delete_instance": {false, true, true},
		"start_instance":  {false, false, true},
		"stop_instance":   {false, true, true},
	} {
		ann := byName[name]["annotations"].(map[string]any)
		if ann["readOnlyHint"] != hints[0] || ann["destructiveHint"] != hints[1] || ann["idempotentHint"] != hints[2] {
			t.Fatalf("%s annotations %v", name, ann)
		}
	}
	del := byName["delete_instance"]["inputSchema"].(map[string]any)
	if !reflect.DeepEqual(del["required"], []any{"index", "confirm"}) {
		t.Fatalf("delete required %v", del["required"])
	}
}

type schemaProbe struct {
	emu.RootInfo
	A      string         `json:"a"`
	B      int            `json:"b,omitempty"`
	C      *bool          `json:"c"`
	D      []string       `json:"d"`
	E      map[string]any `json:"e"`
	F      float64        `json:"f,omitzero"`
	G      string         `json:"-"`
	h      int
	Nested struct {
		X int `json:"x"`
	} `json:"nested"`
}

func TestSchemaOf(t *testing.T) {
	_ = schemaProbe{}.h
	p := jsonRoundTrip(t, schemaOf(reflect.TypeFor[schemaProbe]())).(map[string]any)
	pp := p["properties"].(map[string]any)
	if !reflect.DeepEqual(p["required"], []any{"root", "sdk", "local_sdk", "adb_exe", "adb_server_port",
		"script_adb_server_port", "version", "build", "a", "c", "d", "e", "nested"}) {
		t.Fatalf("required %v", p["required"])
	}
	for _, k := range []string{"G", "h", "RootInfo"} {
		if _, ok := pp[k]; ok {
			t.Fatalf("property %s should not be there", k)
		}
	}
	for k, want := range map[string]any{"a": "string", "b": "integer", "c": []any{"boolean", "null"},
		"d": []any{"array", "null"}, "e": []any{"object", "null"}, "f": "number", "nested": "object"} {
		if got := pp[k].(map[string]any)["type"]; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: type %v, want %v", k, got, want)
		}
	}
	if pp["d"].(map[string]any)["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("d items %v", pp["d"])
	}

	s := jsonRoundTrip(t, schemaOf(reflect.TypeFor[emu.ListResult]())).(map[string]any)
	props := s["properties"].(map[string]any)
	// RootInfo is embedded: its fields are promoted
	for _, k := range []string{"root", "sdk", "adb_server_port", "instances"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("missing property %s in %v", k, props)
		}
	}
	inst := props["instances"].(map[string]any)["items"].(map[string]any)
	ip := inst["properties"].(map[string]any)
	if ip["pid"].(map[string]any)["type"] != "integer" || ip["ports"].(map[string]any)["type"] != "object" {
		t.Fatalf("instance properties %v", ip)
	}
	check := jsonRoundTrip(t, schemaOf(reflect.TypeFor[emu.CheckResult]())).(map[string]any)
	ok := check["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["ok"]
	if !reflect.DeepEqual(ok.(map[string]any)["type"], []any{"boolean", "null"}) {
		t.Fatalf("ok schema %v", ok)
	}
}

func TestIDKey(t *testing.T) {
	for raw, want := range map[string]string{`7`: "n:7", `"7"`: "s:7", `"\u0037"`: "s:7", `-3`: "n:-3", ` 12 `: "n:12"} {
		if got, ok := idKey(json.RawMessage(raw)); !ok || got != want {
			t.Errorf("idKey(%s) = %q %v, want %q", raw, got, ok, want)
		}
	}
	for _, raw := range []string{`null`, `true`, `1.5`, `1e3`, `{}`, `[]`, ``} {
		if _, ok := idKey(json.RawMessage(raw)); ok {
			t.Errorf("idKey(%s) accepted", raw)
		}
	}
}

// ---- a small JSON Schema checker (type / required / properties / items / enum / const / additionalProperties) ------

func jsonRoundTrip(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func validate(schema, v any, path string) error {
	s, _ := schema.(map[string]any)
	if s == nil {
		return nil
	}
	if typ, ok := s["type"]; ok {
		var types []string
		switch x := typ.(type) {
		case string:
			types = []string{x}
		case []any:
			for _, y := range x {
				types = append(types, y.(string))
			}
		}
		matched := false
		for _, ty := range types {
			if typeMatches(ty, v) {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("%s: %v (%T) is not %v", path, v, v, types)
		}
	}
	if c, ok := s["const"]; ok && !reflect.DeepEqual(c, v) {
		return fmt.Errorf("%s: %v is not const %v", path, v, c)
	}
	if enum, ok := s["enum"].([]any); ok {
		found := false
		for _, e := range enum {
			if reflect.DeepEqual(e, v) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%s: %v not in %v", path, v, enum)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if _, ok := x[r.(string)]; !ok {
					return fmt.Errorf("%s: missing required %s", path, r)
				}
			}
		}
		for k, pv := range x {
			if ps, ok := props[k]; ok {
				if err := validate(ps, pv, path+"."+k); err != nil {
					return err
				}
			} else if s["additionalProperties"] == false {
				return fmt.Errorf("%s: additional property %s", path, k)
			}
		}
	case []any:
		for i, item := range x {
			if err := validate(s["items"], item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func typeMatches(ty string, v any) bool {
	switch ty {
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "null":
		return v == nil
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == math.Trunc(f)
	}
	return false
}
