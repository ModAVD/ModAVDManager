//go:build windows

// Package update updates the ModAVD package of a root (emulator, components, scripts and this manager) from GitHub
// or MirrorChyan, the way AUTO-MAS's own updater does and on the same files under <root>\.update (state.go), so the
// two can take over each other's half-done work while both exist. AUTO-MAS keeps its updater for two more major
// versions and then calls `modavd update` instead; until then the manager only shows the settings (source, CDK) and
// the state, and only the command line installs.
//
// Nothing here runs on its own: no check at start-up, no background download. Each step is asked for: check (ask the
// chosen source), prepare (download, unpack, verify, stage — instances may run), install (prepare if needed, then
// swap sdk\emulator and the package's other files; every instance must be off), recover (finish or undo a swap that
// was cut off). Each step first recovers.
package update

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// minFreeBytes is what an update may take at once on the root's drive: download, stage and the old version.
const minFreeBytes = 1825361100

// Progress is one step of a running update.
type Progress struct {
	Stage   string  `json:"stage"` // checking / downloading / extracting / verifying / installing
	Message string  `json:"message"`
	Percent float64 `json:"percent"` // -1 when unknown
	Done    int64   `json:"done_bytes"`
	Total   int64   `json:"total_bytes"`
}

// Options of an update run.
type Options struct {
	// AutoMASVersion is the calling AUTO-MAS's version; a release that needs a newer one is not installed. "" (a
	// person, the manager) skips that check.
	AutoMASVersion string
	// Progress, when set, is told about each step.
	Progress func(Progress)
}

// Updater updates one root.
type Updater struct {
	Root *root.Root
	Log  *logx.Logger
	Opt  Options
	l    layout

	// test hooks
	githubAPI, mirrorAPI string
	httpClient           *http.Client
	scan                 func(programs bool) ([]processUse, error)
}

// New is the updater of r.
func New(r *root.Root, log *logx.Logger, opt Options) *Updater {
	if log == nil {
		log = logx.Nop()
	}
	return &Updater{Root: r, Log: log, Opt: opt, l: layout{root: r.Dir}}
}

func (u *Updater) progress(stage, msg string, percent float64, done, total int64) {
	if u.Opt.Progress != nil {
		u.Opt.Progress(Progress{Stage: stage, Message: msg, Percent: percent, Done: done, Total: total})
	}
}

func (u *Updater) step(stage, msg string) {
	u.Log.Infof("%s", msg)
	u.progress(stage, msg, -1, 0, 0)
}

// Status is the result of every update command (`modavd update --json`, the manager's settings). All fields are
// always present.
type Status struct {
	Root               string   `json:"root"`
	Updatable          bool     `json:"updatable"`
	NotUpdatableReason string   `json:"not_updatable_reason"`
	CurrentVersion     string   `json:"current_version"` // vX.Y.Z, "" when unknown
	CurrentBuild       string   `json:"current_build"`   // mas-N, "" when unknown
	Source             string   `json:"source"`          // the setting: github / mirrorchyan
	CDKSet             bool     `json:"cdk_set"`         // a MirrorChyan CDK is set (never the CDK itself)
	Checked            bool     `json:"checked"`         // a source was asked and answered
	CheckError         string   `json:"check_error"`
	LatestVersion      string   `json:"latest_version"` // vX.Y.Z of the latest release, "" when not asked
	LatestSource       string   `json:"latest_source"`
	UpdateAvailable    bool     `json:"update_available"`
	RequiredAutoMAS    string   `json:"required_automas"` // the latest release's min_automas, "" when none
	ReleaseURL         string   `json:"release_url"`
	Phase              string   `json:"phase"`            // idle / preparing / staged / swapping (.update\state.json)
	PreparedVersion    string   `json:"prepared_version"` // vX.Y.Z downloaded and verified, waiting for install
	LastResult         any      `json:"last_result"`      // state.json's lastResult, null when none
	Recovered          string   `json:"recovered"`        // what recovery did before this command, "" when nothing
	Updated            bool     `json:"updated"`          // install swapped a new version in
	Message            string   `json:"message"`
	Notices            []string `json:"notices"` // e.g. MirrorChyan failed and GitHub was used
}

