//go:build windows

// Package mcp is the Model Context Protocol server of modavd (`modavd mcp`): JSON-RPC 2.0 over stdio, one tool per
// basic operation of emu.Manager.
//
// Hand-rolled on the standard library: stdio + tools is about ten methods, and the official Go SDK would pull HTTP /
// OAuth modules into the build. Both protocol eras are served by the same process. A request whose
// params._meta carries io.modelcontextprotocol/protocolVersion is handled statelessly as 2026-07-28 (server/discover,
// resultType, ttlMs / cacheScope, serverInfo in the result _meta); initialize negotiates 2025-11-25 / 2025-06-18 for
// the requests without it. Framing: one JSON message per line; out carries protocol messages only, diagnostics go to
// Options.Log (stderr). The logging capability is not offered (deprecated in 2026-07-28).
//
// Each tools/call (and subscriptions/listen) runs in its own goroutine with its own context, so the read loop keeps
// taking cancellations and other requests while an instance boots. The other methods are instant and answered in
// the read loop, which also keeps initialize ordered before the requests after it. notifications/cancelled cancels
// that context and suppresses everything still to be sent for the request. Instances are never stopped by the server:
// cancelling start_instance only stops waiting (killing qemu would lose guest data).
//
// Shutdown, at EOF on in or when ctx ends: nothing more is written, and every call is canceled (start / stop just stop
// waiting) except the tools that write files without stopping half way (create / update / delete_instance,
// install_apk). The process exits right after Serve returns, so those get up to shutdownWait (30 s) to finish first:
// an avd.Delete cut in the middle leaves an instance directory without config.ini that modavd no longer lists nor
// deletes, an update cut between config.ini and mas-avd.json leaves the two out of step. Within the same bound Serve
// waits for the canceled calls to return and for their managers' background work (backend.Wait, at most hookWait each
// after the result). What is still running after that is canceled, given shutdownGrace, and then cut by the exit. A
// client that kills the server instead of closing stdin (or kills its whole process tree) gets no such guarantee.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
)

type Options struct {
	// NewManager builds the manager for one tool call. log carries that call's progress messages; the server writes
	// them to Log (they are not sent to the client). Called per call, so a root fixed later (or changed files) is
	// picked up.
	NewManager func(log *logx.Logger) (*emu.Manager, error)
	// Version is serverInfo.version; "" means buildinfo.Version.
	Version string
	// Log is the server's own diagnostics (stderr). Never stdout: stdout carries the protocol only.
	Log *logx.Logger
}

// instructions is handed to the model by the client (initialize / server/discover).
var instructions = fmt.Sprintf("Manages the ModAVD (patched Android Emulator) instances of one root directory, which "+
	"is fixed by the server's --root; instances are numbered 0-%d except %d. Always shut instances down with "+
	"stop_instance, which syncs the guest disk first: never kill emulator or qemu processes, that loses recently "+
	"written game data. start_instance can take minutes and reports progress; delete_instance permanently removes "+
	"an instance with all its game data.", root.MaxIndex, root.ReservedIndex)

// progressEvery throttles progress notifications that do not complete a step.
const progressEvery = time.Second

// Shutdown bounds (see the package comment): how long Serve waits for the in-flight calls, how long a call waits
// for its manager's background work after its result, and how long a call still running at the end gets to unwind
// after its cancellation.
const (
	shutdownWait  = 30 * time.Second
	hookWait      = 10 * time.Second
	shutdownGrace = 2 * time.Second
)

// Serve reads requests from in and writes responses / notifications to out until in reaches EOF (returns nil) or
// ctx ends (errs.Canceled). Either way it returns only after the file-writing calls in flight have finished, at most
// about shutdownWait later (see the package comment).
func Serve(ctx context.Context, in io.Reader, out io.Writer, opt Options) error {
	if opt.NewManager == nil {
		return errs.New(errs.Internal, "MCP 服务没有设置实例管理器（NewManager）")
	}
	newBackend := func(log *logx.Logger) (backend, error) {
		m, err := opt.NewManager(log)
		if err != nil {
			return nil, err
		}
		if m == nil {
			return nil, errs.New(errs.Internal, "实例管理器为空")
		}
		return m, nil
	}
	return newServer(opt, newBackend).serve(ctx, in, out)
}

