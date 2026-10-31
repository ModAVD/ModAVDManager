//go:build windows

package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/mcp"
	"modavd/internal/update"
	"modavd/internal/win"
)

// command is one subcommand: its help, its arguments and what it runs.
type command struct {
	name     string
	args     string // usage after the name
	summary  string
	details  string
	flags    []flagSpec
	posNames []string // what each positional argument is, for "缺少…"
	minPos   int
	maxPos   int // -1 = any number
	// passthrough: after <i> everything is taken verbatim (adb args, a guest command); restName says what it is and
	// makes it required.
	passthrough bool
	restName    string
	raw         bool // writes its own stdout (adb, mcp): no envelope, no summary
	// final: a successful result stands even when Ctrl+C came during the run, because it reports a change that was
	// made (an instance created, stopped ...) or does not depend on the context. Without it such a run fails with
	// errs.Canceled: a query whose adb calls were killed half way returns what it saw by then.
	final bool
	run   func(c *cmdCtx) (any, error)
	human func(w io.Writer, data any)
	exit  func(data any, json bool) int // exit code of a successful run; nil = 0
}

func (c *command) usage() string {
	if c.args == "" {
		return "modavd " + c.name
	}
	return "modavd " + c.name + " " + c.args
}

func (c *command) usageAdvice() string { return "用法：" + c.usage() }

const helpAdvice = "运行 modavd help 查看全部命令"

var (
	optIndex        = flagSpec{"index", kindString, "N", "实例编号（不给就取最小的空闲编号，端口也要空着）"}
	optTitle        = flagSpec{"title", kindString, "T", "显示名称"}
	optMemory       = flagSpec{"memory", kindString, "M", "内存：3072 / 4096 / 5120 / 6144 MB，也可写 6G"}
	optCPU          = flagSpec{"cpu", kindInt, "N", "核数：2 / 4 / 6"}
	optDataGB       = flagSpec{"data-gb", kindInt, "N", "数据盘大小（GB）：16–512"}
	optFPS          = flagSpec{"fps", kindInt, "F", "帧率：30 / 60"}
	optYes          = flagSpec{"yes", kindBool, "", "确认删除（连同游戏数据）"}
	optWindow       = flagSpec{"window", kindBool, "", "带窗口开机（默认无头）"}
	optTimeout      = flagSpec{"timeout", kindInt, "秒", "最多等 Android 开机多少秒（默认 300；到点只是不再等，模拟器不会被结束）"}
	optForce        = flagSpec{"force", kindBool, "", "跳过 Android 正常关机：先 sync，再直接结束"}
	optAll          = flagSpec{"all", kindBool, "", "同时关掉所有在运行的实例"}
	optShellTO      = flagSpec{"timeout", kindInt, "秒", "命令最多执行多少秒（不给用默认值）"}
	optCheckMB      = flagSpec{"memory", kindString, "M", "按这个实例内存判断电脑内存够不够（默认 6G）"}
	optNoBridge     = flagSpec{"no-bridge", kindBool, "", "不装 arm64 执行桥，以后开机也不装（默认 modavd / 管理器开机时自动装上）"}
	optUpdateCheck  = flagSpec{"check", kindBool, "", "只检查，不下载或替换"}
	optBootNoBridge = flagSpec{"no-bridge", kindBool, "",
		"这次开机不自动装执行桥（实例有执行桥的开机标记、桥还没装时默认会装）"}
)

var commands []*command

