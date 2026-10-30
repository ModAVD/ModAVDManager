//go:build windows

package ui

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

const (
	kindStart   = "start"
	kindStop    = "stop"
	kindRestart = "restart"
	kindInstall = "install"

	stateRunning  = "running"
	stateDone     = "done"
	stateFailed   = "failed"
	stateCanceled = "canceled"

	maxJobs = 50
	// progressTotal matches emu's start progress scale, so a restart (stop, then start) stays on one scale.
	progressTotal = 1000
)

func kindLabel(kind string) string {
	switch kind {
	case kindStart:
		return "开机"
	case kindStop:
		return "关机"
	case kindRestart:
		return "重启"
	case kindInstall:
		return "安装 APK"
	}
	return kind
}

type jobProgress struct {
	Step    string `json:"step"`
	Message string `json:"message"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
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
	view   jobView
	cancel context.CancelFunc
	done   chan struct{} // closed when the job function returned
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
	s.log.Infof("[%s] 开始%s实例 %d", view.ID, kindLabel(kind), i)
	s.publishJob(view, true)
	go s.runJob(ctx, j, b, fn)
	return view, nil
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

	switch view.State {
	case stateDone:
		s.log.Infof("[%s] %s实例 %d 完成", view.ID, kindLabel(view.Kind), view.Index)
	case stateCanceled:
		s.log.Infof("[%s] %s实例 %d 已取消", view.ID, kindLabel(view.Kind), view.Index)
	default:
		s.log.Warnf("[%s] %s实例 %d 失败: %s", view.ID, kindLabel(view.Kind), view.Index, errs.As(err).Error())
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