type server struct {
	newBackend    func(log *logx.Logger) (backend, error)
	log           *logx.Logger
	info          implementation
	tools         []*toolDef
	byName        map[string]*toolDef
	progressEvery time.Duration
	shutdownWait  time.Duration
	hookWait      time.Duration

	w      io.Writer
	wmu    sync.Mutex
	closed atomic.Bool // set at shutdown: nothing is written after it
	wfail  bool        // a write failed (logged once)

	base context.Context // parent of the call contexts, canceled when the shutdown begins
	// keep is the parent of the calls of tools that write files (toolDef.keep): canceled only when shutdownWait is
	// over, so those finish instead of being cut by the process exit.
	keep context.Context
	// inflight counts the tools/call goroutines until each has returned and waited for its manager's background
	// work. Added to in the read loop, which is also where the shutdown waits, so no Add races that Wait.
	inflight sync.WaitGroup

	mu     sync.Mutex
	calls  map[string]*call // in-flight tools/call and subscriptions/listen by idKey
	legacy string           // revision negotiated by initialize, "" before
}

// call is one in-flight request. mu orders "cancelled" against every write for it: once cancelled is set, nothing
// more goes out for this id; once finished, no more progress.
type call struct {
	id     json.RawMessage
	key    string
	ctx    context.Context
	cancel context.CancelFunc
	token  json.RawMessage // progressToken; nil when the request has none
	keep   bool            // a file-writing tool: not canceled when the shutdown begins

	mu        sync.Mutex
	cancelled bool
	finished  bool
}

type request struct {
	id     json.RawMessage
	key    string
	method string
	params map[string]json.RawMessage // nil when absent
	meta   map[string]json.RawMessage // params._meta
	modern bool                       // 2026-07-28 request
}

func newServer(opt Options, newBackend func(log *logx.Logger) (backend, error)) *server {
	version := opt.Version
	if version == "" {
		version = buildinfo.Version
	}
	s := &server{
		newBackend:    newBackend,
		log:           opt.Log,
		info:          implementation{Name: "modavd", Title: "ModAVD", Version: version},
		tools:         toolDefs(),
		byName:        map[string]*toolDef{},
		progressEvery: progressEvery,
		shutdownWait:  shutdownWait,
		hookWait:      hookWait,
		calls:         map[string]*call{},
	}
	for _, t := range s.tools {
		s.byName[t.Name] = t
	}
	return s
}

func (s *server) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.w = out
	// Not derived from ctx: shutdown first closes the output, then cancels, so a call that ends because of the
	// cancellation cannot slip its result out in between.
	base, cancelAll := context.WithCancel(context.Background())
	defer cancelAll()
	keep, cancelKeep := context.WithCancel(context.Background())
	defer cancelKeep()
	s.base, s.keep = base, keep

	lines := make(chan []byte)
	readErr := make(chan error, 1)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		// ReadBytes has no line limit (bufio.Scanner would cut messages over 64 KiB).
		br := bufio.NewReaderSize(in, 64<<10)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				select {
				case lines <- line:
				case <-stop:
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	s.log.Infof("MCP 服务已启动（modavd %s），等待客户端请求", s.info.Version)
	for {
		select {
		case line := <-lines:
			s.handleLine(line)
		case err := <-readErr:
			if err == io.EOF {
				s.log.Infof("客户端已关闭标准输入，MCP 服务退出")
			}
			s.shutdown(cancelAll, cancelKeep)
			if err == io.EOF {
				return nil
			}
			return errs.Wrap(errs.Internal, err, "读取标准输入失败")
		case <-ctx.Done():
			s.log.Infof("MCP 服务被中止")
			s.shutdown(cancelAll, cancelKeep)
			return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
	}
}

// shutdown: no more output; every in-flight call is canceled except the file-writing ones, which get up to
// shutdownWait to finish, as the process exits right after Serve (see the package comment). Within the same bound it
// waits for the canceled calls to return and for their managers' background work. The instances keep running.
func (s *server) shutdown(cancelAll, cancelKeep context.CancelFunc) {
	s.closed.Store(true)
	s.mu.Lock()
	canceled, kept := 0, 0
	for _, c := range s.calls {
		if c.keep {
			kept++
		} else {
			canceled++
		}
	}
	s.calls = map[string]*call{}
	s.mu.Unlock()
	if canceled > 0 {
		s.log.Infof("取消 %d 个还没完成的请求", canceled)
	}
	if kept > 0 {
		s.log.Infof("等 %d 个正在写文件的请求做完再退出（最多 %.0f 秒）", kept, s.shutdownWait.Seconds())
	}
	cancelAll()

	done := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(done)
	}()
	t := time.NewTimer(s.shutdownWait)
	defer t.Stop()
	select {
	case <-done:
		cancelKeep()
		return
	case <-t.C:
	}
	s.log.Warnf("还有请求 %.0f 秒内没做完，取消它们后退出", s.shutdownWait.Seconds())
	cancelKeep()
	select {
	case <-done:
	case <-time.After(shutdownGrace):
	}
}