// notUpdatable is why this root cannot be updated ("" when it can).
func (u *Updater) notUpdatable() string {
	if _, err := u.Root.ReadMetadata(); err != nil {
		return errs.As(err).Message + "，先修好或删掉它再更新"
	}
	if u.Root.LocalSDKRoot() != "" {
		return "这个根目录在 mas-avd.json 里设了 sdkRoot（开发用的本地 SDK），不更新"
	}
	if !isFile(filepath.Join(u.l.emulator(), "emulator.exe")) {
		return `这个根目录里没有 sdk\emulator，请先放好魔改 AVD`
	}
	return ""
}

// status fills what needs no network.
func (u *Updater) status() *Status {
	set := LoadSettings()
	st := &Status{Root: u.Root.Dir, Source: set.Source, CDKSet: set.HasCDK(), Notices: []string{}}
	st.NotUpdatableReason = u.notUpdatable()
	st.Updatable = st.NotUpdatableReason == ""
	if v, ok := LocalVersion(u.Root); ok {
		st.CurrentVersion = v.String()
	}
	if n, ok := u.Root.ModBuild(); ok {
		st.CurrentBuild = "mas-" + strconv.Itoa(n)
	}
	state := u.l.readState()
	st.Phase = phaseOf(state)
	if v, ok := u.l.stagedVersion(); ok {
		st.PreparedVersion = v.String()
	}
	if lr := state.Get("lastResult"); lr.Kind() == jsonord.Object {
		st.LastResult = lr.ToGo()
	}
	return st
}

// Status is the update state without asking any source.
func (u *Updater) Status() *Status { return u.status() }

// Check asks the chosen source for the latest version (MirrorChyan without the CDK, which costs nothing; GitHub when
// that fails or is the choice).
func (u *Updater) Check(ctx context.Context) (*Status, error) {
	st := u.status()
	r, err := u.latest(ctx, st)
	if err != nil {
		st.CheckError = errs.As(err).Message
		return st, err
	}
	u.fillLatest(st, r)
	return st, nil
}

func (u *Updater) fillLatest(st *Status, r *release) {
	st.Checked, st.LatestVersion, st.LatestSource, st.RequiredAutoMAS, st.ReleaseURL =
		true, r.version.String(), r.source, r.minAutoMAS, r.pageURL
	local, ok := LocalVersion(u.Root)
	st.UpdateAvailable = !ok || local.Less(r.version)
	switch {
	case st.UpdateAvailable:
		st.Message = "有新版本 " + r.version.Display()
	case ok:
		st.Message = "已经是最新版本（" + local.Display() + "）"
	}
}

// latest is the latest release for a check.
func (u *Updater) latest(ctx context.Context, st *Status) (*release, error) {
	u.step("checking", "查最新版本")
	var mirrorErr error
	if st.Source == SourceMirrorChyan {
		var base *Version
		if v, ok := LocalVersion(u.Root); ok {
			base = &v
		}
		r, err := u.mirrorLatest(ctx, "", base)
		if err == nil {
			return r, nil
		}
		mirrorErr = err
	}
	r, err := u.githubLatest(ctx)
	if err != nil && mirrorErr != nil {
		return nil, sourceError("mirrorchyan: %v；github: %v", mirrorErr, errs.As(err).Message)
	}
	return r, err
}

// lock is .update\modavd.lock: one modavd update step per root at a time.
func (u *Updater) lock() (*win.FileLock, error) {
	if err := os.MkdirAll(u.l.dir(), 0o755); err != nil {
		return nil, errs.Wrap(errs.Internal, err, "建不了 "+u.l.dir())
	}
	l, ok, err := win.TryLock(u.l.lock())
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "更新锁文件打不开: "+u.l.lock())
	}
	if !ok {
		return nil, errs.New(errs.Busy, "这个根目录正在更新").WithAdvice("等它做完")
	}
	return l, nil
}

