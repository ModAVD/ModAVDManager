package ui

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

const (
	kindStart      = "start"
	kindStop       = "stop"
	kindInstall    = "install"
	kindCreate     = "create"
	kindComponents = "components"
	kindCollect    = "collect-info"
	rootJobIndex   = -1

	kindDiskInfo    = "disk-info"
	kindDiskTrim    = "disk-trim"
	kindDiskCompact = "disk-compact"

	// the Google Play Store (playstore.go): an instance's install and uninstall, the root's import of its files
	kindPlayStoreInstall   = "playstore-install"
	kindPlayStoreUninstall = "playstore-uninstall"
	kindPlayStoreImport    = "playstore-import"

	// the reported GLES 3.2 (gles32.go): an instance's switch on and off
	kindGLES32On  = "gles32-on"
	kindGLES32Off = "gles32-off"

	stateRunning  = "running"
	stateDone     = "done"
	stateFailed   = "failed"
	stateCanceled = "canceled"

	maxJobs = 50
	// progressTotal matches emu's start progress scale.
	progressTotal = 1000
)

func kindLabel(kind string) string {
	switch kind {
	case kindStart:
		return "开机"
	case kindStop:
		return "关机"
	case kindInstall:
		return "安装 APK"
	case kindCreate:
		return "新建"
	case kindComponents:
		return "补全组件"
	case kindCollect:
		return "收集问题信息"
	case kindDiskInfo:
		return "查看磁盘占用"
	case kindDiskTrim:
		return "回收磁盘空间"
	case kindDiskCompact:
		return "整理磁盘"
	case kindPlayStoreInstall:
		return "补全 Google Play 商店"
	case kindPlayStoreUninstall:
		return "移除 Google Play 商店"
	case kindPlayStoreImport:
		return "导入 Google Play 商店的文件"
	case kindGLES32On:
		return "开启报告 GLES 3.2"
	case kindGLES32Off:
		return "关闭报告 GLES 3.2"
	}
	return kind
}

// sdkJob: a root job that writes the root's sdk\ (components fetch, the store's import). bootJob: an instance job that
// boots the instance, so reads sdk\. The two never run at the same time.
func sdkJob(kind string) bool { return kind == kindComponents || kind == kindPlayStoreImport }

func bootJob(kind string) bool {
	switch kind {
	case kindStart, kindCreate, "restart", kindPlayStoreInstall, kindPlayStoreUninstall, kindGLES32On, kindGLES32Off:
		return true
	}
	return false
}

type jobProgress struct {
	Step      string  `json:"step"`
	Name      string  `json:"name,omitempty"`
	Message   string  `json:"message"`
	Done      int     `json:"done"`
	Total     int     `json:"total"`
	Speed     float64 `json:"speed,omitempty"`
	Remaining float64 `json:"remaining,omitempty"`
}

// jobView is the Job of API.md.
type jobView struct {
	ID         string       `json:"id"`
	Kind       string       `json:"kind"`
	Index      int          `json:"index"`
	State      string       `json:"state"`
	CreatedAt  string       `json:"created_at"`
	FinishedAt string       `json:"finished_at"`
	Progress   *jobProgress `json:"progress"`
	Result     any          `json:"result"`
	Error      *errorBody   `json:"error"`
	Warnings   []string     `json:"warnings"`
}

// job: view is guarded by jobTable.mu; cancel and done never change.
type job struct {
	view          jobView
	cancel        context.CancelFunc
	done          chan struct{} // closed when the job function returned
	progressAt    time.Time
	progressStage string
	progressName  string
}

type jobTable struct {
	mu   sync.Mutex
	seq  int
	list []*job // oldest first
}

// snapshot copies the view (caller holds mu).
func (j *job) snapshot() jobView {
	v := j.view
	v.Warnings = slices.Clone(j.view.Warnings)
	if v.Progress != nil {
		p := *v.Progress
		v.Progress = &p
	}
	return v
}

func (t *jobTable) running() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, j := range t.list {
		if j.view.State == stateRunning {
			n++
		}
	}
	return n
}

// waitIdle waits up to d for every running job to return.
func (t *jobTable) waitIdle(d time.Duration) {
	t.mu.Lock()
	var pending []*job
	for _, j := range t.list {
		if j.view.State == stateRunning {
			pending = append(pending, j)
		}
	}
	t.mu.Unlock()
	deadline := time.After(d)
	for _, j := range pending {
		select {
		case <-j.done:
		case <-deadline:
			return
		}
	}
}

// busyWith is the running job of instance i that keeps a new job of kind from starting, nil when it may start. The
// one exception: a stop may start while a start runs (emu stops an instance that is booting, and so do we).
func (t *jobTable) busyWith(kind string, i int) *job {
	for _, o := range t.list {
		if o.view.State != stateRunning || o.view.Index != i {
			continue
		}
		if kind == kindStop && o.view.Kind == kindStart {
			continue
		}
		return o
	}
	return nil
}

