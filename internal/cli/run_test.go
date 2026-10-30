//go:build windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/mcp"
	"modavd/internal/root"
	"modavd/internal/win"
)

// fakeManager records calls; each method uses its hook when set, else returns a plain result.
type fakeManager struct {
	mu    sync.Mutex
	calls []string
	log   *logx.Logger

	list    func() (*emu.ListResult, error)
	info    func(i int) (*emu.InstanceInfo, error)
	start   func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error)
	stop    func(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error)
	shell   func(i int, cmd string, timeout time.Duration) (*emu.ShellResult, error)
	adb     func(i int, args []string, stdout, stderr io.Writer) (int, error)
	install func(i int, apk string) (*emu.InstallResult, error)
	create  func(req emu.CreateRequest) (*emu.InstanceInfo, error)
	update  func(i int, req emu.UpdateRequest) (*emu.UpdateResult, error)
	running func(ctx context.Context) ([]int, error)
	wait    func(timeout time.Duration)

	waits []time.Duration // Wait calls (not in calls: most tests count those)
}

func (f *fakeManager) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeManager) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

func sampleInstance(i int) emu.InstanceInfo {
	return emu.InstanceInfo{
		Index: i, Name: root.AvdName(i), Title: root.AvdName(i), Status: emu.StatusOffline,
		Serial: root.Serial(i), AdbAddress: "127.0.0.1:" + itoa(root.AdbPort(i)),
		Ports:  emu.Ports{Console: root.ConsolePort(i), Adb: root.AdbPort(i), Grpc: root.GrpcPort(i)},
		Config: emu.InstanceConfig{MemoryMB: 6144, CPU: 6, FPS: 60, DataPartitionGB: 64, Width: 1280, Height: 720, DPI: 240},
		Path:   `D:\r\avd\` + root.AvdName(i) + ".avd",
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func (f *fakeManager) List(ctx context.Context) (*emu.ListResult, error) {
	f.record("list")
	f.log.Infof("扫描实例")
	f.log.Debugf("调试细节")
	if f.list != nil {
		return f.list()
	}
	return &emu.ListResult{RootInfo: emu.RootInfo{Root: `D:\r`, AdbServerPort: 20050, ScriptAdbServerPort: 20049},
		Instances: []emu.InstanceInfo{sampleInstance(0)}}, nil
}

func (f *fakeManager) Info(ctx context.Context, i int) (*emu.InstanceInfo, error) {
	f.record("info " + itoa(i))
	if f.info != nil {
		return f.info(i)
	}
	in := sampleInstance(i)
	return &in, nil
}

func (f *fakeManager) Create(ctx context.Context, req emu.CreateRequest) (*emu.InstanceInfo, error) {
	f.record("create")
	if f.create != nil {
		return f.create(req)
	}
	in := sampleInstance(1)
	return &in, nil
}

func (f *fakeManager) Update(ctx context.Context, i int, req emu.UpdateRequest) (*emu.UpdateResult, error) {
	f.record("update " + itoa(i))
	if f.update != nil {
		return f.update(i, req)
	}
	return &emu.UpdateResult{Instance: sampleInstance(i), Applied: map[string]any{}}, nil
}

func (f *fakeManager) Delete(ctx context.Context, i int) (*emu.DeleteResult, error) {
	f.record("delete " + itoa(i))
	return &emu.DeleteResult{Index: i, Name: root.AvdName(i), Path: `D:\r\avd\` + root.AvdName(i) + ".avd"}, nil
}

func (f *fakeManager) Start(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
	f.record("start " + itoa(i))
	if f.start != nil {
		return f.start(ctx, i, opt)
	}
	return &emu.StartResult{Instance: sampleInstance(i), Launched: true, Warnings: []string{}}, nil
}

func (f *fakeManager) Stop(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
	f.record("stop " + itoa(i))
	if f.stop != nil {
		return f.stop(ctx, i, opt)
	}
	return &emu.StopResult{Index: i, Name: root.AvdName(i), Method: "graceful", Synced: true, Warnings: []string{}}, nil
}

func (f *fakeManager) Check(ctx context.Context, opt emu.CheckOptions) (*emu.CheckResult, error) {
	f.record("check " + itoa(opt.MemoryMB))
	no := false
	return &emu.CheckResult{Ready: false, Items: []emu.CheckItem{{ID: "disk", Title: "磁盘", OK: &no, Blocking: true}}}, nil
}

func (f *fakeManager) SetWindowVisible(ctx context.Context, i int, visible bool) (*emu.WindowResult, error) {
	if visible {
		f.record("window " + itoa(i) + " show")
	} else {
		f.record("window " + itoa(i) + " hide")
	}
	return &emu.WindowResult{Index: i, Visible: visible, Windows: 2}, nil
}

func (f *fakeManager) Shell(ctx context.Context, i int, command string, timeout time.Duration) (*emu.ShellResult, error) {
	f.record("shell " + itoa(i) + " " + command + " " + timeout.String())
	if f.shell != nil {
		return f.shell(i, command, timeout)
	}
	return &emu.ShellResult{ExitCode: 0, Output: "ok"}, nil
}

func (f *fakeManager) AdbPassthrough(ctx context.Context, i int, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.record("adb " + itoa(i) + " " + strings.Join(args, " "))
	if f.adb != nil {
		return f.adb(i, args, stdout, stderr)
	}
	return 0, nil
}

func (f *fakeManager) Bridge(ctx context.Context, i int, action string) (*emu.BridgeResult, error) {
	f.record("bridge " + itoa(i) + " " + action)
	return &emu.BridgeResult{Index: i, Action: action, Installed: action == "install", Warnings: []string{}}, nil
}

func (f *fakeManager) InstallAPK(ctx context.Context, i int, apkPath string) (*emu.InstallResult, error) {
	f.record("install " + itoa(i) + " " + apkPath)
	if f.install != nil {
		return f.install(i, apkPath)
	}
	return &emu.InstallResult{Index: i, APK: apkPath, Output: "Success"}, nil
}

func (f *fakeManager) EnsureServers(ctx context.Context) (*emu.ServersResult, error) {
	f.record("servers")
	return &emu.ServersResult{AdbServerPort: 20050, AdbServerUp: true, ScriptAdbServerPort: 20049}, nil
}

func (f *fakeManager) Running(ctx context.Context) ([]int, error) {
	f.record("running")
	if f.running != nil {
		return f.running(ctx)
	}
	return nil, nil
}

func (f *fakeManager) Wait(timeout time.Duration) {
	f.mu.Lock()
	f.waits = append(f.waits, timeout)
	f.mu.Unlock()
	if f.wait != nil {
		f.wait(timeout)
	}
}

func (f *fakeManager) waitCalls() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration{}, f.waits...)
}

// harness runs the CLI against a fake manager.
type harness struct {
	fake     *fakeManager
	newCalls int
	rootErr  error // newManager fails with this
	mcpCall  func(ctx context.Context, in io.Reader, out io.Writer, opt mcp.Options) error
	resolve  func(string) (*root.Root, error)
}

type result struct {
	code           int
	stdout, stderr string
}

func (h *harness) run(t *testing.T, ctx context.Context, args ...string) result {
	t.Helper()
	if ctx == nil {
		ctx = context.Background()
	}
	var out, errb bytes.Buffer
	d := deps{
		newManager: func(rootFlag string, log *logx.Logger) (manager, error) {
			h.newCalls++
			if h.rootErr != nil {
				return nil, h.rootErr
			}
			h.fake.log = log
			return h.fake, nil
		},
		resolveRoot: h.resolve,
		serveMCP:    h.mcpCall,
	}
	if d.resolveRoot == nil {
		d.resolveRoot = func(string) (*root.Root, error) { return nil, errs.New(errs.NotFound, "没有根目录") }
	}
	if d.serveMCP == nil {
		d.serveMCP = func(context.Context, io.Reader, io.Writer, mcp.Options) error { return nil }
	}
	code := run(ctx, args, strings.NewReader(""), &out, &errb, d)
	return result{code, out.String(), errb.String()}
}

func newHarness() *harness { return &harness{fake: &fakeManager{}} }

// envelope decodes the single stdout line of a --json run.
func envelope(t *testing.T, stdout string) map[string]any {
	t.Helper()
	if strings.Count(stdout, "\n") != 1 || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("stdout is not exactly one line: %q", stdout)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(stdout), &m); err != nil {
		t.Fatalf("stdout is not JSON: %v: %q", err, stdout)
	}
	return m
}

func TestJSONEnvelopeSuccess(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "list", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d, stderr %s", r.code, r.stderr)
	}
	m := envelope(t, r.stdout)
	if m["ok"] != true {
		t.Fatalf("ok = %v", m["ok"])
	}
	data := m["data"].(map[string]any)
	if data["root"] != `D:\r` || len(data["instances"].([]any)) != 1 {
		t.Errorf("data = %v", data)
	}
	if _, has := m["error"]; has {
		t.Error("success envelope has error")
	}
	// Logs are NDJSON on stderr, Info by default (no Debug).
	lines := strings.Split(strings.TrimSpace(r.stderr), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr lines = %q", lines)
	}
	var log map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &log); err != nil || log["level"] != "info" || log["msg"] != "扫描实例" {
		t.Errorf("log line = %q (%v)", lines[0], err)
	}
}

func TestJSONEmptyListIsArray(t *testing.T) {
	h := newHarness()
	h.fake.list = func() (*emu.ListResult, error) { return &emu.ListResult{}, nil }
	r := h.run(t, nil, "--json", "list")
	if !strings.Contains(r.stdout, `"instances":[]`) {
		t.Errorf("stdout = %s", r.stdout)
	}
}

func TestJSONEnvelopeError(t *testing.T) {
	h := newHarness()
	h.fake.info = func(i int) (*emu.InstanceInfo, error) {
		return nil, errs.Newf(errs.NotFound, "实例 %d 不存在", i).WithAdvice("先用 modavd create 新建")
	}
	r := h.run(t, nil, "info", "mas_3", "--json")
	if r.code != 3 {
		t.Errorf("exit = %d, want 3", r.code)
	}
	m := envelope(t, r.stdout)
	if m["ok"] != false {
		t.Fatalf("ok = %v", m["ok"])
	}
	e := m["error"].(map[string]any)
	if e["code"] != "not_found" || e["message"] != "实例 3 不存在" || e["advice"] != "先用 modavd create 新建" {
		t.Errorf("error = %v", e)
	}
	if _, has := m["data"]; has {
		t.Error("error envelope has data")
	}

	h.fake.info = func(i int) (*emu.InstanceInfo, error) { return nil, errs.New(errs.Busy, "忙") }
	r = h.run(t, nil, "info", "3", "--json")
	if strings.Contains(r.stdout, "advice") {
		t.Errorf("empty advice not omitted: %s", r.stdout)
	}
	if r.code != 5 {
		t.Errorf("busy exit = %d", r.code)
	}
}

func TestHumanError(t *testing.T) {
	h := newHarness()
	h.fake.info = func(i int) (*emu.InstanceInfo, error) {
		return nil, errs.Wrap(errs.NotFound, errStr("detail"), "实例 3 不存在").WithAdvice("先新建")
	}
	r := h.run(t, nil, "info", "3")
	if r.code != 3 || r.stdout != "" {
		t.Errorf("exit %d stdout %q", r.code, r.stdout)
	}
	if !strings.Contains(r.stderr, "错误：实例 3 不存在\n建议：先新建\n") {
		t.Errorf("stderr = %q", r.stderr)
	}
	if strings.Contains(r.stderr, "detail") {
		t.Errorf("cause shown without --verbose: %q", r.stderr)
	}
	r = h.run(t, nil, "info", "3", "--verbose")
	if !strings.Contains(r.stderr, "[调试] 错误原因：detail") {
		t.Errorf("verbose stderr = %q", r.stderr)
	}
}

func TestExitCodes(t *testing.T) {
	codes := map[string]int{
		errs.Usage: 2, errs.InvalidArgument: 2, errs.NotFound: 3, errs.NotReady: 4, errs.PrecheckFailed: 4,
		errs.Unsupported: 4, errs.Busy: 5, errs.AlreadyExists: 5, errs.PortConflict: 5, errs.Timeout: 6,
		errs.LaunchFailed: 7, errs.Canceled: 8, errs.Internal: 1, errs.MetadataUnreadable: 1,
	}
	for code, want := range codes {
		for _, jsonMode := range []bool{false, true} {
			h := newHarness()
			h.fake.start = func(context.Context, int, emu.StartOptions) (*emu.StartResult, error) {
				return nil, errs.New(code, "失败")
			}
			args := []string{"start", "3"}
			if jsonMode {
				args = append(args, "--json")
			}
			r := h.run(t, nil, args...)
			if r.code != want {
				t.Errorf("%s json=%v: exit %d, want %d", code, jsonMode, r.code, want)
			}
			if jsonMode && !strings.Contains(r.stdout, `"code":"`+code+`"`) {
				t.Errorf("%s: stdout %s", code, r.stdout)
			}
		}
	}
	// A non-errs error is internal.
	h := newHarness()
	h.fake.start = func(context.Context, int, emu.StartOptions) (*emu.StartResult, error) { return nil, errStr("boom") }
	if r := h.run(t, nil, "start", "3", "--json"); r.code != 1 || !strings.Contains(r.stdout, `"code":"internal"`) {
		t.Errorf("plain error: %d %s", r.code, r.stdout)
	}
}

func TestUsageErrorsBeforeRoot(t *testing.T) {
	tests := [][]string{
		{"delete", "3"},
		{"set", "3"},
		{"start", "abc"},
		{"start", "3", "--timeout", "0"},
		{"window", "3", "maybe"},
		{"stop"},
		{"stop", "3", "--all"},
		{"info"},
		{"bogus"},
		{},
	}
	for _, args := range tests {
		h := newHarness()
		h.rootErr = errs.New(errs.NotFound, "没有根目录")
		r := h.run(t, nil, append(args, "--json")...)
		if r.code != 2 {
			t.Errorf("%q: exit %d, want 2 (%s)", args, r.code, r.stdout)
		}
		if h.newCalls != 0 {
			t.Errorf("%q: root looked up before the arguments were checked", args)
		}
		envelope(t, r.stdout)
	}
}

func TestDeleteNeedsYes(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "delete", "3")
	if r.code != 2 || !strings.Contains(r.stderr, "错误：删除会连同游戏数据一起删掉，确认请加 --yes") {
		t.Errorf("exit %d stderr %q", r.code, r.stderr)
	}
	r = h.run(t, nil, "delete", "3", "--yes")
	if r.code != 0 || !strings.Contains(r.stdout, "已删除实例 3") {
		t.Errorf("exit %d stdout %q", r.code, r.stdout)
	}
	if got := h.fake.callList(); len(got) != 1 || got[0] != "delete 3" {
		t.Errorf("calls = %q", got)
	}
}

func TestRootErrorReported(t *testing.T) {
	h := newHarness()
	h.rootErr = errs.New(errs.NotFound, "找不到 ModAVD 根目录").WithAdvice("用 --root 指定")
	r := h.run(t, nil, "list", "--json")
	m := envelope(t, r.stdout)
	if r.code != 3 || m["error"].(map[string]any)["advice"] != "用 --root 指定" {
		t.Errorf("exit %d stdout %s", r.code, r.stdout)
	}
}

func TestStopAllConcurrent(t *testing.T) {
	h := newHarness()
	// unsorted, with a duplicate: the targets are sorted and unique (3 is offline: not stopped)
	h.fake.running = func(context.Context) ([]int, error) { return []int{4, 0, 2, 4}, nil }
	var started sync.WaitGroup
	started.Add(3)
	release := make(chan struct{})
	go func() { started.Wait(); close(release) }()
	h.fake.stop = func(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
		if !opt.Force {
			t.Errorf("--force not passed")
		}
		started.Done()
		select {
		case <-release: // all three are inside Stop at the same time
		case <-time.After(5 * time.Second):
			t.Errorf("stops did not run concurrently")
		}
		if i == 2 {
			return nil, errs.New(errs.Busy, "实例 2 正在开机").WithAdvice("等它开完")
		}
		return &emu.StopResult{Index: i, Name: root.AvdName(i), Method: "force", Synced: true}, nil
	}
	r := h.run(t, nil, "stop", "--all", "--force", "--json")
	if r.code != 5 {
		t.Errorf("exit = %d, want 5 (first failure: busy)", r.code)
	}
	var env struct {
		OK   bool          `json:"ok"`
		Data StopAllResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || len(env.Data.Results) != 2 || env.Data.Results[0].Index != 0 || env.Data.Results[1].Index != 4 {
		t.Errorf("results = %+v", env.Data.Results)
	}
	if env.Data.Results[0].Warnings == nil {
		t.Error("warnings null")
	}
	if len(env.Data.Failed) != 1 || env.Data.Failed[0].Index != 2 || env.Data.Failed[0].Error.Code != errs.Busy ||
		env.Data.Failed[0].Error.Advice != "等它开完" {
		t.Errorf("failed = %+v", env.Data.Failed)
	}
	if !strings.Contains(r.stdout, `"failed":[{"index":2,"error":{"code":"busy"`) {
		t.Errorf("stdout = %s", r.stdout)
	}
	// The targets come from the process table: no List (private server, getprop, re-registration) before the stops.
	calls := h.fake.callList()
	if calls[0] != "running" || len(calls) != 4 {
		t.Errorf("calls = %q", calls)
	}
	for _, c := range calls {
		if c == "list" || strings.HasPrefix(c, "info") {
			t.Errorf("stop --all queried the instances: %q", calls)
		}
	}
}

func TestStopAllRunningError(t *testing.T) {
	h := newHarness()
	h.fake.running = func(context.Context) ([]int, error) {
		return nil, errs.New(errs.MetadataUnreadable, "mas-avd.json 读不了")
	}
	r := h.run(t, nil, "stop", "--all", "--json")
	if r.code != 1 || !strings.Contains(r.stdout, `"ok":false`) || !strings.Contains(r.stdout, "mas-avd.json 读不了") {
		t.Errorf("exit %d stdout %s", r.code, r.stdout)
	}
	if got := h.fake.callList(); len(got) != 1 {
		t.Errorf("calls = %q", got)
	}
}

func TestStopAllNoneRunning(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "stop", "--all", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `{"ok":true,"data":{"results":[],"failed":[]}}`) {
		t.Errorf("exit %d stdout %s", r.code, r.stdout)
	}
	r = h.run(t, nil, "stop", "--all")
	if r.code != 0 || !strings.Contains(r.stdout, "没有正在运行的实例") {
		t.Errorf("human: exit %d stdout %q", r.code, r.stdout)
	}
}

func TestRestart(t *testing.T) {
	h := newHarness()
	h.fake.stop = func(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
		if !opt.Force {
			t.Error("force not passed to stop")
		}
		return &emu.StopResult{Index: i, Method: "force", Warnings: []string{"关机警告"}}, nil
	}
	h.fake.start = func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		if !opt.Window || opt.Timeout != 90*time.Second {
			t.Errorf("start options = %+v", opt)
		}
		return &emu.StartResult{Instance: sampleInstance(i), Launched: true, Warnings: []string{"开机警告"}}, nil
	}
	r := h.run(t, nil, "restart", "3", "--force", "--window", "--timeout", "90", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stdout)
	}
	if got := h.fake.callList(); strings.Join(got, ",") != "stop 3,running,start 3" {
		t.Errorf("calls = %q", got)
	}
	if !strings.Contains(r.stdout, `"warnings":["关机警告","开机警告"]`) {
		t.Errorf("stdout = %s", r.stdout)
	}
	if !strings.Contains(r.stderr, "实例 3 已强制关机（先 sync 再结束），用时 0.0 秒，接着开机") {
		t.Errorf("stderr = %s", r.stderr)
	}
	// A failed stop does not start.
	h = newHarness()
	h.fake.stop = func(context.Context, int, emu.StopOptions) (*emu.StopResult, error) {
		return nil, errs.New(errs.Timeout, "关机超时")
	}
	if r := h.run(t, nil, "restart", "3"); r.code != 6 || len(h.fake.callList()) != 1 {
		t.Errorf("exit %d calls %q", r.code, h.fake.callList())
	}
}

// fastRestartPoll shortens the wait for qemu to exit for one test.
func fastRestartPoll(t *testing.T, wait time.Duration) {
	t.Helper()
	oldWait, oldPoll := restartExitWait, restartExitPoll
	restartExitWait, restartExitPoll = wait, 5*time.Millisecond
	t.Cleanup(func() { restartExitWait, restartExitPoll = oldWait, oldPoll })
}

func TestRestartWaitsForQemuExit(t *testing.T) {
	fastRestartPoll(t, 5*time.Second)
	h := newHarness()
	polls := 0
	h.fake.running = func(context.Context) ([]int, error) {
		polls++
		if polls <= 3 {
			return []int{1, 3}, nil // a qemu that is still ending after a force kill
		}
		return []int{1}, nil
	}
	r := h.run(t, nil, "restart", "3", "--force", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	if got := strings.Join(h.fake.callList(), ","); got != "stop 3,running,running,running,running,start 3" {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(r.stderr, "实例 3 的 qemu 进程还没退出，等它退出再开机") || strings.Count(r.stderr, "还没退出") != 1 {
		t.Errorf("stderr = %s", r.stderr)
	}
}

func TestRestartQemuStaysTimesOut(t *testing.T) {
	fastRestartPoll(t, 50*time.Millisecond)
	h := newHarness()
	h.fake.running = func(context.Context) ([]int, error) { return []int{3}, nil } // e.g. an elevated qemu
	started := time.Now()
	r := h.run(t, nil, "restart", "3", "--json")
	if r.code != 6 || !strings.Contains(r.stdout, `"code":"timeout"`) || !strings.Contains(r.stdout, "qemu 进程仍在运行，没有重新开机") {
		t.Errorf("exit %d stdout %s", r.code, r.stdout)
	}
	if time.Since(started) < 50*time.Millisecond {
		t.Errorf("gave up after %v", time.Since(started))
	}
	for _, c := range h.fake.callList() {
		if strings.HasPrefix(c, "start") {
			t.Errorf("started an instance whose qemu is still there: %q", h.fake.callList())
		}
	}
}

func TestRestartCanceledWhileWaiting(t *testing.T) {
	fastRestartPoll(t, 5*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHarness()
	h.fake.running = func(context.Context) ([]int, error) {
		cancel() // Ctrl+C while the qemu is still there
		return []int{3}, nil
	}
	r := h.run(t, ctx, "restart", "3", "--json")
	if r.code != 8 || !strings.Contains(r.stdout, `"code":"canceled"`) {
		t.Errorf("exit %d stdout %s", r.code, r.stdout)
	}
	if got := strings.Join(h.fake.callList(), ","); got != "stop 3,running" {
		t.Errorf("calls = %s", got)
	}
}

// fakeStoppedQemu replaces the check whether the process Stop ended still exists: alive answers it, and every
// process asked about is recorded.
func fakeStoppedQemu(t *testing.T, alive func(n int) bool) *[]win.Process {
	t.Helper()
	saved := stoppedQemuAlive
	t.Cleanup(func() { stoppedQemuAlive = saved })
	var mu sync.Mutex
	var asked []win.Process
	stoppedQemuAlive = func(p win.Process) bool {
		mu.Lock()
		asked = append(asked, p)
		n := len(asked)
		mu.Unlock()
		return alive(n)
	}
	return &asked
}

var stoppedQemu = win.Process{PID: 4242, Name: "qemu-system-x86_64-headless.exe", Created: 133_000_000_000_000_000}

// The qemu Stop terminated no longer counts as the instance's (its runtime file and run lock are gone, so Running
// omits it) but is still releasing its memory: restart waits for that very process to be gone before it starts.
func TestRestartWaitsForStoppedProcess(t *testing.T) {
	fastRestartPoll(t, 5*time.Second)
	h := newHarness()
	h.fake.stop = func(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
		return &emu.StopResult{Index: i, Method: "sync_kill+terminate", PID: int(stoppedQemu.PID), Process: stoppedQemu,
			Warnings: []string{}}, nil
	}
	asked := fakeStoppedQemu(t, func(n int) bool { return n <= 3 })
	r := h.run(t, nil, "restart", "3", "--json")
	if r.code != 0 {
		t.Fatalf("exit %d: %s %s", r.code, r.stdout, r.stderr)
	}
	if got := strings.Join(h.fake.callList(), ","); got != "stop 3,running,running,running,running,start 3" {
		t.Errorf("calls = %s", got)
	}
	if len(*asked) != 4 {
		t.Errorf("asked %d times", len(*asked))
	}
	for _, p := range *asked {
		if p != stoppedQemu {
			t.Errorf("asked about %+v, want the stopped qemu", p)
		}
	}
	if strings.Count(r.stderr, "实例 3 的 qemu 进程还没退出，等它退出再开机") != 1 {
		t.Errorf("stderr = %s", r.stderr)
	}
}

// Still there after restartExitWait: a timeout, no start, although Running had long stopped listing it.
func TestRestartStoppedProcessStaysTimesOut(t *testing.T) {
	fastRestartPoll(t, 50*time.Millisecond)
	h := newHarness()
	h.fake.stop = func(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
		return &emu.StopResult{Index: i, Method: "force", PID: int(stoppedQemu.PID), Process: stoppedQemu}, nil
	}
	fakeStoppedQemu(t, func(int) bool { return true })
	r := h.run(t, nil, "restart", "3", "--force", "--json")
	if r.code != 6 || !strings.Contains(r.stdout, "qemu 进程仍在运行，没有重新开机") {
		t.Errorf("exit %d stdout %s", r.code, r.stdout)
	}
	for _, c := range h.fake.callList() {
		if strings.HasPrefix(c, "start") {
			t.Errorf("started while the stopped qemu is still there: %q", h.fake.callList())
		}
	}
}

// An instance that was already offline has no process to wait for: no wait at all.
func TestRestartAlreadyOffline(t *testing.T) {
	fastRestartPoll(t, 5*time.Second)
	h := newHarness()
	h.fake.stop = func(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error) {
		return &emu.StopResult{Index: i, Method: "already_offline"}, nil
	}
	r := h.run(t, nil, "restart", "3", "--json")
	if r.code != 0 || strings.Join(h.fake.callList(), ",") != "stop 3,running,start 3" {
		t.Errorf("exit %d calls %q", r.code, h.fake.callList())
	}
}

// The real check, on this test process itself (never a qemu): the identity is the creation time; without one, the PID
// and the executable name.
func TestStoppedQemuAliveReal(t *testing.T) {
	if stoppedQemuAlive(win.Process{}) {
		t.Error("no process")
	}
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok || self.Created == 0 {
		t.Skipf("this process: %+v %v", self, ok)
	}
	if !stoppedQemuAlive(self) {
		t.Errorf("this process %+v", self)
	}
	reused := self
	reused.Created++
	if stoppedQemuAlive(reused) {
		t.Error("another creation time: a reused PID, not the stopped process")
	}
	noTime := self
	noTime.Created = 0
	if !stoppedQemuAlive(noTime) {
		t.Error("without a creation time, same PID and name")
	}
	noTime.Name = "qemu-system-x86_64.exe"
	if stoppedQemuAlive(noTime) {
		t.Error("without a creation time, another executable under the PID")
	}
	if stoppedQemuAlive(win.Process{PID: 0xFFFFFFFC, Created: 1}) {
		t.Error("an unused PID")
	}
}

func TestShell(t *testing.T) {
	h := newHarness()
	h.fake.shell = func(i int, cmd string, timeout time.Duration) (*emu.ShellResult, error) {
		return &emu.ShellResult{ExitCode: 3, Output: "no such file"}, nil
	}
	r := h.run(t, nil, "shell", "3", "--timeout", "15", "ls", "/x")
	if r.code != 3 || r.stdout != "no such file\n" {
		t.Errorf("human: exit %d stdout %q", r.code, r.stdout)
	}
	if got := h.fake.callList(); got[0] != "shell 3 ls /x 15s" {
		t.Errorf("call = %q", got[0])
	}
	r = h.run(t, nil, "shell", "--json", "3", "ls", "/x")
	m := envelope(t, r.stdout)
	if r.code != 0 || m["data"].(map[string]any)["exit_code"] != float64(3) {
		t.Errorf("json: exit %d stdout %s", r.code, r.stdout)
	}
	h.fake.shell = func(int, string, time.Duration) (*emu.ShellResult, error) {
		return &emu.ShellResult{ExitCode: -1, Output: "adb 没能执行"}, nil
	}
	if r := h.run(t, nil, "shell", "3", "id"); r.code != 1 {
		t.Errorf("negative exit code mapped to %d", r.code)
	}
}

func TestAdbPassthrough(t *testing.T) {
	h := newHarness()
	h.fake.adb = func(i int, args []string, stdout, stderr io.Writer) (int, error) {
		io.WriteString(stdout, "device\n")
		return 7, nil
	}
	r := h.run(t, nil, "adb", "3", "--json", "get-state", "--json")
	if r.code != 7 || r.stdout != "device\n" {
		t.Errorf("exit %d stdout %q", r.code, r.stdout)
	}
	if got := h.fake.callList(); got[0] != "adb 3 get-state --json" {
		t.Errorf("call = %q", got[0])
	}
	// Errors never become an envelope: stdout belongs to adb.
	h.fake.adb = func(int, []string, io.Writer, io.Writer) (int, error) {
		return 1, errs.New(errs.NotReady, "缺少 adb.exe")
	}
	r = h.run(t, nil, "--json", "adb", "3", "devices")
	if r.code != 4 || r.stdout != "" || !strings.Contains(r.stderr, `"level":"error"`) ||
		!strings.Contains(r.stderr, "缺少 adb.exe") {
		t.Errorf("json error: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	r = h.run(t, nil, "adb", "3", "devices")
	if r.code != 4 || r.stdout != "" || !strings.Contains(r.stderr, "错误：缺少 adb.exe") {
		t.Errorf("human error: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
	r = h.run(t, nil, "--json", "adb", "3")
	if r.code != 2 || r.stdout != "" {
		t.Errorf("usage: exit %d stdout %q", r.code, r.stdout)
	}
}

func TestCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := newHarness()
	h.fake.start = func(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error) {
		return nil, ctx.Err() // a plain context error
	}
	r := h.run(t, ctx, "start", "3", "--json")
	if r.code != 8 || !strings.Contains(r.stdout, `"code":"canceled"`) {
		t.Errorf("exit %d stdout %s", r.code, r.stdout)
	}
}

// TestCanceledQueryNeverOK: Ctrl+C during a query whose adb calls were cut short (an online instance seen as
// starting) ends with errs.Canceled, not with that result.
func TestCanceledQueryNeverOK(t *testing.T) {
	withFakeMemory(t)
	queries := [][]string{{"list"}, {"info", "3"}, {"status", "3"}, {"check"}, {"servers"}, {"shell", "3", "id"}}
	for _, args := range queries {
		for _, jsonMode := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			h := newHarness()
			// Ctrl+C arrives while the query runs; the manager still returns a result.
			h.fake.list = func() (*emu.ListResult, error) {
				cancel()
				in := sampleInstance(3)
				in.Running, in.Status = true, emu.StatusStarting
				return &emu.ListResult{Instances: []emu.InstanceInfo{in}}, nil
			}
			h.fake.info = func(i int) (*emu.InstanceInfo, error) {
				cancel()
				in := sampleInstance(i)
				in.Running, in.Status = true, emu.StatusStarting
				return &in, nil
			}
			h.fake.shell = func(int, string, time.Duration) (*emu.ShellResult, error) {
				cancel()
				return &emu.ShellResult{ExitCode: -1, Output: "已取消"}, nil
			}
			if args[0] == "check" || args[0] == "servers" {
				cancel() // those fakes have no hook: cancel before
			}
			a := args
			if jsonMode {
				a = append([]string{"--json"}, args...)
			}
			r := h.run(t, ctx, a...)
			cancel()
			if r.code != 8 {
				t.Errorf("%q: exit %d, want 8 (%s %s)", a, r.code, r.stdout, r.stderr)
			}
			if jsonMode {
				m := envelope(t, r.stdout)
				if m["ok"] != false || m["error"].(map[string]any)["code"] != errs.Canceled {
					t.Errorf("%q: %s", a, r.stdout)
				}
			} else if r.stdout != "" || !strings.Contains(r.stderr, "错误：已取消") {
				t.Errorf("%q: stdout %q stderr %q", a, r.stdout, r.stderr)
			}
		}
	}
}

// TestCanceledFinalCommandKeepsResult: a change that was made is reported as made, Ctrl+C or not (a stop that
// completed after Ctrl+C really shut the instance down).
func TestCanceledFinalCommandKeepsResult(t *testing.T) {
	withFakeMemory(t)
	finals := [][]string{{"stop", "3"}, {"stop", "--all"}, {"create"}, {"set", "3", "--cpu", "4"}, {"delete", "3", "--yes"},
		{"start", "3"}, {"restart", "3"}, {"window", "3", "show"}, {"install", "3", "x.apk"}, {"version"}, {"help"}}
	for _, args := range finals {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		h := newHarness()
		r := h.run(t, ctx, append([]string{"--json"}, args...)...)
		if r.code != 0 {
			t.Errorf("%q: exit %d (%s)", args, r.code, r.stdout)
			continue
		}
		if m := envelope(t, r.stdout); m["ok"] != true {
			t.Errorf("%q: %s", args, r.stdout)
		}
	}
	// adb keeps adb's own exit code
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h := newHarness()
	h.fake.adb = func(int, []string, io.Writer, io.Writer) (int, error) { return 3, nil }
	if r := h.run(t, ctx, "adb", "3", "devices"); r.code != 3 {
		t.Errorf("adb: exit %d", r.code)
	}
}

func TestWaitsForBackgroundWork(t *testing.T) {
	// After the output, and only for runs that built a manager.
	var out, errb bytes.Buffer
	f := &fakeManager{}
	f.wait = func(time.Duration) {
		if !strings.HasPrefix(out.String(), `{"ok":true`) {
			t.Errorf("waited before the result was written: %q", out.String())
		}
	}
	d := deps{
		newManager: func(_ string, log *logx.Logger) (manager, error) {
			f.log = log
			return f, nil
		},
		resolveRoot: func(string) (*root.Root, error) { return nil, errs.New(errs.NotFound, "没有根目录") },
	}
	if code := run(context.Background(), []string{"list", "--json"}, strings.NewReader(""), &out, &errb, d); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if got := f.waitCalls(); len(got) != 1 || got[0] != backgroundWait || backgroundWait != 10*time.Second {
		t.Errorf("waits = %v", got)
	}

	// a failed command waits too
	h := newHarness()
	h.fake.info = func(int) (*emu.InstanceInfo, error) { return nil, errs.New(errs.NotFound, "实例 3 不存在") }
	if r := h.run(t, nil, "info", "3"); r.code != 3 || len(h.fake.waitCalls()) != 1 {
		t.Errorf("failed info: exit %d waits %v", r.code, h.fake.waitCalls())
	}
	// no manager, no wait
	for _, args := range [][]string{{"version"}, {"help"}, {"delete", "3"}, {"mcp"}} {
		h := newHarness()
		h.run(t, nil, args...)
		if n := len(h.fake.waitCalls()); n != 0 || h.newCalls != 0 {
			t.Errorf("%q: %d waits, %d managers", args, n, h.newCalls)
		}
	}
}

func TestVersionNeedsNoRoot(t *testing.T) {
	h := newHarness()
	h.rootErr = errs.New(errs.NotFound, "没有根目录")
	r := h.run(t, nil, "version", "--json", "--root", `Z:\nowhere`)
	if r.code != 0 || h.newCalls != 0 {
		t.Fatalf("exit %d newCalls %d", r.code, h.newCalls)
	}
	var env struct {
		OK   bool          `json:"ok"`
		Data VersionResult `json:"data"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Data.Version != buildinfo.Version || env.Data.Protocol != buildinfo.Protocol ||
		env.Data.Go != runtime.Version() {
		t.Errorf("version = %+v", env.Data)
	}
	if !strings.Contains(r.stdout, `"version":`) || !strings.Contains(r.stdout, `"protocol":`) || !strings.Contains(r.stdout, `"go":`) {
		t.Errorf("field names: %s", r.stdout)
	}
	r = h.run(t, nil, "version")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "modavd "+buildinfo.Version) {
		t.Errorf("human: %q", r.stdout)
	}
}

func TestMainSetsOrphanHelper(t *testing.T) {
	old := win.OrphanHelper
	defer func() { win.OrphanHelper = old }()
	win.OrphanHelper = ""
	var out, errb bytes.Buffer
	code := Main([]string{"version", "--json"}, strings.NewReader(""), &out, &errb)
	if code != 0 || !strings.HasPrefix(out.String(), `{"ok":true,"data":{"version":`) {
		t.Errorf("exit %d stdout %q stderr %q", code, out.String(), errb.String())
	}
	exe, _ := os.Executable()
	if win.OrphanHelper == "" || win.OrphanHelper != exe {
		t.Errorf("OrphanHelper = %q, want %q", win.OrphanHelper, exe)
	}
}

// The boot watch's hidden subcommand reaches emu.RunBootWatch (bad arguments: exit 2, a JSON line on stderr), before
// any argument parsing or root lookup.
func TestMainRoutesBootWatch(t *testing.T) {
	var out, errb bytes.Buffer
	code := Main([]string{emu.BootWatchArg, "--json"}, strings.NewReader(""), &out, &errb)
	if code != 2 || out.Len() != 0 || !strings.Contains(errb.String(), `"level":"error"`) ||
		!strings.Contains(errb.String(), "开机守护的参数不对") {
		t.Errorf("exit %d stdout %q stderr %q", code, out.String(), errb.String())
	}
}

func TestHelp(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "help")
	if r.code != 0 {
		t.Fatalf("exit %d", r.code)
	}
	for _, c := range commands {
		if !strings.Contains(r.stdout, "  "+strings.TrimPrefix(c.usage(), "modavd ")) {
			t.Errorf("help lacks %s", c.name)
		}
	}
	for _, s := range []string{"--root", "--json", "--quiet", "--verbose", "退出码", "mas_3"} {
		if !strings.Contains(r.stdout, s) {
			t.Errorf("help lacks %q", s)
		}
	}
	r = h.run(t, nil, "help", "start")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "用法：modavd start <i>") || !strings.Contains(r.stdout, "\n选项：\n  --window ") ||
		!strings.Contains(r.stdout, "  --timeout 秒  最多等") {
		t.Errorf("help start: %q", r.stdout)
	}
	if r2 := h.run(t, nil, "start", "--help"); r2.stdout != r.stdout {
		t.Errorf("start --help differs from help start")
	}
	r = h.run(t, nil, "help", "--json")
	m := envelope(t, r.stdout)
	if len(m["data"].(map[string]any)["commands"].([]any)) != len(commands) {
		t.Errorf("help --json: %s", r.stdout)
	}
	if r := h.run(t, nil, "help", "bogus"); r.code != 2 {
		t.Errorf("help bogus: exit %d", r.code)
	}
	if r := h.run(t, nil, "adb", "--help"); r.code != 0 || !strings.Contains(r.stdout, "用法：modavd adb") {
		t.Errorf("adb --help: %d %q", r.code, r.stdout)
	}
	if r := h.run(t, nil, "--help"); r.code != 0 || !strings.Contains(r.stdout, "命令：") {
		t.Errorf("--help: %d", r.code)
	}
	if h.newCalls != 0 {
		t.Error("help looked up the root")
	}
}

func TestLogLevels(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "list")
	if !strings.Contains(r.stderr, "[信息] 扫描实例") || strings.Contains(r.stderr, "调试细节") {
		t.Errorf("default: %q", r.stderr)
	}
	r = h.run(t, nil, "list", "--quiet")
	if r.stderr != "" {
		t.Errorf("quiet: %q", r.stderr)
	}
	r = h.run(t, nil, "--verbose", "list")
	if !strings.Contains(r.stderr, "[调试] 调试细节") {
		t.Errorf("verbose: %q", r.stderr)
	}
	r = h.run(t, nil, "list", "--json", "--verbose")
	if !strings.Contains(r.stderr, `"level":"debug"`) {
		t.Errorf("json verbose: %q", r.stderr)
	}
}

func TestCommandsReachManager(t *testing.T) {
	withFakeMemory(t)
	tests := []struct {
		args []string
		call string
	}{
		{[]string{"info", "mas_2"}, "info 2"},
		{[]string{"status", "2"}, "info 2"},
		{[]string{"create", "--memory", "6G"}, "create"},
		{[]string{"set", "2", "--cpu", "4"}, "update 2"},
		{[]string{"start", "2"}, "start 2"},
		{[]string{"stop", "2"}, "stop 2"},
		{[]string{"check", "--memory", "4G"}, "check 4096"},
		{[]string{"check"}, "check 0"},
		{[]string{"window", "2", "SHOW"}, "window 2 show"},
		{[]string{"window", "2", "hide"}, "window 2 hide"},
		{[]string{"servers"}, "servers"},
	}
	for _, tc := range tests {
		for _, jsonMode := range []bool{false, true} {
			h := newHarness()
			args := tc.args
			if jsonMode {
				args = append([]string{"--json"}, args...)
			}
			r := h.run(t, nil, args...)
			if r.code != 0 {
				t.Errorf("%q: exit %d %s %s", args, r.code, r.stdout, r.stderr)
				continue
			}
			if got := h.fake.callList(); len(got) != 1 || got[0] != tc.call {
				t.Errorf("%q: calls %q, want %q", args, got, tc.call)
			}
			if jsonMode {
				if m := envelope(t, r.stdout); m["ok"] != true {
					t.Errorf("%q: %s", args, r.stdout)
				}
			} else if r.stdout == "" {
				t.Errorf("%q: no summary", args)
			}
		}
	}
}

func TestStatusSubset(t *testing.T) {
	h := newHarness()
	h.fake.info = func(i int) (*emu.InstanceInfo, error) {
		in := sampleInstance(i)
		in.Status, in.Running, in.Booted, in.PID, in.Busy = emu.StatusOnline, true, true, 4242, "stopping"
		return &in, nil
	}
	r := h.run(t, nil, "status", "3", "--json")
	want := `{"ok":true,"data":{"index":3,"name":"mas_3","status":"online","running":true,"booted":true,"busy":"stopping","pid":4242}}` + "\n"
	if r.stdout != want {
		t.Errorf("stdout = %s\nwant     %s", r.stdout, want)
	}
	h.fake.info = nil
	r = h.run(t, nil, "status", "3", "--json")
	want = `{"ok":true,"data":{"index":3,"name":"mas_3","status":"offline","running":false,"booted":false,"busy":"","pid":0}}` + "\n"
	if r.stdout != want {
		t.Errorf("offline stdout = %s", r.stdout)
	}
}

func TestInstallMakesPathAbsolute(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "install", "3", `pkg\game.apk`)
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	got := h.fake.callList()[0]
	if !strings.HasPrefix(got, "install 3 ") || !strings.HasSuffix(got, `\pkg\game.apk`) || !strings.Contains(got, ":\\") {
		t.Errorf("call = %q", got)
	}
}

