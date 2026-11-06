// Package cli is the command line of modavd: a thin layer over emu.Manager, plus mcp.Serve for `modavd mcp`.
//
// Contract (also printed by `modavd help`; part of buildinfo.Protocol, 2 since 0.3.0: what changed is listed there):
//
//	modavd [--root <dir>] [--json] [--quiet] [--verbose] <command> [args]
//
// Global flags are accepted before or after the command (`modavd start 3 --json`), but not inside the verbatim tail
// of adb / shell. Command flags may follow positional arguments. Commands and their --json "data":
//
//	list                                                  emu.ListResult
//	info <i>                                              emu.InstanceInfo
//	status <i>                                            StatusResult {index,name,status,running,booted,window,busy,pid,creation,creation_error,creation_log,creation_progress,serial,adb_address,ports}
//	create [--index N] [--title T] [--memory M] [--cpu N] [--data-gb N] [--fps 30|60]   emu.InstanceInfo
//	set <i> [--title T] [--memory M] [--cpu N] [--fps F]  emu.UpdateResult (at least one option)
//	proxy <i> [on|off] [--server URL|auto] [--add P] [--remove P] [--apps P]   emu.AppProxyResult
//	proxy <i> test                                        emu.AppProxyTestResult
//	delete <i> --yes                                      emu.DeleteResult (without --yes: usage error)
//	start <i> [--window|--headless] [--timeout 300]       emu.StartResult (running the other way than asked: busy)
//	stop <i> [--force]                                    emu.StopResult
//	stop --all [--force]                                  StopAllResult {"results":[StopResult],"failed":[{index,error}]}
//	check [--memory M]                                    emu.CheckResult (exit 0 even when not ready: see "ready")
//	components [status]                                   ComponentsStatusResult {"root","components":[emu.ComponentStatus]}
//	components fetch [--accept-license] [--mirror ID] [--mirror-url URL]... [--keep-downloads]   components.Result
//	report-config [--no-clipboard]                        read-only configuration and installed-game report
//	collect-info [--out-dir D] [--stage-dir D] ...        read-only diagnostics zip
//	window <i> show|hide                                  emu.WindowResult
//	disk <i> trim                                         emu.DiskTrimResult {index,discard_max_bytes}
//	disk <i> info                                         emu.DiskInfoResult {index,overlay,running,needs_shutdown,logical_bytes,on_disk_bytes?,allocated_bytes?,reclaimable_bytes?}
//	disk <i> compact                                      emu.DiskCompactResult (host sizes, freed_bytes >= 0, signed on_disk_delta_bytes, skipped, estimate/threshold)
//	install <i> <apk>                                     emu.InstallResult
//	shell <i> [--timeout S] <command...>                  emu.ShellResult
//	adb <i> [--] <adb args...>                            no envelope: adb's own stdio and exit code
//	servers                                               emu.ServersResult
//	version                                               VersionResult {"version","protocol","go"} (no root needed)
//	update [status|check|prepare|install|recover]         update.Status (the ModAVD package of the root; see update)
//	mcp                                                   MCP server on stdin / stdout
//	help [command]                                        HelpResult
//
// restart went away in 0.3.0 (Protocol 2; no restart: stop, then start); calling it is a usage error that says so
// (removedCommands).
//
// <i> is 3 or mas_3. --memory takes what avd.ParseMemoryMB takes (6144, 6144M, 6G ...). The root is
// root.Resolve(--root), looked up only by commands that need it, after their arguments were checked.
//
// Output. With --json, stdout carries exactly one line, {"ok":true,"data":<result>} or
// {"ok":false,"error":{"code","message","advice","details"}} (advice/details omitted when empty), and stderr carries NDJSON logs
// (logx.JSONSink). Without --json, stdout gets a Chinese summary, errors go to stderr as "错误：…" / "建议：…" lines
// and logs go to stderr as text (logx.TextSink). Log level Info; --quiet Warn; --verbose Debug (and the cause of an
// error). adb and mcp never print an envelope (their stdout belongs to adb / the protocol): their errors go to
// stderr only.
//
// error.details is optional, with string values only. These additions do not change code/message or Protocol 2.
// busy reasons: running, locked, starting (stop before qemu exists), stopping (start awaiting shutdown),
// window_mismatch, port_in_use, port_other_root. locked also has lock: start, stop, boot, bridge, create, guest,
// disk (compact or a shared disk probe), metadata (mas-avd.json), config (per-instance config/metadata writes).
// Other busy errors may omit reason. Clients must tolerate missing/unknown keys and values.
// A failed boot wait or after-boot setup adds instance_status and qemu_running. qemu_running is "true", "false" or
// "unknown"; instance_status is a known lifecycle status, "not_found" or "unknown". The failed boot wait knows
// "starting"; later setup may restart Android, so its boot state can be unknown. The process observation is shared
// with the diagnostic decision, before its optional collection (up to 30 s); it is a snapshot, not a liveness lease.
// Create errors add instance_exists ("true" / "false" / "unknown"), instance_status and qemu_running after cleanup.
// instance_exists counts any instance files or metadata now occupying the index, even incomplete or pre-existing;
// it does NOT prove ownership by this call and never authorizes deleting an already_exists/busy instance.
// Before an index is selected, or when a fact cannot be observed, it is "unknown", never a guessed false.
// Creating/failing metadata overrides the boot state. An own create is observed while its lock is still held but
// reports the state after that lock's imminent release; another creator's state can be unknown without probing it.
// boot_diag_dir, when collected, survives wrappers and the final create state replacing an inner start's state.
// The manager HTTP responses and failed jobs carry these same details. MCP isError text appends one JSON line,
// "error.details: {...}", while retaining its message/advice, error code and diagnostic-directory lines.
//
// Exit codes are errs.ExitCode (0 ok, 1 internal, 2 usage / invalid argument / license declined, 3 not found, 4 not
// ready / precheck failed / unsupported, 5 busy / already exists / port conflict, 6 timeout, 7 launch failed, 8
// canceled, 9 update or components unavailable / rejected, proxy unavailable), except:
// shell without --json exits with the guest command's exit code, adb with adb's, and stop --all with the code of its
// first failure (its envelope still says ok and lists both results and failed). Ctrl+C cancels the context
// (errs.Canceled); an emulator that was already launched keeps running (emu guarantees that). A run during which the
// context ended fails with errs.Canceled even when the command returned a result, unless the command is final (its
// result reports a change that was made: create, set, proxy, delete, start, stop, window, install, components fetch;
// or does not depend on the context: version, help, components status; also bridge and disk trim / info): a query whose adb calls were killed half way
// would report what it saw by then, an online instance as starting. components fetch stops at Ctrl+C with
// errs.Canceled itself, between two reads of a download or an unpack and before the next component.
//
// Before Main returns, a command that used a manager waits up to backgroundWait for the manager's background work
// (emu.Manager.Wait: the logcat restart after a re-registration), after its output is written.
//
// Hidden subcommands, routed before any flag parsing or root lookup: __spawn (win.SpawnHelperMain, the helper of
// win.StartOrphan), __vkprobe (emu.RunVulkanProbe, the Vulkan probe child process) and __bootwatch
// (emu.RunBootWatch), and __logcat (emu.RunLogcatCapture, the host logcat writer).
package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"modavd/internal/appproxy"
	"modavd/internal/components"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/mcp"
	"modavd/internal/root"
	"modavd/internal/win"
)

