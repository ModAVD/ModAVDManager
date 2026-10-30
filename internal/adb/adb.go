//go:build windows

// Package adb runs adb against the root's private adb server and talks to the emulator console.
//
// Port of AUTO-MAS app/utils/common/emulator2/avd/host.py (adb and console parts). Two hard rules measured there:
//   - adb only ever goes to the private server (-P <AdbServerPort>, never 5037). The emulator registers with it once
//     at boot; when the server restarts, Reregister sends host:emulator:<adb port> on the emulator's behalf.
//   - no adb server may inherit our handles: a server spawned by an adb client inherits that client's stdout pipe and
//     whoever reads it waits until the server exits. So EnsureServer starts it detached first, always.
package adb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// DefaultTimeout of one adb command.
const DefaultTimeout = 20 * time.Second

// Timings from AUTO-MAS host.py.
const (
	// EnsureServer's own wait when the command does not use TotalTimeout (ensure_adb_server default).
	serverStartTimeout = 20 * time.Second
	listenPoll         = 100 * time.Millisecond
	// After adb exits its pipes are not waited on for longer than this: an adb server the client spawned itself
	// inherits them and would keep them open until it exits.
	pipeWaitDelay = 2 * time.Second

	registerTimeout = 5 * time.Second
	// The server sends no reply to host:emulator; give it a moment to read before we close (AUTO-MAS sleeps 0.2 s).
	registerLinger = 200 * time.Millisecond
	// A younger qemu has not reached its own registration yet (boot, or started by someone else just now).
	reregisterMinQemuAge = 60 * time.Second
	// How long a re-registered serial may take to show up as "device" (measured 451 ms).
	reregisterWait         = 5 * time.Second
	reregisterPoll         = 200 * time.Millisecond
	reregisterStateTimeout = 5 * time.Second
	closeStatePoll         = 100 * time.Millisecond
)

// Client is bound to one root. Booting, when set, reports serials whose instance is booting right now (another
// modavd holds its start lock); Reregister never registers those, the emulator registers itself at the end of boot.
// OnReregistered, when set, runs in the background after a serial was registered again (modavd restarts the logcat
// capture there); WaitHooks waits for those runs.
type Client struct {
	Root           *root.Root
	Log            *logx.Logger
	Booting        func(serial string) bool
	OnReregistered func(serial string)

	// hooks tracks the OnReregistered runs of this client and of the clients Derive made from it. Nil for a Client
	// written as a literal, which then shares literalHooks.
	hooks *hookGroup
}

func New(r *root.Root, log *logx.Logger) *Client {
	return &Client{Root: r, Log: log, hooks: &hookGroup{}}
}

// Derive is a client on the same root with another logger and OnReregistered (Booting is kept). It shares c's hook
// tracking, so WaitHooks on either one also waits for the hooks the other started.
func (c *Client) Derive(log *logx.Logger, onReregistered func(serial string)) *Client {
	return &Client{Root: c.Root, Log: log, Booting: c.Booting, OnReregistered: onReregistered, hooks: c.hookGroup()}
}

// literalHooks tracks the hooks of clients not made by New or Derive.
var literalHooks hookGroup

func (c *Client) hookGroup() *hookGroup {
	if c.hooks != nil {
		return c.hooks
	}
	return &literalHooks
}

// WaitHooks waits up to timeout for the OnReregistered runs this client (or one derived from it, or the one it was
// derived from) started in the background, including any that start meanwhile.
//
// A short-lived process must call it before it exits: the hook ends the logcat capture of the old private server and
// starts a new one, and a process that exits in between leaves the instance without a guest log for the rest of its
// boot (AUTO-MAS runs the hook in its long-running backend, and its watchdog would restart the capture anyway).
func (c *Client) WaitHooks(timeout time.Duration) {
	if !c.hookGroup().wait(timeout) {
		c.Log.Warnf("重新登记后的收尾（重起客体日志落盘）%.0f 秒内没做完，不再等", timeout.Seconds())
	}
}

// hookGroup counts running hooks. Not a sync.WaitGroup: hooks may start while somebody waits, which a WaitGroup
// forbids once its counter has reached zero.
type hookGroup struct {
	mu      sync.Mutex
	running int
	idle    chan struct{} // closed when running drops back to 0; a new one for every busy period
}

