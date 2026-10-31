//go:build windows

// Package cli is the command line of modavd: a thin layer over emu.Manager, plus mcp.Serve for `modavd mcp`.
//
// Contract (also printed by `modavd help`; part of buildinfo.Protocol):
//
//	modavd [--root <dir>] [--json] [--quiet] [--verbose] <command> [args]
//
// Global flags are accepted before or after the command (`modavd start 3 --json`), but not inside the verbatim tail
// of adb / shell. Command flags may follow positional arguments. Commands and their --json "data":
//
//	list                                                  emu.ListResult
//	info <i>                                              emu.InstanceInfo
//	status <i>                                            StatusResult {index,name,status,running,booted,busy,pid}
//	create [--index N] [--title T] [--memory M] [--cpu N] [--data-gb N] [--fps 30|60]   emu.InstanceInfo
//	set <i> [--title T] [--memory M] [--cpu N] [--fps F]  emu.UpdateResult (at least one option)
//	delete <i> --yes                                      emu.DeleteResult (without --yes: usage error)
//	start <i> [--window] [--timeout 300]                  emu.StartResult
//	stop <i> [--force]                                    emu.StopResult
//	stop --all [--force]                                  StopAllResult {"results":[StopResult],"failed":[{index,error}]}
//	restart <i> [--window] [--timeout 300] [--force]      emu.StartResult (stop, wait for qemu to exit, then start)
//	check [--memory M]                                    emu.CheckResult (exit 0 even when not ready: see "ready")
//	window <i> show|hide                                  emu.WindowResult
//	install <i> <apk>                                     emu.InstallResult
//	shell <i> [--timeout S] <command...>                  emu.ShellResult
//	adb <i> [--] <adb args...>                            no envelope: adb's own stdio and exit code
//	servers                                               emu.ServersResult
//	version                                               VersionResult {"version","protocol","go"} (no root needed)
//	mcp                                                   MCP server on stdin / stdout
//	help [command]                                        HelpResult
//
// <i> is 3 or mas_3. --memory takes what avd.ParseMemoryMB takes (6144, 6144M, 6G ...). The root is
// root.Resolve(--root), looked up only by commands that need it, after their arguments were checked.
//
// Output. With --json, stdout carries exactly one line, {"ok":true,"data":<result>} or
// {"ok":false,"error":{"code","message","advice"}} (advice omitted when empty), and stderr carries NDJSON logs
// (logx.JSONSink). Without --json, stdout gets a Chinese summary, errors go to stderr as "错误：…" / "建议：…" lines
// and logs go to stderr as text (logx.TextSink). Log level Info; --quiet Warn; --verbose Debug (and the cause of an
// error). adb and mcp never print an envelope (their stdout belongs to adb / the protocol): their errors go to
// stderr only.
//
// Exit codes are errs.ExitCode (0 ok, 1 internal, 2 usage / invalid argument, 3 not found, 4 not ready / precheck
// failed / unsupported, 5 busy / already exists / port conflict, 6 timeout, 7 launch failed, 8 canceled), except:
// shell without --json exits with the guest command's exit code, adb with adb's, and stop --all with the code of its
// first failure (its envelope still says ok and lists both results and failed). Ctrl+C cancels the context
// (errs.Canceled); an emulator that was already launched keeps running (emu guarantees that). A run during which the
// context ended fails with errs.Canceled even when the command returned a result, unless the command is final (its
// result reports a change that was made: create, set, delete, start, stop, restart, window, install; or does not
// depend on the context: version, help): a query whose adb calls were killed half way would report what it saw by
// then, an online instance as starting.
//
// Before Main returns, a command that used a manager waits up to backgroundWait for the manager's background work
// (emu.Manager.Wait: the logcat restart after a re-registration), after its output is written.
//
// Hidden subcommands, routed before any flag parsing or root lookup: __spawn (win.SpawnHelperMain, the helper of
// win.StartOrphan) and __vkprobe (emu.RunVulkanProbe, the Vulkan probe child process).
package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/mcp"
	"modavd/internal/root"
	"modavd/internal/update"
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
	EnsureServers(ctx context.Context) (*emu.ServersResult, error)
	// Running is the indexes whose qemu runs, from the process table only (no adb).
	Running(ctx context.Context) ([]int, error)
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
	newManager  func(rootFlag string, log *logx.Logger) (manager, error)
	resolveRoot func(flag string) (*root.Root, error)
	serveMCP    func(ctx context.Context, in io.Reader, out io.Writer, opt mcp.Options) error
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
		resolveRoot: root.Resolve,
		serveMCP:    mcp.Serve,
	}
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
		case "__version":
			return writeHiddenVersion(stdout)
		}
	}
	update.Finalize()
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

func writeHiddenVersion(w io.Writer) int {
	if _, err := fmt.Fprintf(w, "{\"ok\":true,\"data\":{\"version\":%q,\"protocol\":%d,\"go\":%q}}\n", buildinfo.Version, buildinfo.Protocol, runtime.Version()); err != nil {
		return 1
	}
	return 0
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