// The table is built in init: help's run function refers to it.
func init() {
	commands = []*command{
		{
			name: "list", summary: "列出全部实例和状态",
			details: "每台实例的编号、名称、状态、adb 地址、内存、核数、帧率。只对在运行的实例查一次是否开机完成。",
			run:     runList, human: humanOf(formatList),
		},
		{
			name: "info", args: "<i>", summary: "一台实例的详细信息",
			details:  "状态、端口、序列号、截图共享内存、配置、首次初始化、渲染器、路径。",
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			run: runInfo, human: humanOf(formatInfo),
		},
		{
			name: "status", args: "<i>", summary: "一台实例的状态",
			details: "status：offline 已关机 / starting 开机中 / online 运行中；running 是 qemu 进程在不在，booted 是 Android 开完没有；" +
				"busy 是另一个 modavd 正在开机（starting）或关机（stopping）它。",
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			run: runStatus, human: humanOf(formatStatus),
		},
		{
			name: "create", args: "[--index N] [--title T] [--memory 6G] [--cpu N] [--data-gb N] [--fps 30|60] [--no-bridge]",
			summary: "新建一台实例",
			details: "不给的选项用默认值：内存 6 GB、6 核、数据盘 64 GB、60 帧。内存和核数只能取列出的档位，不会自动凑整。" +
				"默认第一次用 modavd 或管理器开机时装上 arm64 执行桥（只对内置名单里的游戏生效，其余应用照旧），--no-bridge 不装。",
			flags: []flagSpec{optIndex, optTitle, optMemory, optCPU, optDataGB, optFPS, optNoBridge},
			final: true,
			run:   runCreate, human: humanOf(formatCreate),
		},
		{
			name: "set", args: "<i> [--title T] [--memory M] [--cpu N] [--fps F] [--data-gb N]",
			summary: "修改实例的标题、内存、核数、帧率、数据盘大小",
			details: "至少给一个选项。改动写进实例配置，下次开机生效。" +
				"--data-gb 只能改大（16–512），游戏数据不动：下次开机时模拟器（0.36 起）扩大磁盘镜像，开机后再在线扩大文件系统。",
			flags:    []flagSpec{optTitle, optMemory, optCPU, optFPS, optDataGB},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			final: true,
			run:   runSet, human: humanOf(formatUpdate),
		},
		{
			name: "delete", args: "<i> --yes", summary: "删除一台已关机的实例（连同游戏数据）",
			details:  "删掉 avd\\mas_<i>.avd（含数据盘）、mas_<i>.ini 和 mas-avd.json 里的记录。实例在运行或正在开关机时拒绝。",
			flags:    []flagSpec{optYes},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			final: true,
			run:   runDelete, human: humanOf(formatDelete),
		},
		{
			name: "start", args: "<i> [--window] [--timeout 300] [--no-bridge]", summary: "开机，等 Android 开完并做好开机后的设置",
			details: "默认无头启动，--window 带窗口。第一次开机还会做首次初始化。" +
				"按 Ctrl+C 或 --timeout 到点都只是不再等：模拟器不会被结束，会接着开机。" +
				"之后再运行一次 start 会接管它，把这次开机还没做完的设置补完（包括首次初始化和客体 Vulkan 修复）。" +
				"实例已经在运行、设置也都做完时，只重做每次开机都做的设置。" +
				"实例还没有 arm64 执行桥时，设置做完后自动装上（中间关机再开机一两次）；--no-bridge 这次不装，" +
				"用 bridge uninstall 卸过或新建时带 --no-bridge 的实例不装，接管已经在运行的实例时也不装。",
			flags:    []flagSpec{optWindow, optTimeout, optBootNoBridge},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			final: true,
			run:   runStart, human: humanOf(formatStart),
		},
		{
			name: "stop", args: "<i> [--force] | stop --all [--force]", summary: "关机",
			details: "先让 Android 正常关机，不成再 sync 后结束 qemu；--force 跳过正常关机，先 sync 再结束。" +
				"--all 按进程表找出所有在运行的实例，同时关掉，结果分 results 和 failed，有失败时退出码是第一个失败的退出码。",
			flags:    []flagSpec{optForce, optAll},
			posNames: []string{"实例编号"}, minPos: 0, maxPos: 1,
			final: true,
			run:   runStop, human: humanStop, exit: exitStop,
		},
		{
			name: "restart", args: "<i> [--window] [--timeout 300] [--force] [--no-bridge]", summary: "关机再开机",
			details: fmt.Sprintf("先关机（--force 同 stop），等 qemu 进程退出（最多 %.0f 秒，还在就报超时、不开机），"+
				"再开机（--window、--timeout、--no-bridge 同 start）。", restartExitWait.Seconds()),
			flags:    []flagSpec{optWindow, optTimeout, optForce, optBootNoBridge},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			final: true,
			run:   runRestart, human: humanOf(formatStart),
		},
		{
			name: "check", args: "[--memory M]", summary: "开机前的电脑检查",
			details: "路径、组件、模拟器版本、硬件加速、Vulkan、磁盘、内存。没通过也返回 0：看 ready（--json）或结论那一行。",
			flags:   []flagSpec{optCheckMB},
			run:     runCheck, human: humanOf(formatCheck),
		},
		{
			name: "window", args: "<i> show|hide", summary: "显示或隐藏实例的窗口",
			details:  "只对带窗口开机的实例有效；无头开机的实例没有窗口。",
			posNames: []string{"实例编号", "show 或 hide"}, minPos: 2, maxPos: 2,
			final: true,
			run:   runWindow, human: humanOf(formatWindow),
		},
		{
			name: "bridge", args: "<i> install|uninstall|verify", summary: "装、卸、检查 arm64 执行桥",
			details: "用 modavd 或管理器开机时会自动装上，一般不用手动跑。调用包里的 tools\\modavd-bridge.ps1：" +
				"install 需要时关机再开机，装上；已经装了的只在载荷有变时更新。uninstall 卸掉桥、开机标记和装桥时加的 asg，" +
				"并记下以后开机不再自动装（退回更早的版本之前要先卸）；再 install 就恢复。" +
				"实例原来关着的，做完再关掉。verify：只读核对，实例要开着。",
			posNames: []string{"实例编号", "install、uninstall 或 verify"}, minPos: 2, maxPos: 2,
			final: true,
			run:   runBridge, human: humanOf(formatBridge), exit: exitBridge,
		},
		{
			name: "install", args: "<i> <apk>", summary: "在已开机的实例上安装 APK（adb install -r）",
			details:  "最多等 900 秒（游戏安装包有 1–3 GB）。",
			posNames: []string{"实例编号", "APK 路径"}, minPos: 2, maxPos: 2,
			final: true,
			run:   runInstall, human: humanOf(formatInstall),
		},
		{
			name: "shell", args: "<i> [--timeout 秒] <命令...>", summary: "在实例里执行一条 shell 命令",
			details: "走私有 adb server，输出 stdout 和 stderr。不带 --json 时退出码就是命令的退出码。" +
				"modavd 的选项要写在命令前面；命令本身以 - 开头时先写 --。",
			flags:    []flagSpec{optShellTO},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			passthrough: true, restName: "要执行的命令",
			run: runShell, human: humanOf(formatShell), exit: exitShell,
		},
		{
			name: "adb", args: "<i> [--] <adb 参数...>", summary: "对实例执行 adb（私有 adb server）",
			details:  "相当于 adb -P <私有端口> -s <序列号> <adb 参数>：stdin、stdout、stderr 和退出码都是 adb 的，不输出 JSON。",
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			passthrough: true, restName: "adb 参数", raw: true,
			run: runAdb,
		},
		{
			name: "servers", summary: "确保私有 adb server 和脚本用的 adb server 在运行",
			details: "没在运行就以脱离方式起好，再报告两个端口的状态。",
			run:     runServers, human: humanOf(formatServers),
		},
		{
			name: "version", summary: "modavd 的版本和协议版本",
			details: "不需要根目录。protocol 是 --json 输出、退出码和 MCP 工具的版本，字段改了含义或去掉时才加一。",
			final:   true,
			run:     runVersion, human: humanOf(formatVersion),
		},
		{
			name: "self-update", args: "[--check]", summary: "检查或安装独立下载包的新版本",
			details: "只对单独下载包联网；ModAVD 根目录里的程序只提示随 ModAVD 更新。默认检查并在确认可替换时更新，--check 只检查。",
			flags:   []flagSpec{optUpdateCheck}, final: true,
			run: runSelfUpdate, human: humanOf(formatSelfUpdate),
		},
		{
			name: "mcp", summary: "在 stdin / stdout 上提供 MCP 服务",
			details: "给 AI 助手用。根目录启动时找一次，没找到时每次调用工具再找；日志只写 stderr。",
			raw:     true,
			run:     runMCP,
		},
		helpCommand(),
	}
}