// ---- reading -----------------------------------------------------------------------------------------------------

func (s *server) handleLine(line []byte) {
	// Windows PowerShell 5.1 pipes put a UTF-8 BOM in front of the first line.
	line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("\xEF\xBB\xBF")))
	if len(line) == 0 {
		return
	}
	if !json.Valid(line) {
		s.log.Warnf("收到的消息不是合法 JSON，已回复解析错误: %s", clip(line))
		s.sendError(nil, codeParseError, "Parse error", nil)
		return
	}
	var msg map[string]json.RawMessage
	if line[0] != '{' || json.Unmarshal(line, &msg) != nil {
		// JSON-RPC batches were removed in 2025-06-18.
		s.sendError(nil, codeInvalidRequest, "Invalid Request: expected one JSON-RPC object per line", nil)
		return
	}
	rawID, hasID := msg["id"]
	key, idOK := "", false
	if hasID {
		key, idOK = idKey(rawID)
	}
	var replyID json.RawMessage
	if idOK {
		replyID = rawID
	}
	rawMethod, hasMethod := msg["method"]
	if !hasMethod {
		_, isResult := msg["result"]
		_, isError := msg["error"]
		if isResult || isError {
			return // a response from the client; this server never sends requests
		}
		s.sendError(replyID, codeInvalidRequest, "Invalid Request: method is missing", nil)
		return
	}
	var version, method string
	if json.Unmarshal(msg["jsonrpc"], &version) != nil || version != "2.0" {
		s.sendError(replyID, codeInvalidRequest, `Invalid Request: jsonrpc must be "2.0"`, nil)
		return
	}
	if json.Unmarshal(rawMethod, &method) != nil {
		s.sendError(replyID, codeInvalidRequest, "Invalid Request: method must be a string", nil)
		return
	}
	if !hasID {
		s.handleNotification(method, msg["params"])
		return
	}
	if !idOK {
		s.sendError(nil, codeInvalidRequest, "Invalid Request: id must be a string or an integer", nil)
		return
	}
	s.handleRequest(&request{id: rawID, key: key, method: method}, msg["params"])
}

func (s *server) handleNotification(method string, rawParams json.RawMessage) {
	switch method {
	case "notifications/cancelled":
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
			Reason    string          `json:"reason"`
		}
		if json.Unmarshal(rawParams, &p) != nil {
			return
		}
		// Unknown, finished or malformed ids are ignored: the result may already be on its way.
		if key, ok := idKey(p.RequestID); ok {
			s.cancelCall(key, p.Reason)
		}
	case "notifications/initialized":
		s.log.Debugf("客户端已完成初始化")
	default:
		// other notifications (roots/list_changed ...) are of no interest
	}
}

func (s *server) handleRequest(r *request, rawParams json.RawMessage) {
	if !isNull(rawParams) {
		if !isObject(rawParams) || json.Unmarshal(rawParams, &r.params) != nil {
			s.sendError(r.id, codeInvalidParams, "Invalid params: params must be an object", nil)
			return
		}
	}
	if m, ok := r.params["_meta"]; ok && !isNull(m) {
		if !isObject(m) || json.Unmarshal(m, &r.meta) != nil {
			s.sendError(r.id, codeInvalidParams, "Invalid params: _meta must be an object", nil)
			return
		}
	}
	if pv, ok := r.meta[metaProtocolVersion]; ok {
		s.handleModern(r, pv)
		return
	}
	s.handleLegacy(r)
}

