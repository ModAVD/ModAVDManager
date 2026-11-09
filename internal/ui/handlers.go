package ui

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/avd"
	"modavd/internal/buildinfo"
	"modavd/internal/components"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// Mutations run on a context the browser cannot cancel: a closed tab must not leave half-written instance files.
func detached(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

func (s *server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Version                   string       `json:"version"`
		Protocol                  int          `json:"protocol"`
		Root                      emu.RootInfo `json:"root"`
		PlayStoreInstallSupported bool         `json:"play_store_install_supported"`
		PlayStoreRequiresBridge   bool         `json:"play_store_requires_bridge"`
	}{buildinfo.Version, buildinfo.Protocol, s.mgr.RootInfo(), components.PlayStoreInstallSupported(), components.PlayStoreRequiresBridge()})
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
		Resolution      *int    `json:"resolution"`
	}
	if !decode(w, r, &body) {
		return
	}
	req := emu.CreateRequest{Index: body.Index, MemoryMB: body.MemoryMB, CPU: body.CPU,
		DataPartitionGB: body.DataPartitionGB, FPS: body.FPS, Resolution: body.Resolution}
	if body.Title != nil {
		req.Title = *body.Title
	}
	// The job is the instance's from the start (its card shows the progress): pick the index now when none is given.
	if req.Index == nil {
		i, err := s.mgr.FreeIndex()
		if err != nil {
			writeError(w, err)
			return
		}
		req.Index = &i
	}
	s.startCreateJob(w, *req.Index, req)
}

// handleResumeCreate runs the setup of an instance whose create failed again (emu create --resume).
func (s *server) handleResumeCreate(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok || !decode(w, r, &struct{}{}) {
		return
	}
	s.startCreateJob(w, i, emu.CreateRequest{Index: &i, Resume: true})
}

