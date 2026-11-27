//go:build darwin && cgo

package macos

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
	darwinOrphanTimeout = 15 * time.Second
	darwinKeepExited    = 10 * time.Minute
)

type darwinChildKey struct {
	pid     uint32
	created uint64
}

type darwinChild struct {
	done     chan struct{}
	process  *os.Process
	code     uint32
	known    bool
	exitedAt time.Time
}

var darwinChildren = struct {
	sync.Mutex
	items map[darwinChildKey]*darwinChild
}{items: make(map[darwinChildKey]*darwinChild)}

// Only a direct child's one cmd.Wait goroutine can recover its wait status.
// Orphans and processes discovered later deliberately have no cached exit code.
func darwinRememberChild(cmd *exec.Cmd, p Process) {
	record := &darwinChild{done: make(chan struct{}), process: cmd.Process}
	darwinChildren.Lock()
	darwinPruneChildren()
	darwinChildren.items[darwinChildKey{p.PID, p.Created}] = record
	darwinChildren.Unlock()
	go func() {
		_ = cmd.Wait() // also reap unsuccessful and signaled children
		var code uint32
		known := false
		if state := cmd.ProcessState; state != nil {
			if status, ok := state.Sys().(syscall.WaitStatus); ok {
				switch {
				case status.Exited():
					code, known = uint32(status.ExitStatus()), true
				case status.Signaled():
					// The public API is unsigned: use the shell convention instead
					// of converting Go's -1 for a signal to an enormous exit code.
					code, known = uint32(128+status.Signal()), true
				}
			}
		}
		darwinChildren.Lock()
		record.code, record.known, record.exitedAt = code, known, time.Now()
		close(record.done)
		darwinChildren.Unlock()
	}()
}

// darwinPruneChildren is called with darwinChildren locked. Like Windows' kept
// handles, completed records are retained for ten minutes and pruned on use.
func darwinPruneChildren() {
	cutoff := time.Now().Add(-darwinKeepExited)
	for key, record := range darwinChildren.items {
		if !record.exitedAt.IsZero() && record.exitedAt.Before(cutoff) {
			delete(darwinChildren.items, key)
		}
	}
}

func darwinChildOf(p Process) *darwinChild {
	if p.PID == 0 || p.Created == 0 {
		return nil
	}
	darwinChildren.Lock()
	defer darwinChildren.Unlock()
	darwinPruneChildren()
	return darwinChildren.items[darwinChildKey{p.PID, p.Created}]
}

// childExit is for Process.ExitCode. It never supplies an exit code from another
// process that happened to reuse a PID.
func childExit(p Process) (uint32, bool) {
	record := darwinChildOf(p)
	if record == nil {
		return 0, false
	}
	select {
	case <-record.done:
		return record.code, record.known
	default:
		return 0, false
	}
}

// childWait is for Process.Wait: known says this package owns the child and its
// wait goroutine; done says that goroutine has reaped it within timeout.
func childWait(p Process, timeout time.Duration) (done, known bool) {
	record := darwinChildOf(p)
	if record == nil {
		return false, false
	}
	select {
	case <-record.done:
		return true, true
	default:
	}
	if timeout <= 0 {
		return false, true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-record.done:
		return true, true
	case <-timer.C:
		return false, true
	}
}

func darwinResolveExe(exe string) (string, error) {
	if exe == "" {
		return "", errors.New("没有给出要启动的程序")
	}
	if !strings.ContainsRune(exe, '/') {
		path, err := exec.LookPath(exe)
		if err != nil {
			return "", err
		}
		exe = path
	}
	return filepath.Abs(exe)
}

// Unix environment keys are case-sensitive; as with exec.Cmd, the last duplicate
// wins. A non-nil empty slice means an empty environment, not inheritance.
func darwinChildEnv(env []string) []string {
	if env == nil {
		env = os.Environ()
	}
	seen := make(map[string]bool, len(env))
	out := make([]string, 0, len(env))
	for i := len(env) - 1; i >= 0; i-- {
		key, _, ok := strings.Cut(env[i], "=")
		if !ok || key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, env[i])
	}
	slices.Reverse(out)
	return out
}

func darwinLaunchError(exe string, err error) error {
	return errs.Wrap(errs.LaunchFailed, err, fmt.Sprintf("启动 %s 失败: %v", filepath.Base(exe), err))
}

