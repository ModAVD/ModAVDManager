//go:build windows

package ui

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/root"
)

// Mutations run on a context the browser cannot cancel: a closed tab must not leave half-written instance files.
func detached(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Version  string       `json:"version"`
		Protocol int          `json:"protocol"`
		Root     emu.RootInfo `json:"root"`
	}{buildinfo.Version, buildinfo.Protocol, s.mgr.RootInfo()})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	res, err := s.mgr.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	s.hub.storeList(res)
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	res, err := s.mgr.Info(r.Context(), i)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Title           *string `json:"title"`
		Index           *int    `json:"index"`
		MemoryMB        *int    `json:"memory_mb"`
		CPU             *int    `json:"cpu"`
		DataPartitionGB *int    `json:"data_partition_gb"`
		FPS             *int    `json:"fps"`
	}
	if !decode(w, r, &body) {
		return
	}
	req := emu.CreateRequest{Index: body.Index, MemoryMB: body.MemoryMB, CPU: body.CPU,
		DataPartitionGB: body.DataPartitionGB, FPS: body.FPS}
	if body.Title != nil {
		req.Title = *body.Title
	}
	res, err := s.mgr.Create(detached(r), req)
	if err != nil {
		writeError(w, err)
		return
	}
	s.hub.dropCached()
	s.hub.requestRefresh()
	writeJSON(w, http.StatusCreated, res)
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Title           *string `json:"title"`
		MemoryMB        *int    `json:"memory_mb"`
		CPU             *int    `json:"cpu"`
		FPS             *int    `json:"fps"`
		DataPartitionGB *int    `json:"data_partition_gb"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Title == nil && body.MemoryMB == nil && body.CPU == nil && body.FPS == nil && body.DataPartitionGB == nil {
		writeError(w, errs.New(errs.Usage, "没有要改的设置").WithAdvice("至少给出 title、memory_mb、cpu、fps、data_partition_gb 中的一项"))
		return
	}
	res, err := s.mgr.Update(detached(r), i, emu.UpdateRequest{Title: body.Title, MemoryMB: body.MemoryMB, CPU: body.CPU,
		FPS: body.FPS, DataPartitionGB: body.DataPartitionGB})
	if err != nil {
		writeError(w, err)
		return
	}
	s.hub.requestRefresh()
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("confirm") != "1" {
		writeError(w, errs.Newf(errs.Usage, "删除实例 %d 要带 confirm=1", i).
			WithAdvice("删除会连同游戏数据一起删掉，确认后再发 DELETE /api/instances/"+r.PathValue("i")+"?confirm=1"))
		return
	}
	// emu refuses a running instance by its locks; a job of ours that has not taken them yet is refused here.
	if err := s.instanceBusy(i); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.mgr.Delete(detached(r), i)
	if err != nil {
		writeError(w, err)
		return
	}
	s.shots.forget(i)
	s.hub.dropCached()
	s.hub.requestRefresh()
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleStart(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Window bool `json:"window"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.respondJob(w)(s.startJob(kindStart, i, func(ctx context.Context, j *job, b backend) (any, error) {
		return b.Start(ctx, i, emu.StartOptions{Window: body.Window, Progress: s.progressFn(j)})
	}))
}

func (s *server) handleStop(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.respondJob(w)(s.startJob(kindStop, i, func(ctx context.Context, j *job, b backend) (any, error) {
		s.stopStarts(ctx, j, i)
		s.setProgress(j, jobProgress{Step: "stop", Message: fmt.Sprintf("实例 %d 正在关机", i), Total: progressTotal})
		res, err := b.Stop(ctx, i, emu.StopOptions{Force: body.Force})
		if err != nil {
			return nil, err
		}
		s.setProgress(j, jobProgress{Step: "done", Message: stopMessage(res), Done: progressTotal, Total: progressTotal})
		return res, nil
	}))
}

func stopMessage(res *emu.StopResult) string {
	if res.Method == "already_offline" {
		return fmt.Sprintf("实例 %d 本来就没开机", res.Index)
	}
	return fmt.Sprintf("实例 %d 已关机（%.1f 秒）", res.Index, res.Seconds)
}

func (s *server) handleRestart(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Window bool `json:"window"`
		Force  bool `json:"force"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.respondJob(w)(s.startJob(kindRestart, i, func(ctx context.Context, j *job, b backend) (any, error) {
		s.setProgress(j, jobProgress{Step: "stop", Message: fmt.Sprintf("实例 %d 先关机", i), Total: progressTotal})
		if _, err := b.Stop(ctx, i, emu.StopOptions{Force: body.Force}); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, errs.Wrap(errs.Canceled, err, "已取消")
		}
		return b.Start(ctx, i, emu.StartOptions{Window: body.Window, Progress: s.progressFn(j)})
	}))
}

// respondJob answers a job request: 202 with the job, or the error.
func (s *server) respondJob(w http.ResponseWriter) func(jobView, error) {
	return func(v jobView, err error) {
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, v)
	}
}

func (s *server) handleWindow(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Visible *bool `json:"visible"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Visible == nil {
		writeError(w, errs.New(errs.Usage, "缺少 visible（true 显示窗口，false 隐藏）"))
		return
	}
	res, err := s.mgr.SetWindowVisible(detached(r), i, *body.Visible)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleOpen(w http.ResponseWriter, r *http.Request) {
	var body struct {
		What  string `json:"what"`
		Index *int   `json:"index"`
	}
	if !decode(w, r, &body) {
		return
	}
	var dir string
	switch body.What {
	case "root":
		dir = s.root.Dir
	case "logs":
		dir = s.root.LogsDir()
		// a root that never booted has no logs folder yet; an empty one is what there is to see
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeError(w, errs.Wrap(errs.Internal, err, "建不了日志文件夹 "+dir+": "+err.Error()))
			return
		}
	case "instance":
		if body.Index == nil {
			writeError(w, errs.New(errs.Usage, "打开实例文件夹要给 index"))
			return
		}
		if !root.ValidIndex(*body.Index) {
			writeError(w, errs.Newf(errs.InvalidArgument, "实例编号 %d 不可用（只能是 0–9，5 除外）", *body.Index))
			return
		}
		dir = filepath.Join(s.root.AvdHome(), root.AvdName(*body.Index)+".avd")
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			writeError(w, errs.Newf(errs.NotFound, "实例 %d 的文件夹不存在: %s", *body.Index, dir))
			return
		}
	default:
		writeError(w, errs.Newf(errs.InvalidArgument, "what 只能是 root、logs 或 instance（给的是 %q）", body.What))
		return
	}
	if err := s.openPath(dir); err != nil {
		writeError(w, errs.Wrap(errs.Internal, err, "在资源管理器里打开 "+dir+" 失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *server) handleCheck(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	asked := time.Now()
	c := &s.check
	if err := c.lock(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	defer c.unlock()
	// a check that finished while this request waited is as fresh as one started for it
	if c.res != nil && (!refresh || c.at.After(asked)) {
		writeJSON(w, http.StatusOK, c.res)
		return
	}
	res, err := s.mgr.Check(r.Context(), emu.CheckOptions{})
	if err != nil {
		writeError(w, err)
		return
	}
	c.res, c.at = res, time.Now()
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleJobs(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Jobs []jobView `json:"jobs"`
	}{s.listJobs()})
}

func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	j, err := s.jobByID(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.jobView(j))
}

func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	v, err := s.cancelJob(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