// handleModern: a 2026-07-28 request. Version first: an unknown revision may shape the rest differently.
func (s *server) handleModern(r *request, rawVersion json.RawMessage) {
	var v string
	if json.Unmarshal(rawVersion, &v) != nil {
		s.sendError(r.id, codeInvalidParams, "Invalid params: _meta "+metaProtocolVersion+" must be a string", nil)
		return
	}
	if !slices.Contains(modernVersions, v) {
		s.log.Warnf("客户端请求的协议版本 %s 不支持（支持 %s）", v, strings.Join(modernVersions, ", "))
		s.sendError(r.id, codeUnsupportedProtocolVersion, "Unsupported protocol version",
			map[string]any{"supported": modernVersions, "requested": v})
		return
	}
	if cc, ok := r.meta[metaClientCapabilities]; !ok || !isObject(cc) {
		s.sendError(r.id, codeInvalidParams, "Invalid params: _meta "+metaClientCapabilities+" is required", nil)
		return
	}
	r.modern = true
	switch r.method {
	case "server/discover":
		s.reply(r, discoverResult{
			ResultType:        resultComplete,
			SupportedVersions: modernVersions,
			Instructions:      instructions,
			TTLMs:             listTTLMs,
			CacheScope:        cacheScope,
			Meta:              s.resultMeta(),
		})
	case "tools/list":
		s.toolsList(r)
	case "tools/call":
		s.toolsCall(r)
	case "subscriptions/listen":
		s.listen(r)
	default:
		// includes ping and logging/setLevel, which 2026-07-28 removed
		s.methodNotFound(r)
	}
}

func (s *server) handleLegacy(r *request) {
	switch r.method {
	case "initialize":
		s.initialize(r)
	case "ping":
		// allowed before initialize too
		s.reply(r, struct{}{})
	case "tools/list", "tools/call":
		if s.negotiated() == "" {
			s.sendError(r.id, codeInvalidParams, noVersionMessage, nil)
			return
		}
		if r.method == "tools/list" {
			s.toolsList(r)
		} else {
			s.toolsCall(r)
		}
	case "server/discover", "subscriptions/listen":
		s.sendError(r.id, codeInvalidParams, noVersionMessage, nil)
	default:
		s.methodNotFound(r)
	}
}

var noVersionMessage = fmt.Sprintf("Invalid params: no protocol version: set params._meta[%q] (supported: %s) or send "+
	"initialize first (supported: %s)", metaProtocolVersion, strings.Join(modernVersions, ", "),
	strings.Join(legacyVersions, ", "))

func (s *server) negotiated() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.legacy
}

// initialize: a supported revision is echoed, anything else gets our newest one and the client decides (no error
// for a mismatch). Handled in the read loop, so later requests see the result.
func (s *server) initialize(r *request) {
	var requested string
	if json.Unmarshal(r.params["protocolVersion"], &requested) != nil {
		s.sendError(r.id, codeInvalidParams, "Invalid params: protocolVersion is required", nil)
		return
	}
	version := legacyVersions[0]
	if slices.Contains(legacyVersions, requested) {
		version = requested
	}
	var client struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	json.Unmarshal(r.params["clientInfo"], &client)
	s.mu.Lock()
	s.legacy = version
	s.mu.Unlock()
	s.log.Infof("客户端 %s %s 已连接，协议版本 %s（客户端请求 %s）", client.Name, client.Version, version, requested)
	s.reply(r, initializeResult{
		ProtocolVersion: version,
		ServerInfo:      s.info,
		Instructions:    instructions,
	})
}

func (s *server) toolsList(r *request) {
	// We never hand out a cursor, so any cursor is invalid.
	if c, ok := r.params["cursor"]; ok && !isNull(c) {
		s.sendError(r.id, codeInvalidParams, "Invalid params: invalid cursor", nil)
		return
	}
	res := listToolsResult{Tools: s.tools}
	if r.modern {
		res.ResultType, res.TTLMs, res.CacheScope, res.Meta = resultComplete, listTTLMs, cacheScope, s.resultMeta()
	}
	s.reply(r, res)
}

// toolsCall validates the request structure here (protocol errors), then runs the tool in its own goroutine.
// Argument problems are tool errors (isError results), so the model can correct them.
func (s *server) toolsCall(r *request) {
	var name string
	if json.Unmarshal(r.params["name"], &name) != nil || name == "" {
		s.sendError(r.id, codeInvalidParams, "Invalid params: name is required", nil)
		return
	}
	a := args{}
	if raw, ok := r.params["arguments"]; ok && !isNull(raw) {
		if !isObject(raw) || json.Unmarshal(raw, &a) != nil {
			s.sendError(r.id, codeInvalidParams, "Invalid params: arguments must be an object", nil)
			return
		}
	}
	t, ok := s.byName[name]
	if !ok {
		s.sendError(r.id, codeInvalidParams, "Unknown tool: "+name, nil)
		return
	}
	c := s.register(r, t.keep)
	if c == nil {
		return
	}
	c.token = progressTokenOf(r.meta[metaProgressToken])
	s.inflight.Add(1)
	go s.runTool(c, t, a, r.modern)
}