// StartDetached creates a new session and process group, with only stdin (null),
// stdout and stderr inherited. The child survives this process's normal exit.
// It remains our child until reaped; StartOrphan additionally removes ancestry.
func StartDetached(exe string, args []string, opt StartOptions) (Process, error) {
	abs, err := darwinResolveExe(exe)
	if err != nil {
		return Process{}, darwinLaunchError(exe, err)
	}
	cmd := exec.Command(abs, args...)
	cmd.Dir = opt.Dir
	if cmd.Dir == "" {
		cmd.Dir = filepath.Dir(abs)
	}
	cmd.Env = darwinChildEnv(opt.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	input, err := os.Open(os.DevNull)
	if err != nil {
		return Process{}, darwinLaunchError(abs, err)
	}
	defer input.Close()
	output := opt.Output
	if output == nil {
		path := opt.OutputPath
		if path == "" {
			path = os.DevNull
		} else if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return Process{}, darwinLaunchError(abs, err)
		}
		output, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return Process{}, darwinLaunchError(abs, err)
		}
		defer output.Close()
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, output
	if err := cmd.Start(); err != nil {
		return Process{}, darwinLaunchError(abs, err)
	}
	// Do not reap before reading creation time: even a very short lived child is
	// still ours as a zombie, so its PID cannot already identify an unrelated one.
	p, ok := startedProcess(uint32(cmd.Process.Pid))
	if !ok || p.Created == 0 {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return Process{}, darwinLaunchError(abs, errors.New("读不到新进程的创建时间"))
	}
	p.Name = filepath.Base(abs)
	darwinRememberChild(cmd, p)
	return p, nil
}

type darwinSpawnRequest struct {
	Exe        string   `json:"exe"`
	Args       []string `json:"args"`
	Dir        string   `json:"dir,omitempty"`
	OutputPath string   `json:"output_path,omitempty"`
}

type darwinSpawnReply struct {
	PID     uint32 `json:"pid,omitempty"`
	Created uint64 `json:"created,omitempty"`
	Error   string `json:"error,omitempty"`
}