// manager is the part of *emu.Manager the commands use; tests substitute a fake.
type manager interface {
	List(ctx context.Context) (*emu.ListResult, error)
	Info(ctx context.Context, i int) (*emu.InstanceInfo, error)
	Create(ctx context.Context, req emu.CreateRequest) (*emu.InstanceInfo, error)
	Update(ctx context.Context, i int, req emu.UpdateRequest) (*emu.UpdateResult, error)
	Delete(ctx context.Context, i int) (*emu.DeleteResult, error)
	Start(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error)
	Stop(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error)
	Check(ctx context.Context, opt emu.CheckOptions) (*emu.CheckResult, error)
	SetWindowVisible(ctx context.Context, i int, visible bool) (*emu.WindowResult, error)
	Shell(ctx context.Context, i int, command string, timeout time.Duration) (*emu.ShellResult, error)
	AdbPassthrough(ctx context.Context, i int, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	InstallAPK(ctx context.Context, i int, apkPath string) (*emu.InstallResult, error)
	Bridge(ctx context.Context, i int, action string) (*emu.BridgeResult, error)
	DiskTrim(ctx context.Context, i int) (*emu.DiskTrimResult, error)
	DiskInfo(ctx context.Context, i int) (*emu.DiskInfoResult, error)
	DiskCompact(ctx context.Context, i int) (*emu.DiskCompactResult, error)
	EnsureServers(ctx context.Context) (*emu.ServersResult, error)
	AppProxy(ctx context.Context, i int) (*emu.AppProxyResult, error)
	SetAppProxy(ctx context.Context, i int, c appproxy.Change) (*emu.AppProxyResult, error)
	TestAppProxy(ctx context.Context, i int) (*emu.AppProxyTestResult, error)
	// StopTargets is the running or actively creating indexes, without adb.
	StopTargets(ctx context.Context) ([]int, error)
	// Wait waits up to timeout for the manager's background work.
	Wait(timeout time.Duration)
}

// backgroundWait bounds the wait for a manager's background work before the process exits. That work is the logcat
// restart after a re-registration with the private adb server (a list does one when the server was restarted): main
// calls os.Exit right after Main, which would cut it half way, and the guest log would then not be saved for the rest
// of that boot (nothing re-registers again later).
const backgroundWait = 10 * time.Second

var _ manager = (*emu.Manager)(nil)

// deps are the outside world of a command run, replaced in tests.
type deps struct {
	newManager      func(rootFlag string, log *logx.Logger) (manager, error)
	resolveRoot     func(flag string) (*root.Root, error)
	serveMCP        func(ctx context.Context, in io.Reader, out io.Writer, opt mcp.Options) error
	fetchComponents func(ctx context.Context, r *root.Root, opt components.Options) (*components.Result, error)
	stdinTerminal   func(io.Reader) bool
}

func defaultDeps() deps {
	return deps{
		newManager: func(rootFlag string, log *logx.Logger) (manager, error) {
			r, err := root.Resolve(rootFlag)
			if err != nil {
				return nil, err
			}
			return emu.New(r, log), nil
		},
		resolveRoot:     root.Resolve,
		serveMCP:        mcp.Serve,
		fetchComponents: components.Fetch,
		stdinTerminal:   readerIsTerminal,
	}
}

// readerIsTerminal: stdin is someone at a console (components fetch may ask which mirror to use), not a pipe, a
// file or NUL. Platform part in terminal_*.go.
func readerIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && f != nil && fileIsTerminal(f)
}