// listen (2026-07-28 subscriptions/listen): acknowledged with an empty filter, as no notification is ever sent,
// then held open until the client cancels it or goes away. Never answered.
func (s *server) listen(r *request) {
	c := s.register(r, false)
	if c == nil {
		return
	}
	s.sendFor(c, notification{JSONRPC: "2.0", Method: "notifications/subscriptions/acknowledged",
		Params: map[string]any{
			"_meta":         map[string]any{metaSubscriptionID: r.id},
			"notifications": struct{}{},
		}})
	go func() {
		<-c.ctx.Done()
		s.forget(c)
	}()
}

func (s *server) methodNotFound(r *request) {
	s.sendError(r.id, codeMethodNotFound, "Method not found: "+r.method, nil)
}

func (s *server) resultMeta() map[string]any {
	return map[string]any{metaServerInfo: s.info}
}

// ---- in-flight calls ---------------------------------------------------------------------------------------------

// register adds r to the in-flight table; nil (after answering the request) when its id is already in flight. keep:
// a file-writing tool, whose context outlives the start of the shutdown.
func (s *server) register(r *request, keep bool) *call {
	s.mu.Lock()
	if _, dup := s.calls[r.key]; dup {
		s.mu.Unlock()
		s.log.Warnf("请求 id %s 还在处理中又收到同一个 id，已拒绝", r.id)
		s.sendError(r.id, codeInvalidRequest, "Invalid Request: id is already in use by a request in progress", nil)
		return nil
	}
	parent := s.base
	if keep {
		parent = s.keep
	}
	ctx, cancel := context.WithCancel(parent)
	c := &call{id: r.id, key: r.key, ctx: ctx, cancel: cancel, keep: keep}
	s.calls[r.key] = c
	s.mu.Unlock()
	return c
}

// forget removes c from the table (its id may be reused from then on).
func (s *server) forget(c *call) {
	s.mu.Lock()
	if s.calls[c.key] == c {
		delete(s.calls, c.key)
	}
	s.mu.Unlock()
}

func (s *server) cancelCall(key, reason string) {
	s.mu.Lock()
	c := s.calls[key]
	delete(s.calls, key)
	s.mu.Unlock()
	if c == nil {
		return
	}
	c.mu.Lock()
	c.cancelled = true
	c.mu.Unlock()
	c.cancel()
	if reason != "" {
		reason = "：" + reason
	}
	s.log.Infof("请求 %s 已被客户端取消%s", c.id, reason)
}

// finish sends the final message of c unless it was canceled. The id is released before the write, so a client
// that reuses it right after reading the response is not refused.
func (s *server) finish(c *call, msg any) {
	s.forget(c)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelled || c.finished {
		return
	}
	c.finished = true
	s.write(msg)
}

// sendFor sends an intermediate message (progress, acknowledgement) of c; false when c is canceled or finished.
func (s *server) sendFor(c *call, msg any) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancelled || c.finished {
		return false
	}
	return s.write(msg)
}

func (s *server) runTool(c *call, t *toolDef, a args, modern bool) {
	defer s.inflight.Done()
	defer c.cancel()
	defer func() {
		if p := recover(); p != nil {
			s.log.Errorf("工具 %s 内部错误: %v\n%s", t.Name, p, debug.Stack())
			s.finish(c, response{JSONRPC: "2.0", ID: c.id,
				Error: &rpcError{Code: codeInternalError, Message: fmt.Sprintf("Internal error: %v", p)}})
		}
	}()
	started := time.Now()
	s.log.Infof("调用工具 %s（请求 %s）", t.Name, c.id)
	data, b, err := s.execTool(c, t, a)
	res := callToolResult{}
	if err != nil {
		e := errs.As(err)
		if c.ctx.Err() != nil {
			s.log.Infof("工具 %s 已取消（请求 %s）", t.Name, c.id)
		} else {
			s.log.Warnf("工具 %s 失败（%s）: %s", t.Name, e.Code, e.Error())
		}
		res.Content = []textContent{{Type: "text", Text: errorText(e)}}
		res.IsError = true
	} else {
		s.log.Infof("工具 %s 完成，用时 %.1f 秒", t.Name, time.Since(started).Seconds())
		res.Content = []textContent{{Type: "text", Text: string(data)}}
		res.StructuredContent = data
	}
	if modern {
		res.ResultType, res.Meta = resultComplete, s.resultMeta()
	}
	s.finish(c, response{JSONRPC: "2.0", ID: c.id, Result: res})
	if b != nil {
		// After the result, so the client is not kept waiting: the call's manager may still be restarting logcat
		// after a re-registration, and a shutdown right now must not exit under it.
		b.Wait(s.hookWait)
	}
}