var helpCmd = &command{
	name: "help", args: "[命令]", summary: "显示帮助",
	details:  "modavd help <命令> 显示一个命令的说明和选项。",
	posNames: []string{"命令"}, minPos: 0, maxPos: 1,
	final: true,
	run:   runHelp, human: humanOf(formatHelp),
}

func helpCommand() *command { return helpCmd }

func findCommand(name string) *command {
	for _, c := range commands {
		if c.name == name {
			return c
		}
	}
	return nil
}

// humanOf adapts a typed formatter to command.human.
func humanOf[T any](f func(io.Writer, *T)) func(io.Writer, any) {
	return func(w io.Writer, data any) {
		if v, ok := data.(*T); ok && v != nil {
			f(w, v)
		}
	}
}

// ---- results defined by the CLI ----------------------------------------------------------------------------------

// StatusResult is the `status` subset of emu.InstanceInfo (same JSON names, every field always present like there).
type StatusResult struct {
	Index   int        `json:"index"`
	Name    string     `json:"name"`
	Status  emu.Status `json:"status"`
	Running bool       `json:"running"`
	Booted  bool       `json:"booted"`
	Busy    string     `json:"busy"`
	PID     int        `json:"pid"`
}

// ErrorInfo is an error in JSON: the envelope's "error" and each stop --all failure.
type ErrorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Advice  string `json:"advice,omitempty"`
}