func (g *hookGroup) add() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running == 0 {
		g.idle = make(chan struct{})
	}
	g.running++
}

func (g *hookGroup) done() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.running--
	if g.running == 0 {
		close(g.idle)
	}
}

// wait reports whether no hook runs any more within timeout.
func (g *hookGroup) wait(timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		g.mu.Lock()
		if g.running == 0 {
			g.mu.Unlock()
			return true
		}
		idle := g.idle
		g.mu.Unlock()
		select {
		case <-idle: // look again: another hook may have started since
		case <-deadline.C:
			return false
		}
	}
}

// Env is the environment for emulator.exe, adb servers and adb clients: ours plus ANDROID_SDK_ROOT and ANDROID_HOME =
// RuntimeSDK, ANDROID_AVD_HOME = AvdHome, ANDROID_ADB_SERVER_PORT = AdbServerPort and root.NoLocalScanEnv. Keys are
// replaced case-insensitively (Windows).
func (c *Client) Env() []string {
	sdk := c.Root.RuntimeSDK()
	set := map[string]string{
		"ANDROID_SDK_ROOT":        sdk,
		"ANDROID_HOME":            sdk,
		"ANDROID_AVD_HOME":        c.Root.AvdHome(),
		"ANDROID_ADB_SERVER_PORT": strconv.Itoa(c.Root.AdbServerPort()),
	}
	for k, v := range root.NoLocalScanEnv {
		set[k] = v
	}
	return mergeEnv(os.Environ(), set)
}

// mergeEnv returns base with the keys of set replaced in place (compared case-insensitively, as Windows does; later
// duplicates dropped) and the keys base lacks appended in sorted order.
func mergeEnv(base []string, set map[string]string) []string {
	byUpper := make(map[string]string, len(set)) // upper-case key -> key as given in set
	for k := range set {
		byUpper[strings.ToUpper(k)] = k
	}
	out := make([]string, 0, len(base)+len(set))
	done := map[string]bool{}
	for _, kv := range base {
		up := strings.ToUpper(envKey(kv))
		k, ok := byUpper[up]
		if !ok {
			out = append(out, kv)
			continue
		}
		if !done[up] {
			out = append(out, k+"="+set[k])
			done[up] = true
		}
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		if !done[strings.ToUpper(k)] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+set[k])
	}
	return out
}

// envKey is the name of a "K=V" entry. Windows keeps hidden per-drive entries such as "=C:=C:\dir" whose name
// itself starts with '='.
func envKey(kv string) string {
	start := 0
	if strings.HasPrefix(kv, "=") {
		start = 1
	}
	if i := strings.IndexByte(kv[start:], '='); i >= 0 {
		return kv[:start+i]
	}
	return kv
}

// ---- servers -------------------------------------------------------------------------------------------------------

// serverKind names one of the two servers in messages (AUTO-MAS _port_conflict arguments).
type serverKind struct {
	label, key, otherKey string
}

var (
	privateServer = serverKind{label: "私有", key: "adbServerPort", otherKey: "scriptAdbServerPort"}
	scriptServer  = serverKind{label: "脚本专用", key: "scriptAdbServerPort", otherKey: "adbServerPort"}
)

func (c *Client) portConflict(k serverKind, port int, pid uint32, name string) *errs.Error {
	return errs.Newf(errs.PortConflict,
		"魔改 AVD 的%s adb 端口 %d 被 %s（PID %d）占用，它不是 adb。请在 %s 里把 %s 改成一个没被占用的端口"+
			"（不能用 5037 和各实例的控制台 / adb / gRPC 端口，也不能和 %s 相同），或者先关掉占用它的程序",
		k.label, port, name, pid, c.Root.MetadataPath(), k.key, k.otherKey)
}

// listener reports whether somebody listens on port, with its PID and process name ("" when unreadable). Reads the
// system table only; connecting to an emulator's adb port would disturb it.
func listener(port int) (up bool, pid uint32, name string, err error) {
	pid, up, err = win.ListenerPID(port)
	if err != nil {
		return false, 0, "", errs.Wrap(errs.Internal, err, "读不出系统的 TCP 监听表: "+err.Error())
	}
	if !up {
		return false, 0, "", nil
	}
	if pid != 0 {
		name = win.ProcessName(pid)
	}
	return true, pid, name, nil
}