func busyError(o *job) error {
	return errs.Newf(errs.Busy, "实例 %d 正在%s（任务 %s），等它做完再试", o.view.Index, kindLabel(o.view.Kind), o.view.ID)
}

// checkFree reports errs.Busy when a job of kind could not start on instance i now.
func (s *server) checkFree(kind string, i int) error {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	if o := s.jobs.busyWith(kind, i); o != nil {
		return busyError(o)
	}
	return nil
}

// instanceBusy is the running job of instance i, if any.
func (s *server) instanceBusy(i int) error {
	return s.checkFree("", i)
}

// prune keeps the newest maxJobs; running jobs are never dropped.
func (t *jobTable) prune() {
	for len(t.list) > maxJobs {
		k := slices.IndexFunc(t.list, func(j *job) bool { return j.view.State != stateRunning })
		if k < 0 {
			return
		}
		t.list = slices.Delete(t.list, k, k+1)
	}
}

type jobFunc func(ctx context.Context, j *job, b backend) (any, error)

// startJob registers a job of kind on instance i and runs fn in its own goroutine with a backend whose log feeds the
// job (file log with the job id, log events, warnings). errs.Busy when the instance has a conflicting job.
func (s *server) startJob(kind string, i int, fn jobFunc) (jobView, error) {
	s.jobs.mu.Lock()
	for _, o := range s.jobs.list {
		if o.view.State != stateRunning {
			continue
		}
		if (sdkJob(kind) && o.view.Index != rootJobIndex && bootJob(o.view.Kind)) ||
			(i != rootJobIndex && sdkJob(o.view.Kind) && bootJob(kind)) {
			s.jobs.mu.Unlock()
			return jobView{}, errs.Newf(errs.Busy, "任务 %s 正在运行（任务 %s），等它做完再试", kindLabel(o.view.Kind), o.view.ID)
		}
	}
	if o := s.jobs.busyWith(kind, i); o != nil {
		s.jobs.mu.Unlock()
		return jobView{}, busyError(o)
	}
	s.jobs.seq++
	ctx, cancel := context.WithCancel(s.jobCtx)
	j := &job{
		view: jobView{
			ID:        "j" + strconv.Itoa(s.jobs.seq),
			Kind:      kind,
			Index:     i,
			State:     stateRunning,
			CreatedAt: time.Now().Format(time.RFC3339),
			Warnings:  []string{},
		},
		cancel: cancel,
		done:   make(chan struct{}),
	}
	s.jobs.list = append(s.jobs.list, j)
	s.jobs.prune()
	view := j.snapshot()
	s.jobs.mu.Unlock()

	b := s.newBackend(logx.New(s.jobLogSink(j)))
	s.log.Infof("[%s] 开始%s%s", view.ID, kindLabel(kind), strings.TrimSpace(jobTarget(i)))
	s.publishJob(view, true)
	go s.runJob(ctx, j, b, fn)
	return view, nil
}

// jobTarget is what a job works on, for the log lines: "实例 3 " (with the space the lines had), nothing for a root
// job (components, collect-info).
func jobTarget(i int) string {
	if i == rootJobIndex {
		return ""
	}
	return "实例 " + strconv.Itoa(i) + " "
}

func (s *server) startRootJob(kind string, fn jobFunc) (jobView, error) {
	return s.startJob(kind, rootJobIndex, fn)
}

func (s *server) runJob(ctx context.Context, j *job, b backend, fn jobFunc) {
	defer close(j.done)
	defer j.cancel()
	var res any
	var err error
	func() {
		defer func() {
			if p := recover(); p != nil {
				s.log.Errorf("[%s] 任务崩溃: %v\n%s", j.view.ID, p, debug.Stack())
				err = errs.Newf(errs.Internal, "任务出错了: %v", p)
			}
		}()
		res, err = fn(ctx, j, b)
	}()
	s.finishJob(ctx, j, res, err)
}

func (s *server) finishJob(ctx context.Context, j *job, res any, err error) {
	if err != nil && ctx.Err() != nil && errs.CodeOf(err) != errs.Canceled {
		// whatever broke after a cancel broke because of it (an adb killed half way ...)
		err = errs.Wrap(errs.Canceled, err, "已取消")
	}
	s.jobs.mu.Lock()
	v := &j.view
	v.FinishedAt = time.Now().Format(time.RFC3339)
	switch {
	case err == nil:
		v.State = stateDone
		v.Result = res
	case errs.CodeOf(err) == errs.Canceled:
		v.State = stateCanceled
	default:
		v.State = stateFailed
	}
	v.Error = newErrorBody(err)
	view := j.snapshot()
	s.jobs.mu.Unlock()
	if sdkJob(view.Kind) {
		s.check.mu.Lock()
		s.check.res, s.check.at = nil, time.Time{}
		s.check.generation++
		s.check.mu.Unlock()
	}

	switch view.State {
	case stateDone:
		s.log.Infof("[%s] %s%s完成", view.ID, kindLabel(view.Kind), jobTarget(view.Index))
	case stateCanceled:
		s.log.Infof("[%s] %s%s已取消", view.ID, kindLabel(view.Kind), jobTarget(view.Index))
	default:
		s.log.Warnf("[%s] %s%s失败: %s", view.ID, kindLabel(view.Kind), jobTarget(view.Index), errs.As(err).Error())
	}
	s.publishJob(view, true)
}

