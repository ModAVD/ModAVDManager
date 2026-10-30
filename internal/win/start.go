//go:build windows

package win

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"modavd/internal/errs"
)

const (
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
	// AUTO-MAS WindowsProcessPlatform.emulator_flags.
	detachedFlags = HideConsole | createNewProcessGroup | createBreakawayFromJob

	orphanHelperTimeout = 15 * time.Second
	// How long the handle of a process we started is kept after it exited (for ExitCode).
	keepExitedHandle = 10 * time.Minute
)

// resolveExe makes exe absolute: a bare name is looked up on PATH, anything else is relative to our working
// directory. syscall.StartProcess would otherwise resolve a relative exe against Dir.
func resolveExe(exe string) (string, error) {
	if exe == "" {
		return "", errors.New("没有给出要启动的程序")
	}
	if !strings.ContainsAny(exe, `\/:`) {
		lp, err := exec.LookPath(exe)
		if err != nil {
			return "", err
		}
		exe = lp
	}
	return filepath.Abs(exe)
}

// envKey is the name of a "K=V" entry; Windows keeps per-drive working directories as "=C:=C:\dir", whose name
// starts with "=". "" for a malformed entry.
func envKey(kv string) string {
	start := 0
	if strings.HasPrefix(kv, "=") {
		start = 1
	}
	i := strings.IndexByte(kv[start:], '=')
	if i < 0 {
		return ""
	}
	return kv[:start+i]
}

// childEnv is the environment block for a child, prepared like os/exec's Cmd.environ (syscall.StartProcess does
// none of it): ours when env is nil; duplicates dropped with the last one winning, names compared case-insensitively
// as Windows does (a caller appending "ANDROID_SDK_ROOT=..." to os.Environ() would otherwise hand the child both,
// and Windows returns the first); SYSTEMROOT added when missing, without which some system DLLs fail to load.
func childEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	seen := make(map[string]bool, len(env))
	out := make([]string, 0, len(env)+1)
	for i := len(env) - 1; i >= 0; i-- {
		k := strings.ToUpper(envKey(env[i]))
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, env[i])
	}
	slices.Reverse(out)
	if !seen["SYSTEMROOT"] {
		if v := os.Getenv("SYSTEMROOT"); v != "" {
			out = append(out, "SYSTEMROOT="+v)
		}
	}
	return out
}

// openOutput opens path for appending, creating it and its directory. FILE_APPEND_DATA access (Go's O_APPEND), so
// the child's writes go to the end even when several processes share the file.
func openOutput(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
}

// startProcess is StartDetached returning the process handle too (the caller closes or keeps it).
//
// Handle inheritance: syscall.StartProcess (go1.26.5 src/syscall/exec_windows.go) duplicates exactly the three
// ProcAttr.Files as inheritable handles and passes only those in PROC_THREAD_ATTRIBUTE_HANDLE_LIST ("Do not
// accidentally inherit more than these handles"), so the child gets NUL, the output file and nothing else of ours,
// even inheritable handles such as os.Pipe ends or another goroutine's in-flight pipes. This is what Python's
// Popen(close_fds=True) gives AUTO-MAS (host.py: an emulator holding our output pipe would block the reader).
func startProcess(exe string, args []string, opt StartOptions) (Process, syscall.Handle, error) {
	name := filepath.Base(exe)
	fail := func(err error) (Process, syscall.Handle, error) {
		return Process{}, 0, errs.Wrap(errs.LaunchFailed, err, fmt.Sprintf("启动 %s 失败: %v", name, err))
	}
	exe, err := resolveExe(exe)
	if err != nil {
		return fail(err)
	}
	name = filepath.Base(exe)
	dir := opt.Dir
	if dir == "" {
		dir = filepath.Dir(exe)
	}

	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return fail(err)
	}
	defer stdin.Close()
	out := opt.Output
	if out == nil {
		var f *os.File
		if opt.OutputPath != "" {
			f, err = openOutput(opt.OutputPath)
		} else {
			f, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		}
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		out = f
	}

	attr := &syscall.ProcAttr{
		Dir:   dir,
		Env:   childEnv(opt.Env),
		Files: []uintptr{stdin.Fd(), out.Fd(), out.Fd()},
		Sys:   &syscall.SysProcAttr{CreationFlags: detachedFlags},
	}
	argv := append([]string{exe}, args...)
	pid, h, err := syscall.StartProcess(exe, argv, attr)
	if errors.Is(err, syscall.ERROR_ACCESS_DENIED) {
		// The job we run in forbids breakaway (AUTO-MAS popen_emulator: WinError 5, retry without the flag).
		attr.Sys.CreationFlags &^= createBreakawayFromJob
		pid, h, err = syscall.StartProcess(exe, argv, attr)
	}
	if err != nil {
		return fail(err)
	}
	handle := syscall.Handle(h)
	created, _, _, _, _ := handleTimes(handle)
	return Process{PID: uint32(pid), PPID: uint32(os.Getpid()), Name: name, Created: created}, handle, nil
}