// begin takes the lock and recovers; every step that writes starts here.
func (u *Updater) begin(st *Status) (*win.FileLock, error) {
	lock, err := u.lock()
	if err != nil {
		return nil, err
	}
	msg, err := u.recover()
	if err != nil {
		lock.Unlock()
		return nil, errs.Wrap(errs.Internal, err, "收拾上次没做完的更新失败: "+err.Error())
	}
	if msg != "" {
		u.Log.Infof("%s", msg)
	}
	st.Recovered = msg
	return lock, nil
}

// Recover finishes or undoes a swap that was cut off and drops a preparation that was.
func (u *Updater) Recover(ctx context.Context) (*Status, error) {
	st := u.status()
	lock, err := u.begin(st)
	if err != nil {
		return st, err
	}
	lock.Unlock()
	rec := st.Recovered
	st = u.status()
	st.Recovered = rec
	st.Message = rec
	if st.Message == "" {
		st.Message = "没有要收拾的"
	}
	return st, nil
}

// Prepare downloads, verifies and stages the latest version (instances may keep running).
func (u *Updater) Prepare(ctx context.Context) (*Status, error) {
	st := u.status()
	if !st.Updatable {
		return st, errs.New(errs.NotReady, st.NotUpdatableReason)
	}
	lock, err := u.begin(st)
	if err != nil {
		return st, err
	}
	defer lock.Unlock()
	err = u.prepare(ctx, st)
	rec, notices := st.Recovered, st.Notices
	*st = *u.status()
	st.Recovered, st.Notices = rec, notices
	if err != nil {
		return st, err
	}
	if st.PreparedVersion != "" {
		st.Message = displayVersion(st.PreparedVersion) + " 已下载并核对好，可以安装"
	}
	return st, nil
}

// prepare stages the latest version unless it is not newer or already staged; st gets the check.
func (u *Updater) prepare(ctx context.Context, st *Status) error {
	if phaseOf(u.l.readState()) == phaseSwapping {
		return errs.New(errs.Busy, "上一次换上还没收尾，请稍后再试")
	}
	r, err := u.pick(ctx, st)
	if err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	if v, ok := u.l.stagedVersion(); ok && v == r.version {
		return nil
	}
	if err := u.checkSpace(); err != nil {
		return err
	}
	err = u.stage(ctx, r)
	if err != nil && r.source == SourceMirrorChyan && errs.CodeOf(err) == errs.UpdateRejected {
		note := "Mirror 酱的包核对不过，改从 GitHub 下载"
		if r.incremental {
			note = "Mirror 酱的增量包核对不过，改从 GitHub 下载"
		}
		u.Log.Warnf("%s: %v", note, err)
		st.Notices = append(st.Notices, note)
		gh, gerr := u.githubNewer(ctx)
		if gerr != nil {
			return gerr
		}
		err = u.stage(ctx, gh)
	}
	return err
}