func errorInfo(err error) ErrorInfo {
	e := errs.As(err)
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	return ErrorInfo{Code: e.Code, Message: msg, Advice: e.Advice}
}

// StopFailure is one instance stop --all could not stop.
type StopFailure struct {
	Index int       `json:"index"`
	Error ErrorInfo `json:"error"`
}

// StopAllResult is `stop --all`: both lists sorted by index, never null.
type StopAllResult struct {
	Results []emu.StopResult `json:"results"`
	Failed  []StopFailure    `json:"failed"`
}

type VersionResult struct {
	Version  string `json:"version"`
	Protocol int    `json:"protocol"`
	Go       string `json:"go"`
}

type HelpEntry struct {
	Name    string       `json:"name"`
	Usage   string       `json:"usage"`
	Summary string       `json:"summary"`
	Details string       `json:"details"`
	Options []HelpOption `json:"options"` // never null
}

type HelpOption struct {
	Flag string `json:"flag"` // "--timeout 秒"
	Help string `json:"help"`
}

type HelpResult struct {
	Usage    string      `json:"usage"`
	Commands []HelpEntry `json:"commands"`
	// all: the overview (global flags, exit codes) is printed too; false for `help <command>`.
	all bool
}

// ---- request building (pure, tested directly) --------------------------------------------------------------------

func buildCreateRequest(inv *invocation) (emu.CreateRequest, error) {
	req := emu.CreateRequest{Title: inv.strOpt("title")}
	if inv.isSet("index") {
		i, err := parseIndex(inv.strOpt("index"))
		if err != nil {
			return req, err
		}
		req.Index = &i
	}
	if inv.isSet("memory") {
		mb, err := memoryOpt(inv.strOpt("memory"))
		if err != nil {
			return req, err
		}
		req.MemoryMB = &mb
	}
	req.CPU = intPtrOpt(inv, "cpu")
	req.DataPartitionGB = intPtrOpt(inv, "data-gb")
	req.FPS = intPtrOpt(inv, "fps")
	req.NoBridge = inv.boolOpt("no-bridge")
	return req, nil
}