// StartDetached starts a process that inherits no handle of ours except Output (and NUL for stdin), has no console
// window and survives our exit: CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP | CREATE_BREAKAWAY_FROM_JOB; when the
// job we are in forbids breakaway (ERROR_ACCESS_DENIED), retries once without CREATE_BREAKAWAY_FROM_JOB (AUTO-MAS
// popen_emulator). Returns the started process (Created filled in) without waiting.
//
// Nothing waits on the child; its handle stays in this package only so ExitCode works after it exits (keepHandle).
func StartDetached(exe string, args []string, opt StartOptions) (Process, error) {
	p, h, err := startProcess(exe, args, opt)
	if err != nil {
		return Process{}, err
	}
	keepHandle(h)
	return p, nil
}

// kept are handles of processes this process started. Holding one keeps the process object, and with it the exit
// code, after the process exits (Popen.poll() semantics for ExitCode); it also stops the PID being reused meanwhile.
// Handles of processes that exited more than keepExitedHandle ago are closed on the next keepHandle, so a long-lived
// MCP server does not accumulate them.
var kept struct {
	sync.Mutex
	handles []syscall.Handle
}

func keepHandle(h syscall.Handle) {
	kept.Lock()
	defer kept.Unlock()
	cutoff := filetimeValue(syscall.NsecToFiletime(time.Now().Add(-keepExitedHandle).UnixNano()))
	live := kept.handles[:0]
	for _, k := range kept.handles {
		if !running(k) {
			if _, exited, _, _, err := handleTimes(k); err != nil || exited < cutoff {
				syscall.CloseHandle(k)
				continue
			}
		}
		live = append(live, k)
	}
	kept.handles = append(live, h)
}

// spawnRequest is the one argument after "__spawn".
type spawnRequest struct {
	Exe        string   `json:"exe"`
	Args       []string `json:"args"`
	Dir        string   `json:"dir,omitempty"`
	OutputPath string   `json:"output_path,omitempty"`
	// Parent receives a duplicate of the child's handle, so it can read the child's exit code even when the child
	// exits before the parent could open it.
	Parent uint32 `json:"parent,omitempty"`
}

// spawnReply is the helper's one stdout line.
type spawnReply struct {
	PID     uint32 `json:"pid,omitempty"`
	Created uint64 `json:"created,omitempty"`
	Handle  uint64 `json:"handle,omitempty"` // the duplicate in Parent; 0 when that failed
	Error   string `json:"error,omitempty"`
}