func listening(port int) (bool, error) {
	up, _, _, err := listener(port)
	return up, err
}

func isAdbName(name string) bool {
	n := strings.ToLower(name)
	return n == "adb.exe" || n == "adb"
}

func canceled(ctx context.Context) *errs.Error {
	return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
}

// sleepCtx waits d; false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// EnsureServer starts the private server (adb -P <port> start-server, detached, Env()) unless something listens on
// the port, and waits up to timeout until it listens (polling the listener table every 100 ms, not waiting for the
// start-server client, which takes ~2.1 s; a client that exits non-zero while the port is still closed = failed).
// Returns whether it listens. A listener whose process name is not adb.exe is errs.PortConflict with the AUTO-MAS
// message (change adbServerPort in mas-avd.json); an unreadable owner name counts as adb.
func (c *Client) EnsureServer(ctx context.Context, timeout time.Duration) (bool, error) {
	return c.ensure(ctx, privateServer, c.Root.AdbServerPort(), timeout)
}

// EnsureScriptServer is EnsureServer for ScriptAdbServerPort (the server scripts use).
func (c *Client) EnsureScriptServer(ctx context.Context, timeout time.Duration) (bool, error) {
	return c.ensure(ctx, scriptServer, c.Root.ScriptAdbServerPort(), timeout)
}

func (c *Client) ensure(ctx context.Context, k serverKind, port int, timeout time.Duration) (bool, error) {
	up, pid, name, err := listener(port)
	if err != nil {
		return false, err
	}
	if up {
		if name != "" && !isAdbName(name) {
			return false, c.portConflict(k, port, pid, name)
		}
		return true, nil
	}
	if err := ctx.Err(); err != nil {
		return false, canceled(ctx)
	}
	c.Log.Infof("启动魔改 AVD %s adb server（端口 %d）", k.label, port)
	// StartOrphan rather than StartDetached: an MCP client ends modavd's whole process tree when it disconnects, and
	// while the start-server client (our child for ~2 s) is alive the server it forks is part of that tree. Losing
	// the server is not harmless: the emulator registers with it only once, at boot. Without win.OrphanHelper (unit
	// tests) StartOrphan is StartDetached. Output goes nowhere, like AUTO-MAS (stdout=DEVNULL).
	client, err := win.StartOrphan(c.Root.AdbExe(), []string{"-P", strconv.Itoa(port), "start-server"},
		win.StartOptions{Env: c.Env()})
	if err != nil {
		return false, errs.Wrap(errs.Internal, err, fmt.Sprintf("启动魔改 AVD %s adb server（端口 %d）失败: %s",
			k.label, port, errs.As(err).Message))
	}
	return waitListening(ctx, port, timeout, client)
}