// startCreateJob: create sets the instance up with a headless boot of a few minutes (emu createinit.go), so it runs
// as a job with progress; a closed window waits for it.
func (s *server) startCreateJob(w http.ResponseWriter, i int, req emu.CreateRequest) {
	v, err := s.startJob(kindCreate, i, func(ctx context.Context, j *job, b backend) (any, error) {
		req.Progress = s.progressFn(j)
		s.hub.requestRefresh() // the new instance shows up as creating at once
		res, err := b.Create(ctx, req)
		s.hub.dropCached()
		s.hub.requestRefresh()
		return res, err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

func (s *server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Title           *string `json:"title"`
		Diagnostics     *bool   `json:"diagnostics"`
		MemoryMB        *int    `json:"memory_mb"`
		CPU             *int    `json:"cpu"`
		FPS             *int    `json:"fps"`
		Resolution      *int    `json:"resolution"`
		DataPartitionGB *int    `json:"data_partition_gb"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Title == nil && body.MemoryMB == nil && body.CPU == nil && body.FPS == nil && body.Resolution == nil && body.DataPartitionGB == nil && body.Diagnostics == nil {
		writeError(w, errs.New(errs.Usage, "没有要改的设置").WithAdvice("至少给出 title、memory_mb、cpu、fps、resolution、data_partition_gb 中的一项"))
		return
	}
	res, err := s.mgr.Update(detached(r), i, emu.UpdateRequest{Title: body.Title, MemoryMB: body.MemoryMB, CPU: body.CPU,
		FPS: body.FPS, Resolution: body.Resolution, DataPartitionGB: body.DataPartitionGB, Diagnostics: body.Diagnostics})
	if err != nil {
		writeError(w, err)
		return
	}
	s.hub.requestRefresh()
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleDiagnosticsPreview(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	result, err := s.mgr.PreviewDiagnostics(r.Context(), i)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
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

// handleStart boots an instance with its window: the manager starts no headless ones (only the CLI does). One already
// running headless is not adopted (emu: no switch while it runs).
func (s *server) handleStart(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok || !decode(w, r, &struct{}{}) {
		return
	}
	s.respondJob(w)(s.startJob(kindStart, i, func(ctx context.Context, j *job, b backend) (any, error) {
		return b.Start(ctx, i, emu.StartOptions{Window: true, Progress: s.progressFn(j)})
	}))
}

func (s *server) handleStop(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Force        bool `json:"force"`
		CancelCreate bool `json:"cancel_create"`
	}
	if !decode(w, r, &body) {
		return
	}
	s.respondJob(w)(s.startJob(kindStop, i, func(ctx context.Context, j *job, b backend) (any, error) {
		s.stopStarts(ctx, j, i)
		s.setProgress(j, jobProgress{Step: "stop", Message: fmt.Sprintf("实例 %d 正在关机", i), Total: progressTotal})
		res, err := b.Stop(ctx, i, emu.StopOptions{Force: body.Force, CancelCreate: body.CancelCreate})
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

func (s *server) handleOpen(w http.ResponseWriter, r *http.Request) {
	platform := root.CurrentPlatform()
	if !platform.Windows() && platform.MirrorOS != "macos" {
		writeError(w, errs.New(errs.Unsupported, "当前平台暂不支持打开目录"))
		return
	}
	var body struct {
		What  string `json:"what"`
		Index *int   `json:"index"`
		Job   string `json:"job"`
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
	case "collect-info":
		// only the folder of a problem package this manager made: the page names the job, never a path
		zip, err := s.collectZip(body.Job)
		if err != nil {
			writeError(w, err)
			return
		}
		dir = filepath.Dir(zip)
	default:
		writeError(w, errs.Newf(errs.InvalidArgument, "what 只能是 root、logs、instance 或 collect-info（给的是 %q）", body.What))
		return
	}
	if err := s.openPath(dir); err != nil {
		message := "在资源管理器里打开 "
		if platform.MirrorOS == "macos" {
			message = "在 Finder 里打开 "
		}
		writeError(w, errs.Wrap(errs.Internal, err, message+dir+" 失败: "+err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// collectZip is the problem package of a finished 收集问题信息 job (errs.NotFound when the job is not one, or the
// package is gone).
func (s *server) collectZip(id string) (string, error) {
	if id == "" {
		return "", errs.New(errs.Usage, "打开问题包所在文件夹要给 job（收集问题信息的任务 id）")
	}
	j, err := s.jobByID(id)
	if err != nil {
		return "", err
	}
	v := s.jobView(j)
	res, ok := v.Result.(*DiagCollectResult)
	if v.Kind != kindCollect || v.State != stateDone || !ok || res == nil || res.ZipPath == "" {
		return "", errs.Newf(errs.NotFound, "任务 %s 不是做完了的收集问题信息", id)
	}
	if st, err := os.Stat(res.ZipPath); err != nil || st.IsDir() {
		return "", errs.Newf(errs.NotFound, "问题包已经不在了: %s", res.ZipPath)
	}
	return res.ZipPath, nil
}

func (s *server) handleComponents(w http.ResponseWriter, r *http.Request) {
	res, err := s.diag.Components(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) handleComponentLicense(w http.ResponseWriter, r *http.Request) {
	source := components.LoadSourceSettings()
	if q := r.URL.Query(); q.Get("source") != "" || q.Get("mirror") != "" || len(q["mirror_urls"]) > 0 {
		urls := append([]string(nil), q["mirror_urls"]...)
		var err error
		source, err = componentSourceFromFields(q.Get("source"), q.Get("mirror"), urls)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	res, err := s.diag.License(r.Context(), source)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

type componentSourceView struct {
	Source     string   `json:"source"`
	Mirror     string   `json:"mirror"`
	MirrorURLs []string `json:"mirror_urls"`
	Configured bool     `json:"configured"`
}

func componentSourceViewOf(s components.SourceSettings) componentSourceView {
	source := "auto"
	if len(s.MirrorURLs) > 0 {
		source = "custom"
	} else if s.Mirror != "" {
		source = s.Mirror
	}
	urls := append([]string{}, s.MirrorURLs...)
	return componentSourceView{Source: source, Mirror: s.Mirror, MirrorURLs: urls, Configured: s.Set}
}

func componentSourceFromFields(source, mirror string, urls []string) (components.SourceSettings, error) {
	source = strings.ToLower(strings.TrimSpace(source))
	mirror = strings.TrimSpace(mirror)
	switch source {
	case "auto":
		mirror, urls = "", nil
	case "googledownloads_cn", "tencent", "google":
		mirror, urls = source, nil
	case "custom":
		if len(urls) == 0 {
			return components.SourceSettings{}, errs.New(errs.Usage, "自定义组件下载源至少要有一个地址")
		}
		mirror = ""
	case "":
		if mirror == "" && len(urls) == 0 {
			return components.SourceSettings{}, errs.New(errs.Usage, "组件下载源必须给 source 或 mirror_urls")
		}
	default:
		return components.SourceSettings{}, errs.Newf(errs.InvalidArgument, "组件下载源只能是 auto、googledownloads_cn、tencent、google 或 custom，收到「%s」", source)
	}
	return components.SourceSettings{Mirror: mirror, MirrorURLs: urls, Set: true}, nil
}

func (s *server) handleComponentSource(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, componentSourceViewOf(components.LoadSourceSettings()))
		return
	}
	var body struct {
		Source     string   `json:"source"`
		Mirror     string   `json:"mirror"`
		MirrorURLs []string `json:"mirror_urls"`
	}
	if !decode(w, r, &body) {
		return
	}
	source, err := componentSourceFromFields(body.Source, body.Mirror, body.MirrorURLs)
	if err != nil {
		writeError(w, err)
		return
	}
	saved, err := components.SaveSourceSettings(source.Mirror, source.MirrorURLs)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, componentSourceViewOf(saved))
}

func (s *server) handleComponentsFetch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AcceptLicense *bool    `json:"accept_license"`
		Source        string   `json:"source"`
		Mirror        string   `json:"mirror"`
		MirrorURLs    []string `json:"mirror_urls"`
		LicenseSHA256 string   `json:"license_sha256"`
		PlayStore     bool     `json:"play_store"` // also the optional Google Play Store
	}
	if !decode(w, r, &body) {
		return
	}
	if body.AcceptLicense == nil || !*body.AcceptLicense {
		writeError(w, errs.New(errs.Usage, "补全组件必须明确同意许可协议").WithAdvice("先阅读许可协议，再发送 accept_license: true"))
		return
	}
	// the consent is to the text the dialog showed: GET /api/components/license gave its SHA-256, and the fetch stops
	// when the mirror now serves another one
	if !isSHA256Hex(body.LicenseSHA256) {
		writeError(w, errs.New(errs.Usage, "补全组件要带上看过的许可协议的 license_sha256").
			WithAdvice("先用 GET /api/components/license 取许可协议（和这次一样的下载源），把它的 license_sha256 原样带上"))
		return
	}
	source := components.LoadSourceSettings()
	if body.Source != "" || body.Mirror != "" || len(body.MirrorURLs) > 0 {
		var err error
		source, err = componentSourceFromFields(body.Source, body.Mirror, body.MirrorURLs)
		if err != nil {
			writeError(w, err)
			return
		}
	}
	v, err := s.startRootJob(kindComponents, func(ctx context.Context, j *job, _ backend) (any, error) {
		return s.jobDiag(j).Fetch(ctx, true, body.PlayStore, source, body.LicenseSHA256, func(p DiagProgress) { s.setDiagProgress(j, p) })
	})
	s.respondJob(w)(v, err)
}

// jobDiag is the diagnostic backend of one job: its messages are the job's (file log tagged with the job id, the
// job's lines in the event stream, warnings), like the instance backend startJob gives the other jobs.
func (s *server) jobDiag(j *job) diagBackend {
	return s.newDiag(logx.New(s.jobLogSink(j)))
}

func isSHA256Hex(v string) bool {
	if len(v) != 64 {
		return false
	}
	_, err := hex.DecodeString(v)
	return err == nil
}

func (s *server) handleDiagCollect(w http.ResponseWriter, r *http.Request) {
	var body DiagCollectOptions
	if !decode(w, r, &body) {
		return
	}
	v, err := s.startRootJob(kindCollect, func(ctx context.Context, j *job, _ backend) (any, error) {
		return s.jobDiag(j).Collect(ctx, body, func(p DiagProgress) { s.setDiagProgress(j, p) })
	})
	s.respondJob(w)(v, err)
}

func (s *server) handleReportConfig(w http.ResponseWriter, r *http.Request) {
	text, err := s.diag.ReportConfig(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Text string `json:"text"`
	}{text})
}

func (s *server) handleCheck(w http.ResponseWriter, r *http.Request) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, errs.New(errs.InvalidArgument, "检查参数不合法"))
		return
	}
	refresh := query.Get("refresh") == "1"
	var index *int
	memoryMB := avd.DefaultMemoryMB
	if values, provided := query["index"]; provided {
		if len(values) != 1 {
			writeError(w, errs.New(errs.InvalidArgument, "只能指定一个检查实例"))
			return
		}
		i, err := parseIndex(values[0])
		if err != nil {
			writeError(w, err)
			return
		}
		info, err := s.mgr.Info(r.Context(), i)
		if err != nil {
			writeError(w, err)
			return
		}
		index = &i
		if info.Config.MemoryMB > 0 {
			memoryMB = info.Config.MemoryMB
		}
	}
	writeResult := func(res *emu.CheckResult) {
		writeJSON(w, http.StatusOK, struct {
			*emu.CheckResult
			InstanceIndex *int `json:"instance_index"`
			MemoryMB      int  `json:"memory_mb"`
			RootReady     bool `json:"root_ready"`
		}{res, index, memoryMB, res.RootReady()})
	}
	asked := time.Now()
	c := &s.check
	if err := c.lock(r.Context()); err != nil {
		writeError(w, err)
		return
	}
	defer c.unlock()
	// A refresh may share a refresh that finished while it waited, but a normal
	// check may have reused the hardware probes' caches.
	c.mu.Lock()
	cached, cachedAt, cachedMemoryMB := c.res, c.at, c.memoryMB
	cachedRefreshed := c.refreshed
	generation := c.generation
	c.mu.Unlock()
	if cached != nil && cachedMemoryMB == memoryMB && (!refresh || (cachedRefreshed && cachedAt.After(asked))) {
		writeResult(cached)
		return
	}
	res, err := s.mgr.Check(r.Context(), emu.CheckOptions{MemoryMB: memoryMB, Refresh: refresh})
	if err != nil {
		writeError(w, err)
		return
	}
	c.mu.Lock()
	if c.generation == generation {
		c.res, c.at, c.memoryMB, c.refreshed = res, time.Now(), memoryMB, refresh
	}
	c.mu.Unlock()
	writeResult(res)
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