// StartOrphan starts exe so that it is not our descendant: it runs `<OrphanHelper> __spawn` (itself started with
// StartDetached), which starts exe with StartDetached semantics, reports the child to us and exits at once. The
// child's parent PID then names a dead process, so tools that end "the process tree" of modavd (Claude Code ends an
// MCP server's whole tree on disconnect; AUTO-MAS Runtime's job object) cannot reach the emulator: ending qemu
// without a guest shutdown is a power cut. Output must be given as opt.OutputPath (opened by the helper,
// appended to); opt.Output is not allowed here. The helper gets opt.Env and passes it on. Returns the grandchild.
//
// Protocol: one JSON argument (spawnRequest) after "__spawn"; the helper's stdout is a pipe to us carrying one JSON
// line (spawnReply). We wait at most 15 s for the line and for the helper to exit, so the returned process's parent
// is gone when we return. Returned PPID is the helper's PID.
func StartOrphan(exe string, args []string, opt StartOptions) (Process, error) {
	if opt.Output != nil {
		return Process{}, errs.New(errs.Internal, "StartOrphan 只能用 OutputPath 指定输出文件")
	}
	if OrphanHelper == "" {
		return StartDetached(exe, args, opt)
	}
	name := filepath.Base(exe)
	fail := func(err error) (Process, error) {
		return Process{}, errs.Wrap(errs.LaunchFailed, err, fmt.Sprintf("启动 %s 失败: %v", name, err))
	}
	abs, err := resolveExe(exe)
	if err != nil {
		return fail(err)
	}
	name = filepath.Base(abs)
	req := spawnRequest{Exe: abs, Args: args, Dir: opt.Dir, OutputPath: opt.OutputPath, Parent: uint32(os.Getpid())}
	// The helper runs in its own directory: make the paths absolute against ours.
	if req.Dir == "" {
		req.Dir = filepath.Dir(abs)
	} else if req.Dir, err = filepath.Abs(req.Dir); err != nil {
		return fail(err)
	}
	if req.OutputPath != "" {
		if req.OutputPath, err = filepath.Abs(req.OutputPath); err != nil {
			return fail(err)
		}
	}
	if req.Args == nil {
		req.Args = []string{}
	}
	arg, err := json.Marshal(req)
	if err != nil {
		return fail(err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	defer r.Close()
	helper, hh, err := startProcess(OrphanHelper, []string{SpawnHelperArg, string(arg)}, StartOptions{Env: opt.Env, Output: w})
	w.Close() // the helper holds the only write end now: its exit ends our read
	if err != nil {
		return Process{}, err
	}
	defer syscall.CloseHandle(hh)

	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(io.LimitReader(r, 1<<16)).ReadString('\n')
		lines <- line
	}()
	deadline := time.Now().Add(orphanHelperTimeout)
	timer := time.NewTimer(orphanHelperTimeout)
	defer timer.Stop()
	var line string
	select {
	case line = <-lines:
	case <-timer.C:
		syscall.TerminateProcess(hh, 1)
		return Process{}, errs.Newf(errs.LaunchFailed, "启动 %s 失败：中转进程 %.0f 秒内没有回报", name,
			orphanHelperTimeout.Seconds())
	}
	if ev, _ := syscall.WaitForSingleObject(hh, waitMillis(time.Until(deadline))); ev != waitObject0 {
		// Reported but hangs: end it, the child is already on its own.
		syscall.TerminateProcess(hh, 1)
	}

	var rep spawnReply
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &rep); err != nil || (rep.Error == "" && rep.PID == 0) {
		var code uint32
		syscall.GetExitCodeProcess(hh, &code)
		return Process{}, errs.Newf(errs.LaunchFailed, "启动 %s 失败：中转进程没有正常回报（退出码 %d）: %s", name, code,
			strings.TrimSpace(line))
	}
	if rep.Error != "" {
		return Process{}, errs.New(errs.LaunchFailed, rep.Error)
	}
	p := Process{PID: rep.PID, PPID: helper.PID, Name: name, Created: rep.Created}
	if !adoptHandle(rep, p) {
		if h, err := p.open(syscall.SYNCHRONIZE); err == nil {
			keepHandle(h)
		}
	}
	return p, nil
}

// adoptHandle keeps the handle the helper duplicated into us when it really is p's.
func adoptHandle(rep spawnReply, p Process) bool {
	if rep.Handle == 0 {
		return false
	}
	h := syscall.Handle(rep.Handle)
	if pid, _ := call(procGetProcessId, uintptr(h)); uint32(pid) != p.PID {
		return false // not a handle of that process: not ours to close either
	}
	if c, _, _, _, err := handleTimes(h); err != nil || c != p.Created {
		syscall.CloseHandle(h)
		return false
	}
	keepHandle(h)
	return true
}

// duplicateInto copies h into process pid and returns the value there, 0 on failure.
func duplicateInto(pid uint32, h syscall.Handle) uint64 {
	target, err := syscall.OpenProcess(processDupHandle, false, pid)
	if err != nil {
		return 0
	}
	defer syscall.CloseHandle(target)
	self, _ := syscall.GetCurrentProcess()
	var dup syscall.Handle
	if err := syscall.DuplicateHandle(self, h, target, &dup, 0, false, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		return 0
	}
	return uint64(dup)
}

// SpawnHelperMain is the helper side of StartOrphan: args are what follows "__spawn". It writes exactly one JSON line
// to stdout ({"pid":N,"created":N} or {"error":"..."}) and returns the exit code (0 / 1).
func SpawnHelperMain(args []string, stdout *os.File) int {
	reply := func(rep spawnReply) int {
		b, _ := json.Marshal(rep)
		stdout.Write(append(b, '\n'))
		if rep.Error != "" {
			return 1
		}
		return 0
	}
	if len(args) != 1 {
		return reply(spawnReply{Error: fmt.Sprintf("中转进程要一个 JSON 参数，收到 %d 个", len(args))})
	}
	var req spawnRequest
	if err := json.Unmarshal([]byte(args[0]), &req); err != nil {
		return reply(spawnReply{Error: fmt.Sprintf("中转进程的参数不是合法的 JSON: %v", err)})
	}
	if req.Exe == "" {
		return reply(spawnReply{Error: "中转进程的参数里没有要启动的程序"})
	}
	// Env nil: the child gets the helper's environment, which is the caller's opt.Env.
	p, h, err := startProcess(req.Exe, req.Args, StartOptions{Dir: req.Dir, OutputPath: req.OutputPath})
	if err != nil {
		return reply(spawnReply{Error: errs.As(err).Message})
	}
	defer syscall.CloseHandle(h)
	rep := spawnReply{PID: p.PID, Created: p.Created}
	if req.Parent != 0 {
		rep.Handle = duplicateInto(req.Parent, h)
	}
	return reply(rep)
}