// StartOrphan runs the existing __spawn protocol in a short lived helper. It
// returns only after that helper has been reaped and a surviving target has
// moved to launchd (PID 1), in its own process group. Its eventual exit code is
// unknown to us: it is not our child and no success status is fabricated.
// An empty helper falls back to StartDetached.
func StartOrphan(exe string, args []string, opt StartOptions, helperPath string) (Process, error) {
	if opt.Output != nil {
		return Process{}, errs.New(errs.Internal, "StartOrphan 只能用 OutputPath 指定输出文件")
	}
	if helperPath == "" {
		return StartDetached(exe, args, opt)
	}
	abs, err := darwinResolveExe(exe)
	if err != nil {
		return Process{}, darwinLaunchError(exe, err)
	}
	req := darwinSpawnRequest{Exe: abs, Args: args, Dir: opt.Dir, OutputPath: opt.OutputPath}
	if req.Dir == "" {
		req.Dir = filepath.Dir(abs)
	} else if req.Dir, err = filepath.Abs(req.Dir); err != nil {
		return Process{}, darwinLaunchError(abs, err)
	}
	if req.OutputPath != "" {
		if req.OutputPath, err = filepath.Abs(req.OutputPath); err != nil {
			return Process{}, darwinLaunchError(abs, err)
		}
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return Process{}, darwinLaunchError(abs, err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		return Process{}, darwinLaunchError(abs, err)
	}
	defer reader.Close()
	cmd := exec.Command(helperPath, SpawnHelperArg, string(encoded))
	cmd.Env = darwinChildEnv(opt.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdout, cmd.Stderr = writer, writer
	if err := cmd.Start(); err != nil {
		writer.Close()
		return Process{}, darwinLaunchError(abs, err)
	}
	writer.Close()
	helper, helperKnown := startedProcess(uint32(cmd.Process.Pid))
	waited := make(chan error, 1)
	helperDone := make(chan struct{})
	go func() {
		waited <- cmd.Wait()
		close(helperDone)
	}()
	deadline := time.Now().Add(darwinOrphanTimeout)
	var target Process
	failed := func(err error) (Process, error) {
		// Capture any children before ending a helper that has not reported yet.
		// Identity checks in KillTree/Kill avoid acting on a reused helper PID.
		if helperKnown && helper.Created != 0 {
			_ = KillTree(helper)
		}
		if target.PID != 0 && target.Created != 0 {
			_ = target.Kill()
		}
		_ = cmd.Process.Kill()
		select {
		case <-helperDone:
		case <-time.After(time.Second):
		}
		return Process{}, darwinLaunchError(abs, err)
	}
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult, 1)
	go func() {
		line, err := bufio.NewReader(io.LimitReader(reader, 1<<16+1)).ReadString('\n')
		lines <- lineResult{line, err}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var line lineResult
	select {
	case line = <-lines:
	case <-timer.C:
		return failed(errors.New("中转进程 15 秒内没有回报"))
	}
	var reply darwinSpawnReply
	if line.err != nil || len(line.line) > 1<<16 || json.Unmarshal([]byte(line.line), &reply) != nil ||
		(reply.Error == "" && (reply.PID == 0 || reply.Created == 0)) {
		return failed(errors.New("中转进程没有正常回报"))
	}
	if reply.PID != 0 && reply.Created != 0 {
		if reply.PID == uint32(os.Getpid()) || reply.PID == uint32(cmd.Process.Pid) {
			return failed(errors.New("中转进程回报了无效的子进程身份"))
		}
		target = Process{PID: reply.PID, PPID: uint32(cmd.Process.Pid), Name: filepath.Base(abs), Created: reply.Created}
	}
	select {
	case err := <-waited:
		if reply.Error != "" {
			return Process{}, errs.New(errs.LaunchFailed, reply.Error)
		}
		if err != nil {
			return failed(fmt.Errorf("中转进程退出失败: %w", err))
		}
	case <-timer.C:
		return failed(errors.New("中转进程 15 秒内没有退出"))
	}
	for {
		info, err := target.checkedInfo()
		if errors.Is(err, os.ErrProcessDone) {
			// The reported process has already exited. Returning its original
			// identity lets callers observe that, without touching a reused PID.
			return target, nil
		}
		if err != nil {
			return failed(fmt.Errorf("无法验证新进程的身份: %w", err))
		}
		current := processFromInfo(info)
		group, err := syscall.Getpgid(int(target.PID))
		if current.PPID == 1 && err == nil && group == int(target.PID) {
			afterInfo, err := target.checkedInfo()
			if errors.Is(err, os.ErrProcessDone) {
				return target, nil
			}
			if err != nil {
				return failed(fmt.Errorf("无法再次验证新进程的身份: %w", err))
			}
			if processFromInfo(afterInfo).PPID == 1 {
				target.PPID = 1
				return target, nil
			}
		}
		if !time.Now().Before(deadline) {
			return failed(errors.New("新进程没有脱离中转进程和调用者的进程组"))
		}
		time.Sleep(min(10*time.Millisecond, time.Until(deadline)))
	}
}

// SpawnHelperMain takes the same one JSON argument as the Windows helper and
// writes exactly one JSON reply. Target stdout/stderr never use the reply pipe.
func SpawnHelperMain(args []string, stdout *os.File) int {
	reply := func(rep darwinSpawnReply) bool {
		return json.NewEncoder(stdout).Encode(rep) == nil
	}
	if len(args) != 1 {
		reply(darwinSpawnReply{Error: fmt.Sprintf("中转进程要一个 JSON 参数，收到 %d 个", len(args))})
		return 1
	}
	var req darwinSpawnRequest
	if err := json.Unmarshal([]byte(args[0]), &req); err != nil || req.Exe == "" {
		reply(darwinSpawnReply{Error: "中转进程的 JSON 参数无效或没有要启动的程序"})
		return 1
	}
	p, err := StartDetached(req.Exe, req.Args, StartOptions{Dir: req.Dir, OutputPath: req.OutputPath})
	if err != nil {
		reply(darwinSpawnReply{Error: errs.As(err).Message})
		return 1
	}
	if !reply(darwinSpawnReply{PID: p.PID, Created: p.Created}) {
		if child := darwinChildOf(p); child != nil {
			_ = child.process.Kill()
			<-child.done
		}
		return 1
	}
	return 0
}