func TestMCP(t *testing.T) {
	h := newHarness()
	var got mcp.Options
	h.mcpCall = func(ctx context.Context, in io.Reader, out io.Writer, opt mcp.Options) error {
		got = opt
		io.WriteString(out, "{\"jsonrpc\":\"2.0\"}\n")
		return nil
	}
	resolveCalls := 0
	h.resolve = func(string) (*root.Root, error) {
		resolveCalls++
		return nil, errs.New(errs.NotFound, "没有根目录")
	}
	r := h.run(t, nil, "mcp")
	if r.code != 0 || r.stdout != "{\"jsonrpc\":\"2.0\"}\n" {
		t.Errorf("exit %d stdout %q", r.code, r.stdout)
	}
	if got.Version != buildinfo.Version || got.NewManager == nil || got.Log == nil {
		t.Errorf("options = %+v", got)
	}
	if !strings.Contains(r.stderr, "没找到根目录") {
		t.Errorf("stderr = %q", r.stderr)
	}
	// A root missing at startup is looked up again per call.
	if _, err := got.NewManager(logx.Nop()); errs.CodeOf(err) != errs.NotFound || resolveCalls != 2 {
		t.Errorf("NewManager: %v, resolve calls %d", err, resolveCalls)
	}
	// Serve's error goes to stderr, never stdout.
	h.mcpCall = func(context.Context, io.Reader, io.Writer, mcp.Options) error {
		return errs.New(errs.Internal, "坏了")
	}
	r = h.run(t, nil, "mcp", "--json")
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "坏了") {
		t.Errorf("serve error: exit %d stdout %q stderr %q", r.code, r.stdout, r.stderr)
	}
}