// pick is AUTO-MAS _pick_package: the release to download, nil when nothing is newer. MirrorChyan with the CDK when
// that is the choice (an incremental package when the root's manifest allows), GitHub otherwise or as the fallback.
func (u *Updater) pick(ctx context.Context, st *Status) (*release, error) {
	set := LoadSettings()
	local, haveLocal := LocalVersion(u.Root)
	newer := func(r *release) bool { return !haveLocal || local.Less(r.version) }
	if set.Source == SourceMirrorChyan && set.HasCDK() {
		var base *Version
		if haveLocal && patchBaseOK(u.Root, local) {
			base = &local
		}
		r, err := u.mirrorLatest(ctx, set.CDK(), base)
		var me *mirrorError
		switch {
		case err == nil && r.url != "" && newer(r):
			u.fillLatest(st, r)
			if err := u.autoMASCheck(r); err != nil {
				return nil, err
			}
			return r, nil
		case err == nil && !newer(r):
			u.fillLatest(st, r)
			return nil, nil
		case errors.As(err, &me) && me.Code != 8001:
			note := me.Msg
			if !strings.Contains(note, "Mirror 酱") {
				note = "Mirror 酱：" + note
			}
			st.Notices = append(st.Notices, note+"，改从 GitHub 下载")
			u.Log.Warnf("%s，改从 GitHub 下载", note)
		}
	}
	r, err := u.githubLatest(ctx)
	if err != nil {
		hint := "（更新源选的是 GitHub）"
		if set.Source == SourceMirrorChyan {
			hint = "（没有填 Mirror 酱 CDK 时只能从 GitHub 下载）"
			if set.HasCDK() {
				hint = ""
			}
		}
		st.CheckError = errs.As(err).Message
		return nil, errs.Wrap(errs.As(err).Code, err, errs.As(err).Message+hint)
	}
	u.fillLatest(st, r)
	if !newer(r) {
		return nil, nil
	}
	if err := u.autoMASCheck(r); err != nil {
		return nil, err
	}
	return r, nil
}

// githubNewer is GitHub's latest release, which must be newer than the root's.
func (u *Updater) githubNewer(ctx context.Context) (*release, error) {
	r, err := u.githubLatest(ctx)
	if err != nil {
		return nil, err
	}
	if local, ok := LocalVersion(u.Root); ok && !local.Less(r.version) {
		return nil, sourceError("GitHub 上的最新版本是 %s，不比装着的 %s 新", r.version.Display(), local.Display())
	}
	return r, u.autoMASCheck(r)
}

func (u *Updater) autoMASCheck(r *release) error {
	if u.Opt.AutoMASVersion == "" || r.minAutoMAS == "" {
		return nil
	}
	if versionLess(u.Opt.AutoMASVersion, r.minAutoMAS) {
		return errs.Newf(errs.NotReady, "%s 需要 AUTO-MAS %s 或更新的版本，请先更新 AUTO-MAS", r.version.Display(), r.minAutoMAS)
	}
	return nil
}

var numRe = regexp.MustCompile(`[0-9]+`)

// versionLess compares the numbers of two dotted versions (5.8.0 < 5.10.0); pre-release tags are ignored.
func versionLess(a, b string) bool {
	x, y := numRe.FindAllString(a, 4), numRe.FindAllString(b, 4)
	for k := 0; k < max(len(x), len(y)); k++ {
		var p, q int
		if k < len(x) {
			p, _ = strconv.Atoi(x[k])
		}
		if k < len(y) {
			q, _ = strconv.Atoi(y[k])
		}
		if p != q {
			return p < q
		}
	}
	return false
}

func (u *Updater) checkSpace() error {
	free, err := win.DiskFree(u.Root.Dir)
	if err != nil || free >= minFreeBytes {
		return nil
	}
	return errs.Newf(errs.PrecheckFailed, "%s 剩余空间 %.1f GB，更新要临时占用约 1.7 GB，请先清理",
		filepath.VolumeName(u.Root.Dir), float64(free)/(1<<30))
}