// execTool runs t; the backend it built (nil when none) comes back too, for its background work.
func (s *server) execTool(c *call, t *toolDef, a args) (json.RawMessage, backend, error) {
	if err := t.checkArgs(a); err != nil {
		return nil, nil, err
	}
	prefix := fmt.Sprintf("[%s %s] ", t.Name, c.id)
	callLog := logx.New(func(level logx.Level, msg string) { s.log.Log(level, prefix+msg) })
	b, err := s.newBackend(callLog)
	if err != nil {
		return nil, nil, err
	}
	var progress func(emu.Progress)
	if c.token != nil {
		progress = s.progressFunc(c)
	}
	v, err := t.run(c.ctx, b, a, progress)
	if err != nil {
		return nil, b, err
	}
	data, err := encodeJSON(v)
	if err != nil {
		return nil, b, errs.Wrap(errs.Internal, err, "工具结果无法编码成 JSON")
	}
	if !isObject(data) {
		return nil, b, errs.New(errs.Internal, "工具没有返回结果")
	}
	return data, b, nil
}

// errorText is the text of an isError result: the Chinese message and advice, then the machine-readable code.
func errorText(e *errs.Error) string {
	return e.Error() + "\n错误码: " + e.Code
}

// progressFunc forwards emu.Progress as notifications/progress. Values must strictly increase: a new step jumps
// to its Done count, repeats within a step creep towards the next integer. Repeats are throttled to one per
// progressEvery; step changes always go out.
func (s *server) progressFunc(c *call) func(emu.Progress) {
	var (
		mu       sync.Mutex
		sent     bool
		last     float64
		lastDone = -1
		lastAt   time.Time
	)
	return func(p emu.Progress) {
		mu.Lock()
		defer mu.Unlock()
		now := time.Now()
		if sent && p.Done <= lastDone && now.Sub(lastAt) < s.progressEvery {
			return
		}
		v := nextProgress(last, sent, p.Done)
		params := progressParams{ProgressToken: c.token, Progress: v, Message: p.Message}
		if p.Total > 0 {
			params.Total = float64(p.Total)
		}
		if s.sendFor(c, notification{JSONRPC: "2.0", Method: "notifications/progress", Params: params}) {
			sent, last, lastAt = true, v, now
			lastDone = max(lastDone, p.Done)
		}
	}
}

// nextProgress: the first value is done; afterwards done when it is past last, else 1/40 of the way from last to
// done+1, so a boot wait polled once a second (at most 300 s by default) stays below the next step. Rounded to 6
// decimals and always above last (a much longer wait may creep past done+1, which only looks odd).
func nextProgress(last float64, sent bool, done int) float64 {
	const scale = 1e6
	base := float64(done)
	if !sent {
		return base
	}
	v := base
	if base <= last {
		v = last + (base+1-last)/40
	}
	v = math.Round(v*scale) / scale
	if v <= last {
		v = math.Round((last+1/scale)*scale) / scale
	}
	return v
}

// ---- writing -----------------------------------------------------------------------------------------------------

func (s *server) reply(r *request, result any) {
	s.write(response{JSONRPC: "2.0", ID: r.id, Result: result})
}

func (s *server) sendError(id json.RawMessage, code int, message string, data any) {
	s.write(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

// write puts one message on out as one line. All output goes through here, under one lock.
func (s *server) write(msg any) bool {
	line, err := encodeJSON(msg)
	if err != nil {
		s.log.Errorf("MCP 消息编码失败: %v", err)
		return false
	}
	line = append(line, '\n')
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if s.closed.Load() {
		return false
	}
	if _, err := s.w.Write(line); err != nil {
		if !s.wfail {
			s.log.Errorf("写标准输出失败: %v", err)
			s.wfail = true
		}
		return false
	}
	return true
}

// clip shortens a received line for a log message.
func clip(b []byte) string {
	const n = 200
	if len(b) <= n {
		return string(b)
	}
	cut := n
	for cut > 0 && (b[cut]&0xC0) == 0x80 {
		cut--
	}
	return string(b[:cut]) + "…"
}