// Main runs one command and returns the process exit code (errs.ExitCode, see the package comment).
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		switch args[0] {
		case win.SpawnHelperArg:
			f, ok := stdout.(*os.File)
			if !ok {
				f = os.Stdout
			}
			return win.SpawnHelperMain(args[1:], f)
		case emu.VulkanProbeArg:
			return emu.RunVulkanProbe(stdout)
		case emu.BootWatchArg:
			return emu.RunBootWatch(args[1:], stderr)
		case emu.LogcatCaptureArg:
			return emu.RunLogcatCapture(args[1:], stderr)
		case emu.DiagnosticArchiveArg:
			return emu.RunDiagnosticArchive(args[1:], stderr)
		}
	}
	// StartOrphan re-runs this executable as the spawn helper.
	if exe, err := os.Executable(); err == nil {
		win.OrphanHelper = exe
	}
	// SIGTERM is what Go delivers for a console close / logoff on Windows.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		// Back to the default handler: a second Ctrl+C ends modavd at once (lock files die with the process).
		stop()
	}()
	return run(ctx, args, stdin, stdout, stderr, defaultDeps())
}

// cmdCtx is what a command's run function gets.
type cmdCtx struct {
	ctx    context.Context
	inv    *invocation
	log    *logx.Logger
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	deps   deps
	m      manager
}