// stage is AUTO-MAS _stage_release: preparing → download → unpack → verify → stage → staged; any failure goes back
// to idle with the reason (and drops a download that turned out bad).
func (u *Updater) stage(ctx context.Context, r *release) (err error) {
	if r.assetName == "" || r.url == "" {
		return sourceError("%s 没有能下载的包", r.version.Display())
	}
	target := r.target()
	if err := u.l.beginStaging(target); err != nil {
		return errs.Wrap(errs.Internal, err, "准备暂存失败: "+err.Error())
	}
	archive := ""
	defer func() {
		if err == nil {
			return
		}
		if archive != "" && errs.CodeOf(err) == errs.UpdateRejected {
			os.Remove(archive)
		}
		reason := errs.As(err).Message
		if errs.CodeOf(err) == errs.Canceled {
			reason = "被取消了，没做完"
		}
		u.l.abortStaging(reason)
	}()
	if archive, err = u.download(ctx, r); err != nil {
		return err
	}
	var files int
	if r.incremental {
		u.step("extracting", "正在按增量包拼新版")
		files, err = u.l.buildPatchedPackage(u.Root, archive, r.base)
	} else {
		u.step("extracting", "正在解包")
		files, err = u.l.extractPackage(archive, r.topDir, nil)
	}
	if err != nil {
		return err
	}
	u.step("verifying", "正在按清单核对每个文件")
	verified, err := u.l.verifyPackage(r.version, r.tag)
	if err != nil {
		return err
	}
	if err := u.l.buildStage(r.version); err != nil {
		return err
	}
	mode := "full"
	if r.incremental {
		mode = "incremental"
	}
	if err := u.l.finishStaging(target, verified, files, mode); err != nil {
		return errs.Wrap(errs.Internal, err, "记录暂存失败: "+err.Error())
	}
	return nil
}

// Install brings the root to the latest version: prepares it when it is newer and not staged yet, then swaps it in
// (every instance off, no other program of the root running). Nothing newer and nothing staged: up to date.
func (u *Updater) Install(ctx context.Context) (*Status, error) {
	st := u.status()
	if !st.Updatable {
		return st, errs.New(errs.NotReady, st.NotUpdatableReason)
	}
	lock, err := u.begin(st)
	if err != nil {
		return st, err
	}
	defer lock.Unlock()
	perr := u.prepare(ctx, st)
	staged, haveStaged := u.l.stagedVersion()
	if perr != nil && !haveStaged {
		return u.finish(st), perr
	}
	if perr != nil {
		// the check failed but an earlier preparation is there: install that one
		u.Log.Warnf("查新版本没成（%s），装已经准备好的 %s", errs.As(perr).Message, staged.Display())
	}
	if !haveStaged {
		out := u.finish(st)
		if out.Message == "" {
			out.Message = "已经是最新版本"
		}
		return out, nil
	}
	if local, ok := LocalVersion(u.Root); ok && !local.Less(staged) {
		u.l.abortStaging("准备好的版本不比装着的新")
		out := u.finish(st)
		out.Message = "已经是最新版本（" + local.Display() + "）"
		return out, nil
	}
	u.step("installing", "正在换上新版")
	version, err := u.swapIn()
	out := u.finish(st)
	if err != nil {
		if errs.CodeOf(err) == errs.Busy {
			return out, errs.Newf(errs.Busy, "%s 已下载并核对好，但现在不能换上：%s。处理好后再装就直接换上，不用重新下载",
				staged.Display(), errs.As(err).Message)
		}
		return out, err
	}
	out.Updated = true
	out.Message = "已更新到 " + displayVersion(version)
	if len(st.Notices) > 0 {
		out.Message += "（" + strings.Join(st.Notices, "；") + "）"
	}
	u.Log.Infof("%s", out.Message)
	return out, nil
}

// finish is the status after a step, keeping what the step learned (check, notices, recovery).
func (u *Updater) finish(st *Status) *Status {
	out := u.status()
	out.Checked, out.CheckError, out.LatestVersion, out.LatestSource = st.Checked, st.CheckError, st.LatestVersion, st.LatestSource
	out.RequiredAutoMAS, out.ReleaseURL, out.Recovered, out.Notices = st.RequiredAutoMAS, st.ReleaseURL, st.Recovered, st.Notices
	if local, ok := LocalVersion(u.Root); ok && st.LatestVersion != "" {
		if v, ok := ParseVersion(st.LatestVersion); ok {
			out.UpdateAvailable = local.Less(v)
		}
	}
	out.Message = st.Message
	return out
}