// waitListening polls until port listens, at most timeout (AUTO-MAS _wait_listening). It does not wait for the
// start-server client to exit, but gives up early when that client has failed.
func waitListening(ctx context.Context, port int, timeout time.Duration, client win.Process) (bool, error) {
	deadline := time.Now().Add(timeout)
	for {
		up, err := listening(port)
		if err != nil || up {
			return up, err
		}
		if clientFailed(client) {
			return listening(port)
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		if !sleepCtx(ctx, listenPoll) {
			return false, canceled(ctx)
		}
	}
}

// clientFailed: the start-server client exited without success. Once it is gone its exit code may be unreadable
// (nobody holds a handle to it); that counts as failed too, since a client that succeeded leaves the port listening.
func clientFailed(p win.Process) bool {
	if p.PID == 0 || p.Alive() {
		return false
	}
	code, ok := p.ExitCode()
	return !ok || code != 0
}

// ---- commands ------------------------------------------------------------------------------------------------------

// Result of one adb command. Out is stdout and stderr merged in arrival order (one pipe for both, as AUTO-MAS
// stderr=STDOUT), decoded like AUTO-MAS decode_bytes (UTF-8, else the ANSI code page, GBK, GB18030, else Latin-1)
// and trimmed; Stdout / Stderr are kept separately only with RunOptions.Raw, and then Out is the Stderr text.
type Result struct {
	Code   int // the exit code; -1 when adb could not run or timed out (Out says why)
	Out    string
	Stdout []byte
	Stderr string
}

func (r Result) OK() bool { return r.Code == 0 }

type RunOptions struct {
	Serial  string        // -s <serial>; "" for none
	Timeout time.Duration // 0 = DefaultTimeout
	// Reregister: when the server does not know Serial ("device 'emulator-20000' not found"), register it again and
	// retry once (only if its own qemu listens on its adb port, it is not booting and qemu is >= 60 s old).
	Reregister bool
	// TotalTimeout: starting the server counts against Timeout too (close path); otherwise EnsureServer gets its own
	// 20 s. When the server cannot be started in time adb is not run at all.
	TotalTimeout bool
	// Raw keeps stdout bytes and stderr text apart (exec-out screencap ...).
	Raw bool
}

// Run is `adb -P <port> [-s serial] args...` with stdin NUL, no console window, Env(); the process is killed at the
// timeout (and its pipes are not waited on for more than 2 s after it exits). Never returns an error: failures are
// Code -1 with the reason in Out.
func (c *Client) Run(ctx context.Context, opt RunOptions, args ...string) Result {
	res := c.runOnce(ctx, opt, args)
	if opt.Reregister && res.Code != 0 && opt.Serial != "" && DeviceMissing(opt.Serial, res.Out) {
		if c.Reregister(ctx, opt.Serial) {
			// AUTO-MAS run_adb retries without total_timeout: the server is up again by now.
			retry := opt
			retry.TotalTimeout = false
			res = c.runOnce(ctx, retry, args)
		}
	}
	return res
}

func failed(raw bool, msg string) Result {
	r := Result{Code: -1, Out: msg}
	if raw {
		r.Stderr = msg
	}
	return r
}

// runOnce is AUTO-MAS _run_adb_raw: the server first, then one adb client.
func (c *Client) runOnce(ctx context.Context, opt RunOptions, args []string) Result {
	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	started := time.Now()
	port := c.Root.AdbServerPort()
	wait := serverStartTimeout
	if opt.TotalTimeout {
		wait = timeout
	}
	// Never let the adb client start the server itself: that server would inherit the client's pipes, and with them
	// ours (WaitDelay bounds the damage, but the server would then hold a dead pipe for its whole life).
	up, err := c.EnsureServer(ctx, wait)
	if err != nil {
		return failed(opt.Raw, err.Error())
	}
	if !up {
		return failed(opt.Raw, fmt.Sprintf("私有 adb server（端口 %d）%.0f 秒内没起来", port, wait.Seconds()))
	}
	limit := timeout
	if opt.TotalTimeout {
		limit -= time.Since(started)
	}
	cmdArgs := []string{"-P", strconv.Itoa(port)}
	if opt.Serial != "" {
		cmdArgs = append(cmdArgs, "-s", opt.Serial)
	}
	cmdArgs = append(cmdArgs, args...)
	return c.execAdb(ctx, opt.Raw, limit, timeout, args, cmdArgs)
}

// execAdb runs adb.exe cmdArgs for at most limit. The timeout message names args (without -P / -s) and the whole
// timeout, like AUTO-MAS.
func (c *Client) execAdb(ctx context.Context, raw bool, limit, timeout time.Duration, args, cmdArgs []string) Result {
	timedOut := func() Result {
		return failed(raw, fmt.Sprintf("adb %s 超时（%.0f 秒）", strings.Join(args, " "), timeout.Seconds()))
	}
	if ctx.Err() != nil {
		return failed(raw, "已取消")
	}
	if limit <= 0 {
		return timedOut()
	}
	tctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(tctx, c.Root.AdbExe(), cmdArgs...)
	cmd.Env = c.Env()
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	cmd.WaitDelay = pipeWaitDelay
	// Cancel runs only when the deadline (or ctx) ends adb while it still runs, not when merely the pipe wait after a
	// normal exit outlasts the deadline; that is what tells a timeout from a slow pipe.
	var killed atomic.Bool
	cmd.Cancel = func() error {
		err := cmd.Process.Kill()
		if err == nil {
			killed.Store(true)
		}
		return err
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	if raw {
		cmd.Stderr = &stderr
	} else {
		cmd.Stderr = &stdout // the same writer: os/exec gives the child one pipe for both, so the order is kept
	}
	err := cmd.Run()
	if cmd.ProcessState == nil {
		if ctx.Err() != nil {
			return failed(raw, "已取消")
		}
		return failed(raw, "无法运行 adb: "+err.Error())
	}
	if killed.Load() {
		if ctx.Err() != nil {
			return failed(raw, fmt.Sprintf("adb %s 已取消", strings.Join(args, " ")))
		}
		return timedOut()
	}
	code := cmd.ProcessState.ExitCode()
	if raw {
		text := strings.TrimSpace(decodeOutput(stderr.Bytes()))
		return Result{Code: code, Out: text, Stdout: stdout.Bytes(), Stderr: text}
	}
	return Result{Code: code, Out: strings.TrimSpace(decodeOutput(stdout.Bytes()))}
}

// Shell is Run(serial, "shell", command) with Reregister.
func (c *Client) Shell(ctx context.Context, serial, command string, timeout time.Duration) Result {
	return c.Run(ctx, RunOptions{Serial: serial, Timeout: timeout, Reregister: true}, "shell", command)
}

// ---- registration --------------------------------------------------------------------------------------------------

// Register sends host:emulator:<emulatorAdbPort> (4 hex digits of length + message) to the server on serverPort,
// waits 200 ms and closes; the server sends no reply.
func Register(ctx context.Context, serverPort, emulatorAdbPort int, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = registerTimeout
	}
	msg := "host:emulator:" + strconv.Itoa(emulatorAdbPort)
	payload := fmt.Sprintf("%04x%s", len(msg), msg)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(serverPort))
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return canceled(ctx)
		}
		if dctx.Err() != nil {
			return errs.Wrap(errs.Internal, err, fmt.Sprintf("连接 %s 超时（%.0f 秒）", addr, timeout.Seconds()))
		}
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("连不上 %s: %v", addr, err))
	}
	defer conn.Close()
	conn.SetWriteDeadline(time.Now().Add(timeout))
	if _, err := io.WriteString(conn, payload); err != nil {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("向 %s 发送登记失败: %v", addr, err))
	}
	if !sleepCtx(ctx, registerLinger) {
		return canceled(ctx)
	}
	return nil
}

