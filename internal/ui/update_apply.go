package ui

// 设置 → 更新 (API.md "更新"): the manager updates its own root. It cannot replace itself while it runs, so the job
// (kind "update") prepares (downloads, verifies and stages the new version; instances may keep running meanwhile),
// stops the running instances, then hands the swap to the root's tools\modavd.exe
//
//	modavd --root <root> --json update install --after-pid <our pid> --reopen <our exe> --reopen-arg ...
//
// (update/handoff.go) and the manager exits. The CLI swaps once we are gone, leaves its result in .update and starts
// the manager again, which shows that result (updateView.Handoff).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/logx"
	"modavd/internal/update"
)

const (
	kindUpdate = "update"
	// handoffExitDelay: how long after the hand-off job ended the manager exits, so the page sees the job end and
	// closes its window first.
	handoffExitDelay  = 1500 * time.Millisecond
	cliVersionTimeout = 15 * time.Second
	// handoffAliveCheck: how long after starting the CLI we look whether it still runs.
	handoffAliveCheck = 500 * time.Millisecond
	// updateLogName: the CLI's output during the hand-off, under logs\ (appended).
	updateLogName = "modavd-update.log"
)

// Reopen is how the CLI starts this manager again after the swap: our executable and the arguments it was started
// with that matter (the root, --port, --no-window, --dev). Nil: the manager cannot update itself (tests, or when
// ModAVD.exe could not tell its own path).
type Reopen struct {
	Exe  string
	Args []string
}

// updateResult is the update job's result.
type updateResult struct {
	Handoff bool   `json:"handoff"` // the CLI took over: the manager exits now and is started again after the swap
	Version string `json:"version"` // the version handed over, vX.Y.Z ("" when already up to date)
	Stopped []int  `json:"stopped"` // the instances stopped for the swap
	Message string `json:"message"`
}

func (s *server) updateCLI() string { return filepath.Join(s.root.Dir, "tools", "modavd.exe") }

// applyReady is what the 更新 refuses at once, before a job: no way to come back, no CLI of our version to hand to.
func (s *server) applyReady() error {
	cliAdvice := `关掉管理器后运行 tools\modavd.exe update install`
	if !s.canInstall() {
		return errs.New(errs.Unsupported, "这个管理器不能自己更新").WithAdvice(cliAdvice)
	}
	cli := s.updateCLI()
	if fi, err := os.Stat(cli); err != nil || fi.IsDir() {
		return errs.New(errs.NotReady, `根目录里没有 tools\modavd.exe，管理器没法交给它换上新版`).
			WithAdvice("从 ModAVD 的发布页下载新版整包，关掉所有实例和管理器后整个覆盖解压")
	}
	v, err := s.cliVersion(cli)
	if err != nil {
		return errs.Wrap(errs.NotReady, err, `运行不了 tools\modavd.exe：`+err.Error()).WithAdvice(cliAdvice)
	}
	if v != buildinfo.Version {
		return errs.Newf(errs.NotReady, `tools\modavd.exe 的版本（%s）和管理器（%s）不一样，不能从管理器更新`, v, buildinfo.Version).
			WithAdvice(cliAdvice)
	}
	return nil
}

func (s *server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	if err := s.applyReady(); err != nil {
		writeError(w, err)
		return
	}
	s.respondJob(w)(s.startRootJob(kindUpdate, s.updateJob))
}