func buildUpdateRequest(inv *invocation) (emu.UpdateRequest, error) {
	var req emu.UpdateRequest
	if inv.isSet("title") {
		t := inv.strOpt("title")
		req.Title = &t
	}
	if inv.isSet("memory") {
		mb, err := memoryOpt(inv.strOpt("memory"))
		if err != nil {
			return req, err
		}
		req.MemoryMB = &mb
	}
	req.CPU = intPtrOpt(inv, "cpu")
	req.FPS = intPtrOpt(inv, "fps")
	req.DataPartitionGB = intPtrOpt(inv, "data-gb")
	if req.Title == nil && req.MemoryMB == nil && req.CPU == nil && req.FPS == nil && req.DataPartitionGB == nil {
		return req, errs.New(errs.Usage, "没有要改的内容：至少给 --title、--memory、--cpu、--fps、--data-gb 中的一个").
			WithAdvice(findCommand("set").usageAdvice())
	}
	return req, nil
}

func buildStartOptions(inv *invocation) (emu.StartOptions, error) {
	opt := emu.StartOptions{Window: inv.boolOpt("window"), NoBridge: inv.boolOpt("no-bridge")}
	if inv.isSet("timeout") {
		s := inv.intOpt("timeout")
		if s <= 0 {
			return opt, errs.Newf(errs.InvalidArgument, "--timeout 要给大于 0 的秒数，收到 %d", s)
		}
		opt.Timeout = time.Duration(s) * time.Second
	}
	return opt, nil
}

func intPtrOpt(inv *invocation, name string) *int {
	if !inv.isSet(name) {
		return nil
	}
	v := inv.intOpt(name)
	return &v
}

func statusOf(in *emu.InstanceInfo) *StatusResult {
	return &StatusResult{Index: in.Index, Name: in.Name, Status: in.Status, Running: in.Running, Booted: in.Booted,
		Busy: in.Busy, PID: in.PID}
}

// ---- run functions -----------------------------------------------------------------------------------------------

// indexAndManager parses <i> (the first positional) and then gets the manager.
func (c *cmdCtx) indexAndManager() (int, manager, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return 0, nil, err
	}
	m, err := c.mgr()
	return i, m, err
}

func runList(c *cmdCtx) (any, error) {
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.List(c.ctx)
	if err != nil {
		return nil, err
	}
	if r.Instances == nil {
		r.Instances = []emu.InstanceInfo{}
	}
	return r, nil
}

func runInfo(c *cmdCtx) (any, error) {
	i, m, err := c.indexAndManager()
	if err != nil {
		return nil, err
	}
	in, err := m.Info(c.ctx, i)
	if err != nil {
		return nil, err
	}
	return in, nil
}

func runStatus(c *cmdCtx) (any, error) {
	i, m, err := c.indexAndManager()
	if err != nil {
		return nil, err
	}
	in, err := m.Info(c.ctx, i)
	if err != nil {
		return nil, err
	}
	return statusOf(in), nil
}