// reregisterLocks serializes re-registration per serial (AUTO-MAS _REREGISTER_LOCKS), so concurrent polls do not
// all send it. Each value is a chan struct{} of capacity 1, so waiting for it can be canceled.
var reregisterLocks sync.Map

func serialLock(serial string) chan struct{} {
	l, _ := reregisterLocks.LoadOrStore(serial, make(chan struct{}, 1))
	return l.(chan struct{})
}

// consolePortOf maps emulator-<console> to the console port when it is one of our port plan.
func consolePortOf(serial string) (int, bool) {
	rest, ok := strings.CutPrefix(serial, "emulator-")
	if !ok || rest == "" || len(rest) > 6 {
		return 0, false
	}
	for _, ch := range rest {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	port, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	if _, ok := root.IndexOfConsolePort(port); !ok {
		return 0, false
	}
	return port, true
}

// Reregister brings serial back onto the private server (AUTO-MAS reregister_emulator): only for emulator-<console>
// serials of our port plan, not while Booting(serial), only when QemuOnAdbPort finds the instance's own qemu (of this
// root) and it is at least 60 s old; then get-state, Register, and up to 5 s of get-state == "device". Serialized per
// serial within this process. Starts OnReregistered on success.
func (c *Client) Reregister(ctx context.Context, serial string) bool {
	console, ok := consolePortOf(serial)
	if !ok || (c.Booting != nil && c.Booting(serial)) {
		return false
	}
	qemu, ok := qemuOnAdbPort(c.Root, console)
	if !ok {
		return false
	}
	startedAt := qemu.StartedAt()
	if startedAt.IsZero() || time.Since(startedAt) < reregisterMinQemuAge {
		return false
	}
	lock := serialLock(serial)
	select {
	case lock <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-lock }()

	serverPort := c.Root.AdbServerPort()
	if c.GetState(ctx, serial, reregisterStateTimeout) == "device" {
		return true // another caller just registered it
	}
	c.Log.Warnf("私有 adb server（端口 %d）上没有 %s，按 adb 端口 %d 重新登记", serverPort, serial, console+1)
	started := time.Now()
	if err := Register(ctx, serverPort, console+1, registerTimeout); err != nil {
		c.Log.Warnf("重新登记 %s 失败: %v", serial, err)
		return false
	}
	for time.Since(started) < reregisterWait {
		if c.GetState(ctx, serial, reregisterStateTimeout) == "device" {
			c.Log.Infof("%s 已重新登记到私有 adb server（用时 %.0f ms）", serial, msSince(started))
			c.runReregisterHook(serial)
			return true
		}
		if !sleepCtx(ctx, reregisterPoll) {
			return false
		}
	}
	c.Log.Warnf("重新登记 %s 后 %.0f 秒仍未上线", serial, reregisterWait.Seconds())
	return false
}

// runReregisterHook runs OnReregistered in the background (its failure must not undo the recovered connection, nor
// hold up the command that re-registered), tracked for WaitHooks.
func (c *Client) runReregisterHook(serial string) {
	hook := c.OnReregistered
	if hook == nil {
		return
	}
	g := c.hookGroup()
	g.add()
	go func() {
		defer g.done()
		defer func() {
			if r := recover(); r != nil {
				c.Log.Warnf("%s 重新登记后的收尾失败: %v", serial, r)
			}
		}()
		hook(serial)
	}()
}

func msSince(t time.Time) float64 { return float64(time.Since(t)) / float64(time.Millisecond) }

// PrepareForClose makes the private server usable for this instance within timeout before a shutdown (AUTO-MAS
// prepare_adb_for_close). Server not listening: start it, check QemuOnAdbPort, Register, wait for get-state ==
// device. Server listening (and adb): one get-state; when the server does not know the instance, the same check,
// Register and wait. All within timeout. Returns "" when usable, else the reason (Chinese), and the stop then goes
// straight to the console.
//
// AUTO-MAS trusts a listening server as is, because its freeze watchdog and its status polls re-register a lost
// instance within seconds. modavd has neither, and a server that was restarted (kill-server, a crash, `modavd
// servers`, another instance's command) does not know a running instance until somebody registers it. The shutdown
// commands do not re-register, so they and the fallback sync would fail with "device not found" and qemu would be
// killed without a sync. The get-state is answered by the server itself (tens of ms). Any other answer (device,
// offline, a timeout) is left to the commands that follow, as before.
//
// It does not go through Reregister: the boot guard (60 s) and its 5 s wait do not fit a shutdown.
func (c *Client) PrepareForClose(ctx context.Context, console int, timeout time.Duration) string {
	started := time.Now()
	port := c.Root.AdbServerPort()
	serial := "emulator-" + strconv.Itoa(console)
	up, pid, name, err := listener(port)
	if err != nil {
		return err.Error()
	}
	if up {
		if name != "" && !isAdbName(name) {
			return c.portConflict(privateServer, port, pid, name).Error()
		}
		res := c.runOnce(ctx, RunOptions{Serial: serial, Timeout: timeout, TotalTimeout: true}, []string{"get-state"})
		if res.Code == 0 || !DeviceMissing(serial, res.Out) {
			return ""
		}
		c.Log.Warnf("私有 adb server（端口 %d）上没有 %s，关机前替它登记", port, serial)
		return c.registerForClose(ctx, console, started, timeout, "私有 adb server")
	}
	ok, err := c.EnsureServer(ctx, timeout)
	if err != nil {
		return err.Error()
	}
	if !ok {
		return fmt.Sprintf("私有 adb server（端口 %d）%.0f 秒内没起来", port, timeout.Seconds())
	}
	return c.registerForClose(ctx, console, started, timeout, "新起的私有 adb server")
}

// registerForClose is the tail of PrepareForClose once the private server listens but does not know the instance:
// check that qemu of this root's instance listens on its adb port, Register it, wait for get-state == device, all
// within what is left of timeout (measured from started). server names the server in messages.
func (c *Client) registerForClose(ctx context.Context, console int, started time.Time, timeout time.Duration,
	server string) string {
	port := c.Root.AdbServerPort()
	serial := "emulator-" + strconv.Itoa(console)
	if _, ok := qemuOnAdbPort(c.Root, console); !ok {
		return fmt.Sprintf("adb 端口 %d 上不是这台实例的模拟器，不替它登记", console+1)
	}
	remaining := timeout - time.Since(started)
	if remaining <= 0 {
		return fmt.Sprintf("私有 adb server（端口 %d）%.0f 秒内没来得及登记 %s", port, timeout.Seconds(), serial)
	}
	if err := Register(ctx, port, console+1, remaining); err != nil {
		if errs.CodeOf(err) == errs.Canceled {
			return "已取消"
		}
		return fmt.Sprintf("向%s 登记 %s 失败: %v", server, serial, err)
	}
	for {
		remaining = timeout - time.Since(started)
		if remaining <= 0 {
			return fmt.Sprintf("%s 上 %s 在 %.0f 秒内没有上线", server, serial, timeout.Seconds())
		}
		res := c.runOnce(ctx, RunOptions{Serial: serial, Timeout: remaining, TotalTimeout: true}, []string{"get-state"})
		if res.Code == 0 && res.Out == "device" {
			c.Log.Infof("关机前已向%s 登记 %s（用时 %.0f ms）", server, serial, msSince(started))
			return ""
		}
		if !sleepCtx(ctx, closeStatePoll) {
			return "已取消"
		}
	}
}

// qemuOnAdbPort is QemuOnAdbPort; tests put a stand-in here (a qemu of our own would need a real instance port).
var qemuOnAdbPort = QemuOnAdbPort

// QemuOnAdbPort is the process listening on console+1 when it is qemu-system* with "-ports <console>,<console+1>"
// and "-avd mas_<i>" (i from the console port) on its command line, and QemuBelongsTo says it is instance i of r.
//
// Somebody listening is not enough: the port may belong to another program, and registering it would hand that
// program's port to the private server. Nor are the arguments: the same-numbered instance of another root has the
// same ones, and registering it would put one emulator on two private servers.
func QemuOnAdbPort(r *root.Root, console int) (win.Process, bool) {
	i, ok := root.IndexOfConsolePort(console)
	if !ok || r == nil {
		return win.Process{}, false
	}
	pid, up, err := win.ListenerPID(console + 1)
	if err != nil || !up || pid == 0 {
		return win.Process{}, false
	}
	p, ok := win.FindProcess(pid)
	if !ok || !strings.HasPrefix(strings.ToLower(p.Name), "qemu-system") {
		return win.Process{}, false
	}
	cmdline, err := p.CommandLine()
	if err != nil || !qemuArgsMatch(cmdline, console, root.AvdName(i)) || !QemuBelongsTo(r, p, i) {
		return win.Process{}, false
	}
	return p, true
}

// qemuArgsMatch: the adjacent pairs ("-ports", "<console>,<console+1>") and ("-avd", avd) are both on the command
// line (AUTO-MAS instance_qemu_on_adb_port).
func qemuArgsMatch(cmdline []string, console int, avd string) bool {
	ports := strconv.Itoa(console) + "," + strconv.Itoa(console+1)
	hasPorts, hasAvd := false, false
	for k := 0; k+1 < len(cmdline); k++ {
		switch cmdline[k] {
		case "-ports":
			hasPorts = hasPorts || cmdline[k+1] == ports
		case "-avd":
			hasAvd = hasAvd || cmdline[k+1] == avd
		}
	}
	return hasPorts && hasAvd
}

// DeviceMissing: output is the server saying it has no such device (contains the serial and "not found",
// case-insensitive).
func DeviceMissing(serial, output string) bool {
	text := strings.ToLower(output)
	return strings.Contains(text, strings.ToLower(serial)) && strings.Contains(text, "not found")
}

// GetState is `adb get-state` for serial; "device" when online. Not reregistering. "" when adb failed (unknown
// serial, server down, timeout).
func (c *Client) GetState(ctx context.Context, serial string, timeout time.Duration) string {
	res := c.Run(ctx, RunOptions{Serial: serial, Timeout: timeout}, "get-state")
	if res.Code != 0 {
		return ""
	}
	return res.Out
}

// isTimeout: a deadline (ours) ended the network operation.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout())
}