// mgr resolves the root and builds the manager on first use, so argument errors are reported before root errors
// and commands such as version never need a root.
func (c *cmdCtx) mgr() (manager, error) {
	if c.m != nil {
		return c.m, nil
	}
	m, err := c.deps.newManager(c.inv.root, c.log)
	if err != nil {
		return nil, err
	}
	c.m = m
	return m, nil
}

func newLogger(w io.Writer, g globals) *logx.Logger {
	min := logx.Info
	if g.quiet {
		min = logx.Warn
	}
	if g.verbose {
		min = logx.Debug
	}
	if g.json {
		return logx.New(logx.JSONSink(w, min))
	}
	return logx.New(logx.TextSink(w, min))
}

// run is Main without the process-wide setup.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, d deps) int {
	if d.stdinTerminal == nil {
		d.stdinTerminal = readerIsTerminal
	}
	inv, perr := parseArgs(args)
	log := newLogger(stderr, inv.globals)
	out := &output{stdout: stdout, stderr: stderr, json: inv.json, raw: inv.cmd != nil && inv.cmd.raw, log: log}
	if perr != nil {
		return out.fail(perr)
	}
	if inv.cmd == nil {
		if inv.help {
			return out.success(helpCommand(), helpAll())
		}
		return out.fail(errs.New(errs.Usage, "缺少命令").WithAdvice(helpAdvice))
	}
	if inv.help {
		// `modavd start --help` describes the command; the help entry is never raw.
		out.raw = false
		return out.success(helpCommand(), helpFor(inv.cmd))
	}
	if err := inv.checkArgs(); err != nil {
		return out.fail(err)
	}
	c := &cmdCtx{ctx: ctx, inv: inv, log: log, stdin: stdin, stdout: stdout, stderr: stderr, deps: d}
	// Deferred: runs after the output below is written, so the caller has its result at once.
	defer c.waitBackground()
	data, err := inv.cmd.run(c)
	if err == nil && ctx.Err() != nil && !inv.cmd.final && !inv.cmd.raw {
		// Ctrl+C during a query: its adb calls were killed and it returned what it saw by then (an online instance
		// as starting, a check as "could not check"). Never an ok result. raw commands keep their own exit code.
		err = errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
	if err != nil {
		if ctx.Err() != nil && errs.CodeOf(err) != errs.Canceled {
			// Whatever broke after Ctrl+C broke because of it (an adb killed half way ...).
			err = errs.Wrap(errs.Canceled, err, "已取消")
		}
		return out.fail(err)
	}
	if code, ok := data.(rawExit); ok {
		return int(code)
	}
	if rc := out.success(inv.cmd, data); rc != 0 {
		return rc
	}
	if inv.cmd.exit != nil {
		return inv.cmd.exit(data, inv.json)
	}
	return 0
}

// rawExit is the result of a command that wrote its own output (adb, mcp): just the exit code.
type rawExit int

// waitBackground waits for the background work of the manager this run built, if any (see backgroundWait). A second
// Ctrl+C still ends the process at once: Main restored the default handler after the first.
func (c *cmdCtx) waitBackground() {
	if c.m != nil {
		c.m.Wait(backgroundWait)
	}
}