// updateJob: prepare, stop the instances, hand off; then the manager exits (handoffExit).
func (s *server) updateJob(ctx context.Context, j *job, b backend) (any, error) {
	lg := logx.New(s.jobLogSink(j))
	st, err := s.prepareUpdate(ctx, lg, func(p update.Progress) { s.setDiagProgress(j, updateProgress(p)) })
	if st != nil && st.Checked {
		s.upd.mu.Lock()
		s.upd.checked, s.upd.at, s.upd.err = st, time.Now(), nil
		s.upd.mu.Unlock()
	}
	s.hub.broadcast("update", s.updateView())
	if err != nil {
		return nil, err
	}
	if !stagedNewer(st) {
		msg := st.Message
		if msg == "" || st.PreparedVersion != "" {
			msg = "已经是最新版本"
		}
		return &updateResult{Stopped: []int{}, Message: msg}, nil
	}
	shown := strings.TrimPrefix(st.PreparedVersion, "v")
	if err := s.beforeStopping(ctx, b, shown); err != nil {
		return nil, err
	}
	stopped, err := s.stopForUpdate(ctx, j, b, shown)
	if err != nil {
		return nil, err
	}
	s.setProgress(j, jobProgress{Step: "handoff", Message: "正在交给命令行换上 " + shown})
	cliAdvice := shown + ` 已下载并核对好；关掉管理器后运行 tools\modavd.exe update install，不用重新下载`
	alive, err := s.handOff(lg)
	if err != nil {
		return nil, errs.Wrap(errs.LaunchFailed, err, `启动不了 tools\modavd.exe 来换上新版：`+errs.As(err).Message).
			WithAdvice(cliAdvice)
	}
	// It waits for us from its first step: gone already means it failed (a crash, an antivirus), and nobody would
	// start the manager again.
	select {
	case <-time.After(handoffAliveCheck):
	case <-ctx.Done():
		// we stay: the CLI gives up after update.AfterPIDWait, swaps nothing and only brings our window back
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
	if !alive() {
		return nil, errs.New(errs.LaunchFailed, `tools\modavd.exe 启动后马上退出了，新版没有换上`).
			WithAdvice(cliAdvice + `；原因看 logs\` + updateLogName)
	}
	s.handingOff.Store(true)
	go s.handoffExit(j)
	return &updateResult{Handoff: true, Version: st.PreparedVersion, Stopped: stopped,
		Message: "管理器会关闭，换上 " + shown + " 后自动重新打开"}, nil
}

// stagedNewer: a version is staged and it is newer than the one installed (a staged one may be older, when the
// package was copied over by hand since).
func stagedNewer(st *update.Status) bool {
	staged, ok := update.ParseVersion(st.PreparedVersion)
	if !ok {
		return false
	}
	cur, ok := update.ParseVersion(st.CurrentVersion)
	return !ok || cur.Less(staged)
}

// beforeStopping refuses what would make the swap fail after the instances were stopped for nothing: another
// program of the root running (an MCP server, another modavd), an instance being created (it must not be cancelled).
func (s *server) beforeStopping(ctx context.Context, b backend, version string) error {
	advice := version + " 已下载并核对好；处理好后再点「更新」，不用重新下载"
	progs, err := s.programsInUse()
	if err != nil {
		return errs.Wrap(errs.Internal, err, "查不了有没有别的程序在用这个根目录，一台实例都没关："+err.Error())
	}
	if len(progs) > 0 {
		return errs.Newf(errs.Busy, "还有程序在用这个根目录的 modavd：%s。一台实例都没关", strings.Join(progs, "、")).
			WithAdvice("先关掉它们（比如用 modavd mcp 的 AI 助手、别的命令行窗口）；" + advice)
	}
	list, err := b.List(ctx)
	if err != nil {
		return err
	}
	for _, in := range list.Instances {
		if in.Status == emu.StatusCreating {
			return errs.Newf(errs.Busy, "实例 %d 正在新建，一台实例都没关", in.Index).WithAdvice("等它建完；" + advice)
		}
	}
	return nil
}

// updateProgress is an update step as the job's progress line (a download's line has the percentage and sizes).
func updateProgress(p update.Progress) DiagProgress {
	msg := p.Message
	if p.Stage == "downloading" && p.Total > 0 {
		msg = fmt.Sprintf("%s：%.0f%%（%.1f / %.1f MB）", p.Message, p.Percent, float64(p.Done)/(1<<20), float64(p.Total)/(1<<20))
	}
	return DiagProgress{Stage: p.Stage, Message: msg, Done: p.Done, Total: p.Total}
}

// stopForUpdate stops every running instance of the root at once, the way `modavd stop --all` does, but never one
// still being created (that stop fails, and so does the update: the user finishes or cancels the creation first).
func (s *server) stopForUpdate(ctx context.Context, j *job, b backend, version string) ([]int, error) {
	targets, err := b.StopTargets(ctx)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return []int{}, nil
	}
	names := make([]string, len(targets))
	for k, i := range targets {
		names[k] = strconv.Itoa(i)
	}
	s.setProgress(j, jobProgress{Step: "stopping", Message: "正在正常关掉实例 " + strings.Join(names, "、")})
	fails := make([]error, len(targets))
	var wg sync.WaitGroup
	for k, i := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, fails[k] = b.Stop(ctx, i, emu.StopOptions{})
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return targets, errs.Wrap(errs.Canceled, err, "已取消")
	}
	var failed []string
	for k, e := range fails {
		if e != nil {
			failed = append(failed, fmt.Sprintf("实例 %d：%s", targets[k], errs.As(e).Message))
		}
	}
	if len(failed) > 0 {
		return targets, errs.Newf(errs.Busy, "有实例没关掉，新版没有换上（%s）", strings.Join(failed, "；")).
			WithAdvice(version + " 已下载并核对好；处理好后再点「更新」，不用重新下载")
	}
	return targets, nil
}

// handOff starts the CLI that swaps once we are gone and starts us again; it is not our descendant (host.StartOrphan),
// so nothing that ends this process can end it. alive tells whether it still runs.
func (s *server) handOff(lg *logx.Logger) (alive func() bool, err error) {
	args := []string{"--root", s.root.Dir, "--json", "update", "install",
		"--after-pid", strconv.Itoa(os.Getpid()), "--reopen", s.reopen.Exe}
	for _, a := range s.reopen.Args {
		args = append(args, "--reopen-arg", a)
	}
	logPath := filepath.Join(s.root.LogsDir(), updateLogName)
	lg.Infof("交给命令行换上新版：%s %s（输出在 %s）", s.updateCLI(), strings.Join(args, " "), logPath)
	return s.spawn(s.updateCLI(), args, logPath)
}

// handoffExit ends the manager once the job's end has been sent (the CLI waits for us to exit).
func (s *server) handoffExit(j *job) {
	<-j.done
	select {
	case <-time.After(handoffExitDelay):
	case <-s.ctx.Done():
	}
	s.log.Infof("新版交给命令行去换，管理器退出")
	s.requestExit()
}

func (s *server) requestExit() { s.exitOnce.Do(func() { close(s.exitReq) }) }

// ---- the real hand-off steps (Options can replace them in tests)

// realCLIVersion is `<exe> --json version`'s version (no window: it is a console program).
func realCLIVersion(exe string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), cliVersionTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "--json", "version")
	host.HideConsoleCmd(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if json.Unmarshal(out, &env) != nil || !env.OK || env.Data.Version == "" {
		return "", fmt.Errorf("看不懂它的输出：%q", strings.TrimSpace(string(out)))
	}
	return env.Data.Version, nil
}

func realSpawn(exe string, args []string, logPath string) (func() bool, error) {
	p, err := host.StartOrphan(exe, args, host.StartOptions{Dir: filepath.Dir(exe), OutputPath: logPath})
	if err != nil {
		return nil, err
	}
	return p.Alive, nil
}