func runCreate(c *cmdCtx) (any, error) {
	req, err := buildCreateRequest(c.inv)
	if err != nil {
		return nil, err
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	in, err := m.Create(c.ctx, req)
	if err != nil {
		return nil, err
	}
	return in, nil
}

func runSet(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	req, err := buildUpdateRequest(c.inv)
	if err != nil {
		return nil, err
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.Update(c.ctx, i, req)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func runDelete(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	if !c.inv.boolOpt("yes") {
		return nil, errs.New(errs.Usage, "删除会连同游戏数据一起删掉，确认请加 --yes")
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.Delete(c.ctx, i)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func runStart(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	opt, err := buildStartOptions(c.inv)
	if err != nil {
		return nil, err
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.Start(c.ctx, i, opt)
	if err != nil {
		return nil, err
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	return r, nil
}

func runStop(c *cmdCtx) (any, error) {
	opt := emu.StopOptions{Force: c.inv.boolOpt("force")}
	if c.inv.boolOpt("all") {
		if len(c.inv.pos) > 0 {
			return nil, errs.New(errs.Usage, "stop --all 不能再给实例编号").WithAdvice(findCommand("stop").usageAdvice())
		}
		m, err := c.mgr()
		if err != nil {
			return nil, err
		}
		return stopAll(c, m, opt)
	}
	if len(c.inv.pos) == 0 {
		return nil, errs.New(errs.Usage, "缺少实例编号（要关掉全部请用 stop --all）").
			WithAdvice(findCommand("stop").usageAdvice())
	}
	i, m, err := c.indexAndManager()
	if err != nil {
		return nil, err
	}
	r, err := m.Stop(c.ctx, i, opt)
	if err != nil {
		return nil, err
	}
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	return r, nil
}

// stopAll stops every running instance concurrently: the caller (AUTO-MAS before a power action) gives the whole
// thing about 30 s, the same budget one stop has.
//
// The targets come from the process table only, like AUTO-MAS close_running_instances (_scan_processes). List would
// first start the private server and run a getprop with re-registration on each instance, up to 5 s for a stuck
// adbd, before the first stop begins; and a server it started would make each Stop trust it and skip its own "start
// the server and register" step, so an instance not registered yet would be killed without a sync.
func stopAll(c *cmdCtx, m manager, opt emu.StopOptions) (*StopAllResult, error) {
	running, err := m.Running(c.ctx)
	if err != nil {
		return nil, err
	}
	targets := slices.Clone(running)
	slices.Sort(targets)
	targets = slices.Compact(targets)
	res := &StopAllResult{Results: []emu.StopResult{}, Failed: []StopFailure{}}
	if len(targets) == 0 {
		c.log.Infof("没有正在运行的实例")
		return res, nil
	}
	c.log.Infof("同时正常关闭 %d 台魔改 AVD 实例（安卓正常关机）：%s", len(targets), joinInts(targets, "、"))
	type outcome struct {
		r   *emu.StopResult
		err error
	}
	outs := make([]outcome, len(targets))
	var wg sync.WaitGroup
	for k, i := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := m.Stop(c.ctx, i, opt)
			if err == nil && r == nil {
				err = errs.Newf(errs.Internal, "实例 %d 关机没有返回结果", i)
			}
			outs[k] = outcome{r, err}
		}()
	}
	wg.Wait()
	for k, o := range outs {
		if o.err != nil {
			e := o.err
			if c.ctx.Err() != nil && errs.CodeOf(e) != errs.Canceled {
				e = errs.Wrap(errs.Canceled, e, "已取消")
			}
			c.log.Warnf("魔改 AVD 实例 %d 关机失败: %s", targets[k], errs.As(e).Error())
			res.Failed = append(res.Failed, StopFailure{Index: targets[k], Error: errorInfo(e)})
			continue
		}
		r := *o.r
		if r.Warnings == nil {
			r.Warnings = []string{}
		}
		res.Results = append(res.Results, r)
	}
	return res, nil
}

func exitStop(data any, _ bool) int {
	if r, ok := data.(*StopAllResult); ok && len(r.Failed) > 0 {
		return errs.ExitCode(errs.New(r.Failed[0].Error.Code, r.Failed[0].Error.Message))
	}
	return 0
}

func humanStop(w io.Writer, data any) {
	switch r := data.(type) {
	case *emu.StopResult:
		formatStop(w, r)
	case *StopAllResult:
		formatStopAll(w, r)
	}
}

func runRestart(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	opt, err := buildStartOptions(c.inv)
	if err != nil {
		return nil, err
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	sr, err := m.Stop(c.ctx, i, emu.StopOptions{Force: c.inv.boolOpt("force")})
	if err != nil {
		return nil, err
	}
	c.log.Infof("%s，接着开机", stopLine(sr))
	if err := waitQemuGone(c, m, i, sr.Process); err != nil {
		return nil, err
	}
	r, err := m.Start(c.ctx, i, opt)
	if err != nil {
		return nil, err
	}
	// The stop's warnings belong to this restart too.
	r.Warnings = append(append([]string{}, sr.Warnings...), r.Warnings...)
	return r, nil
}

// How long restart waits for the qemu to be gone after Stop, and how often it looks (variables for tests).
var (
	restartExitWait = 15 * time.Second
	restartExitPoll = 500 * time.Millisecond
)

// stoppedQemuAlive: the very process Stop ended still exists (a variable for tests). With its creation time known,
// Process.Alive checks the identity, so a reused PID is not taken for it; without one only the PID and the executable
// name can be compared.
var stoppedQemuAlive = func(p win.Process) bool {
	if p.PID == 0 {
		return false // the instance was already offline: nothing to wait for
	}
	if p.Created != 0 {
		return p.Alive()
	}
	q, ok := win.FindProcess(p.PID)
	return ok && (p.Name == "" || strings.EqualFold(q.Name, p.Name))
}

// waitQemuGone waits until instance i is no longer running (process table only, no adb) and the qemu process Stop
// handled (stopped, zero when it was offline) is really gone, at most restartExitWait.
//
// Stop's kill paths do not confirm the exit: a big WHPX qemu can take seconds to end after TerminateProcess, and one
// running elevated cannot be ended by us at all. A Start right then would adopt that qemu, then either report a
// launch failure once it is gone or "succeed" without any restart. Running alone is not enough: it recognises an
// instance's qemu by its runtime file and run lock, which go away before a terminated qemu has released its memory
// and exited, so Running stops listing it while the process (and its gigabytes of guest memory) is still there.
func waitQemuGone(c *cmdCtx, m manager, i int, stopped win.Process) error {
	deadline := time.Now().Add(restartExitWait)
	logged := false
	for {
		running, err := m.Running(c.ctx)
		if err != nil {
			return err
		}
		if !slices.Contains(running, i) && !stoppedQemuAlive(stopped) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errs.Newf(errs.Timeout, "实例 %d 关机后等了 %.0f 秒，qemu 进程仍在运行，没有重新开机", i,
				restartExitWait.Seconds()).
				WithAdvice(fmt.Sprintf("等 qemu 退出后再运行 modavd start %d；它一直不退出时，可能是以管理员身份运行的，"+
					"modavd 结束不了它", i))
		}
		if !logged {
			c.log.Infof("实例 %d 的 qemu 进程还没退出，等它退出再开机（最多 %.0f 秒）", i, restartExitWait.Seconds())
			logged = true
		}
		t := time.NewTimer(restartExitPoll)
		select {
		case <-c.ctx.Done():
			t.Stop()
			return errs.Wrap(errs.Canceled, c.ctx.Err(), "已取消")
		case <-t.C:
		}
	}
}

func runCheck(c *cmdCtx) (any, error) {
	var opt emu.CheckOptions
	if c.inv.isSet("memory") {
		mb, err := memoryOpt(c.inv.strOpt("memory"))
		if err != nil {
			return nil, err
		}
		opt.MemoryMB = mb
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.Check(c.ctx, opt)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func runWindow(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	var visible bool
	switch strings.ToLower(c.inv.pos[1]) {
	case "show":
		visible = true
	case "hide":
	default:
		return nil, errs.Newf(errs.Usage, "第二个参数只能是 show 或 hide，收到「%s」", c.inv.pos[1]).
			WithAdvice(findCommand("window").usageAdvice())
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.SetWindowVisible(c.ctx, i, visible)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func runBridge(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	action := strings.ToLower(c.inv.pos[1])
	switch action {
	case "install", "uninstall", "verify":
	default:
		return nil, errs.Newf(errs.Usage, "第二个参数只能是 install、uninstall 或 verify，收到「%s」", c.inv.pos[1]).
			WithAdvice(findCommand("bridge").usageAdvice())
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	return m.Bridge(c.ctx, i, action)
}

// exitBridge: 1 when the bridge did not end up as asked (installed after install, gone after uninstall), the script's
// exit code for verify. --json: 0, the result says it.
func exitBridge(data any, json bool) int {
	r, ok := data.(*emu.BridgeResult)
	if json || !ok {
		return 0
	}
	switch r.Action {
	case "install":
		if !r.Installed {
			return 1
		}
	case "uninstall":
		if r.Installed {
			return 1
		}
	default:
		if r.ExitCode < 0 || r.ExitCode > 255 {
			return 1
		}
		return r.ExitCode
	}
	return 0
}

func runInstall(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	apk := c.inv.pos[1]
	if abs, err := filepath.Abs(apk); err == nil {
		apk = abs
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.InstallAPK(c.ctx, i, apk)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func runShell(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	var timeout time.Duration
	if c.inv.isSet("timeout") {
		s := c.inv.intOpt("timeout")
		if s <= 0 {
			return nil, errs.Newf(errs.InvalidArgument, "--timeout 要给大于 0 的秒数，收到 %d", s)
		}
		timeout = time.Duration(s) * time.Second
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	// Joined like `adb shell a b c`: the guest shell parses the words again.
	r, err := m.Shell(c.ctx, i, strings.Join(c.inv.rest, " "), timeout)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func exitShell(data any, json bool) int {
	r, ok := data.(*emu.ShellResult)
	if json || !ok {
		return 0
	}
	if r.ExitCode < 0 || r.ExitCode > 255 {
		return 1
	}
	return r.ExitCode
}

func runAdb(c *cmdCtx) (any, error) {
	i, m, err := c.indexAndManager()
	if err != nil {
		return nil, err
	}
	code, err := m.AdbPassthrough(c.ctx, i, c.inv.rest, c.stdin, c.stdout, c.stderr)
	if err != nil {
		return nil, err
	}
	return rawExit(code), nil
}

func runServers(c *cmdCtx) (any, error) {
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	r, err := m.EnsureServers(c.ctx)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func runVersion(c *cmdCtx) (any, error) {
	return &VersionResult{Version: buildinfo.Version, Protocol: buildinfo.Protocol, Go: runtime.Version()}, nil
}

func runSelfUpdate(c *cmdCtx) (any, error) {
	if c.inv.isSet("check") {
		return update.Check(c.ctx, update.Options{})
	}
	return update.SelfUpdate(c.ctx, update.Options{})
}

func formatSelfUpdate(w io.Writer, r *update.Result) {
	if r.Message != "" {
		fmt.Fprintln(w, r.Message)
	}
}

func runMCP(c *cmdCtx) (any, error) {
	r, rerr := c.deps.resolveRoot(c.inv.root)
	if rerr != nil {
		c.log.Warnf("没找到根目录：%s（之后每次调用工具时再找）", errs.As(rerr).Error())
	} else {
		c.log.Infof("MCP 服务已就绪，根目录 %s", r.Dir)
	}
	var mu sync.Mutex
	opt := mcp.Options{
		Version: buildinfo.Version,
		Log:     c.log,
		NewManager: func(log *logx.Logger) (*emu.Manager, error) {
			mu.Lock()
			defer mu.Unlock()
			if r == nil {
				var err error
				if r, err = c.deps.resolveRoot(c.inv.root); err != nil {
					r = nil
					return nil, err
				}
			}
			return emu.New(r, log), nil
		},
	}
	if err := c.deps.serveMCP(c.ctx, c.stdin, c.stdout, opt); err != nil {
		return nil, err
	}
	return rawExit(0), nil
}

func runHelp(c *cmdCtx) (any, error) {
	if len(c.inv.pos) == 0 {
		return helpAll(), nil
	}
	cmd := findCommand(c.inv.pos[0])
	if cmd == nil {
		return nil, errs.Newf(errs.Usage, "未知命令 %s", c.inv.pos[0]).WithAdvice(helpAdvice)
	}
	return helpFor(cmd), nil
}

const globalUsage = "modavd [--root <目录>] [--json] [--quiet] [--verbose] <命令> [参数]"

func entryOf(c *command) HelpEntry {
	e := HelpEntry{Name: c.name, Usage: c.usage(), Summary: c.summary, Details: c.details, Options: []HelpOption{}}
	for _, f := range c.flags {
		flag := "--" + f.name
		if f.arg != "" {
			flag += " " + f.arg
		}
		e.Options = append(e.Options, HelpOption{Flag: flag, Help: f.help})
	}
	return e
}

func helpAll() *HelpResult {
	r := &HelpResult{Usage: globalUsage, Commands: []HelpEntry{}, all: true}
	for _, c := range commands {
		r.Commands = append(r.Commands, entryOf(c))
	}
	return r
}

func helpFor(c *command) *HelpResult {
	return &HelpResult{Usage: globalUsage, Commands: []HelpEntry{entryOf(c)}}
}