// publishJob sends the job event; refresh also asks for a fresh instance list (a job step changes instance state).
func (s *server) publishJob(v jobView, refresh bool) {
	s.hub.broadcast("job", v)
	if refresh {
		s.hub.requestRefresh()
	}
}

func (s *server) setProgress(j *job, p jobProgress) {
	s.jobs.mu.Lock()
	if j.view.State != stateRunning {
		s.jobs.mu.Unlock()
		return
	}
	j.view.Progress = &p
	view := j.snapshot()
	s.jobs.mu.Unlock()
	s.publishJob(view, true)
}

func (s *server) setDiagProgress(j *job, p DiagProgress) {
	s.jobs.mu.Lock()
	if j.view.State != stateRunning {
		s.jobs.mu.Unlock()
		return
	}
	now := time.Now()
	if !j.progressAt.IsZero() && now.Sub(j.progressAt) < 500*time.Millisecond && j.progressStage == p.Stage && j.progressName == p.Name {
		s.jobs.mu.Unlock()
		return
	}
	j.progressAt, j.progressStage, j.progressName = now, p.Stage, p.Name
	j.view.Progress = &jobProgress{Step: p.Stage, Name: p.Name, Message: p.Message, Done: int(p.Done), Total: int(p.Total), Speed: p.Speed, Remaining: p.Remaining}
	view := j.snapshot()
	s.jobs.mu.Unlock()
	s.publishJob(view, false)
}

// progressFn forwards emu's start progress into the job.
func (s *server) progressFn(j *job) func(emu.Progress) {
	return func(p emu.Progress) {
		s.setProgress(j, jobProgress{Step: p.Step, Message: p.Message, Done: p.Done, Total: p.Total})
	}
}

func (s *server) addWarning(j *job, msg string) {
	s.jobs.mu.Lock()
	j.view.Warnings = append(j.view.Warnings, msg)
	view := j.snapshot()
	s.jobs.mu.Unlock()
	s.publishJob(view, false)
}

// jobLogSink: the job's messages go to the file log (tagged with the job id) and the event streams; warnings and
// errors also into job.warnings. That is the same set emu collects into StartResult / StopResult.Warnings.
func (s *server) jobLogSink(j *job) logx.Sink {
	id := j.view.ID
	return func(level logx.Level, msg string) {
		s.log.Log(level, "["+id+"] "+msg)
		s.emitLog(level, msg, id)
		if level >= logx.Warn {
			s.addWarning(j, msg)
		}
	}
}

func (s *server) jobByID(id string) (*job, error) {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	for _, j := range s.jobs.list {
		if j.view.ID == id {
			return j, nil
		}
	}
	return nil, errs.Newf(errs.NotFound, "没有任务 %s（只保留最近 %d 个）", id, maxJobs)
}

func (s *server) jobView(j *job) jobView {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	return j.snapshot()
}

// listJobs is newest first.
func (s *server) listJobs() []jobView {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	out := make([]jobView, 0, len(s.jobs.list))
	for k := len(s.jobs.list) - 1; k >= 0; k-- {
		out = append(out, s.jobs.list[k].snapshot())
	}
	return out
}

// cancelJob cancels the job's context; the job turns "canceled" once its function returns.
func (s *server) cancelJob(id string) (jobView, error) {
	j, err := s.jobByID(id)
	if err != nil {
		return jobView{}, err
	}
	s.jobs.mu.Lock()
	running := j.view.State == stateRunning
	s.jobs.mu.Unlock()
	if running {
		s.log.Infof("[%s] 请求取消", id)
		j.cancel()
	}
	return s.jobView(j), nil
}

// stopStarts is the first step of a stop: a start job of the same instance is cancelled (which only stops waiting;
// the emulator keeps booting) and waited for, so the stop sees the qemu that start launched instead of racing it.
func (s *server) stopStarts(ctx context.Context, j *job, i int) {
	s.jobs.mu.Lock()
	var starts []*job
	for _, o := range s.jobs.list {
		if o != j && o.view.State == stateRunning && o.view.Index == i && o.view.Kind == kindStart {
			starts = append(starts, o)
		}
	}
	s.jobs.mu.Unlock()
	if len(starts) == 0 {
		return
	}
	s.setProgress(j, jobProgress{Step: "stop", Message: fmt.Sprintf("实例 %d 先停下开机任务", i), Total: progressTotal})
	for _, o := range starts {
		s.log.Infof("[%s] 关机前取消开机任务 %s", j.view.ID, o.view.ID)
		o.cancel()
	}
	deadline := time.After(s.stopWait)
	for _, o := range starts {
		select {
		case <-o.done:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}
