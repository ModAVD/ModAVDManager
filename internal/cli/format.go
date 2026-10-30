//go:build windows

package cli

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"modavd/internal/emu"
)

// Human-readable summaries (stdout without --json). Wording follows AUTO-MAS where it has a message for the same
// thing; nothing here is machine-readable, scripts use --json.

func statusText(s emu.Status) string {
	switch s {
	case emu.StatusOffline:
		return "已关机"
	case emu.StatusStarting:
		return "开机中"
	case emu.StatusOnline:
		return "运行中"
	case "":
		return "未知"
	}
	return string(s)
}

func busyText(b string) string {
	switch b {
	case "starting":
		return "正在开机"
	case "stopping":
		return "正在关机"
	}
	return b
}

func fmtMemory(mb int) string {
	if mb <= 0 {
		return "-"
	}
	if mb%1024 == 0 {
		return strconv.Itoa(mb/1024) + " GB"
	}
	return strconv.Itoa(mb) + " MB"
}

func fmtGB(gb int) string {
	if gb <= 0 {
		return "-"
	}
	return strconv.Itoa(gb) + " GB"
}

func intOrDash(n int) string {
	if n <= 0 {
		return "-"
	}
	return strconv.Itoa(n)
}

func joinInts(ns []int, sep string) string {
	s := make([]string, len(ns))
	for k, n := range ns {
		s[k] = strconv.Itoa(n)
	}
	return strings.Join(s, sep)
}

// instLabel is "实例 3", with the title when it is not just the AVD name.
func instLabel(index int, name, title string) string {
	if title != "" && title != name {
		return fmt.Sprintf("实例 %d（%s）", index, title)
	}
	return fmt.Sprintf("实例 %d", index)
}

// subject is instLabel followed by Chinese text: "实例 3 已开机" but "实例 3（星铁）已开机".
func subject(index int, name, title string) string {
	l := instLabel(index, name, title)
	if l[len(l)-1] < 0x80 {
		l += " "
	}
	return l
}

// runeWidth is the terminal width of r: 2 for East Asian wide / fullwidth characters, else 1.
func runeWidth(r rune) int {
	switch {
	case r < 0x1100:
		return 1
	case r <= 0x115F, // Hangul Jamo
		r >= 0x2E80 && r <= 0x303E, // CJK radicals, punctuation
		r >= 0x3041 && r <= 0x33FF, // kana, CJK compatibility
		r >= 0x3400 && r <= 0x4DBF, // CJK extension A
		r >= 0x4E00 && r <= 0x9FFF, // CJK unified
		r >= 0xA000 && r <= 0xA4CF, // Yi
		r >= 0xAC00 && r <= 0xD7A3, // Hangul syllables
		r >= 0xF900 && r <= 0xFAFF, // CJK compatibility ideographs
		r >= 0xFE30 && r <= 0xFE4F, // CJK compatibility forms
		r >= 0xFF00 && r <= 0xFF60, // fullwidth forms
		r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x20000 && r <= 0x3FFFD:
		return 2
	}
	return 1
}

func displayWidth(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// writeTable aligns columns by display width (text/tabwriter counts runes, which misaligns Chinese).
func writeTable(w io.Writer, rows [][]string) {
	var widths []int
	for _, row := range rows {
		for k, cell := range row {
			if k >= len(widths) {
				widths = append(widths, 0)
			}
			widths[k] = max(widths[k], displayWidth(cell))
		}
	}
	for _, row := range rows {
		var b strings.Builder
		for k, cell := range row {
			if k > 0 {
				b.WriteString("  ")
			}
			b.WriteString(cell)
			b.WriteString(strings.Repeat(" ", widths[k]-displayWidth(cell)))
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

func writeWarnings(w io.Writer, warnings []string) {
	for _, s := range warnings {
		fmt.Fprintf(w, "警告：%s\n", s)
	}
}

func writeRootLines(w io.Writer, ri emu.RootInfo) {
	version := ri.Version
	if version == "" {
		version = "版本未知"
	}
	fmt.Fprintf(w, "根目录：%s（ModAVD %s）\n", ri.Root, version)
	if ri.LocalSDK {
		fmt.Fprintf(w, "SDK：%s（mas-avd.json 的 sdkRoot）\n", ri.SDK)
	}
	fmt.Fprintf(w, "adb server：私有 %d，脚本用 %d\n", ri.AdbServerPort, ri.ScriptAdbServerPort)
}

// ---- per command -------------------------------------------------------------------------------------------------

func formatList(w io.Writer, r *emu.ListResult) {
	writeRootLines(w, r.RootInfo)
	if len(r.Instances) == 0 {
		fmt.Fprintln(w, "还没有实例，用 modavd create 新建一台")
		return
	}
	rows := [][]string{{"编号", "名称", "状态", "adb 地址", "内存", "核数", "帧率", "备注"}}
	for _, in := range r.Instances {
		title := in.Title
		if title == "" {
			title = in.Name
		}
		rows = append(rows, []string{
			strconv.Itoa(in.Index), title, statusText(in.Status), in.AdbAddress,
			fmtMemory(in.Config.MemoryMB), intOrDash(in.Config.CPU), intOrDash(in.Config.FPS),
			strings.Join(listNotes(in), "，"),
		})
	}
	writeTable(w, rows)
}

func listNotes(in emu.InstanceInfo) []string {
	var notes []string
	if b := busyText(in.Busy); b != "" {
		notes = append(notes, b)
	}
	if !in.Initialized {
		notes = append(notes, "未初始化")
	}
	if in.SoftwareRenderer {
		notes = append(notes, "软件渲染")
	}
	return notes
}

func statusDetail(status emu.Status, pid int, startedAt, busy string) string {
	s := statusText(status)
	var extra []string
	if pid != 0 {
		extra = append(extra, "PID "+strconv.Itoa(pid))
	}
	if startedAt != "" {
		extra = append(extra, "启动于 "+startedAt)
	}
	if b := busyText(busy); b != "" {
		extra = append(extra, b)
	}
	if len(extra) > 0 {
		s += "（" + strings.Join(extra, "，") + "）"
	}
	return s
}

func formatInfo(w io.Writer, in *emu.InstanceInfo) {
	kv := func(k, v string) { fmt.Fprintf(w, "%s：%s\n", k, v) }
	kv("编号", strconv.Itoa(in.Index))
	kv("名称", in.Name)
	if in.Title != "" && in.Title != in.Name {
		kv("标题", in.Title)
	}
	kv("状态", statusDetail(in.Status, in.PID, in.StartedAt, in.Busy))
	kv("adb 地址", in.AdbAddress)
	kv("序列号", in.Serial+"（私有 adb server 上）")
	kv("端口", fmt.Sprintf("控制台 %d，adb %d，gRPC %d", in.Ports.Console, in.Ports.Adb, in.Ports.Grpc))
	shm := in.Screencap.Shm
	if in.Screencap.ShmReady {
		shm += "（已就绪）"
	} else {
		shm += "（未就绪）"
	}
	kv("截图共享内存", shm)
	cfg := in.Config
	kv("内存", fmtMemory(cfg.MemoryMB))
	kv("核数", intOrDash(cfg.CPU))
	kv("帧率", intOrDash(cfg.FPS))
	kv("数据盘", fmtGB(cfg.DataPartitionGB))
	if cfg.Width > 0 && cfg.Height > 0 {
		kv("显示", fmt.Sprintf("%dx%d，%d dpi", cfg.Width, cfg.Height, cfg.DPI))
	}
	if in.Initialized {
		kv("首次初始化", "已完成")
	} else {
		kv("首次初始化", "还没做（第一次开机时做）")
	}
	if in.Launcher != "" {
		kv("桌面", in.Launcher)
	}
	if in.Renderer != "" || in.SoftwareRenderer {
		r := in.Renderer
		if in.SoftwareRenderer {
			r = strings.TrimSpace(r + " 软件渲染（没有可用的显卡驱动）")
		}
		kv("渲染器", r)
	}
	kv("路径", in.Path)
}

func formatStatus(w io.Writer, s *StatusResult) {
	fmt.Fprintf(w, "实例 %d：%s\n", s.Index, statusDetail(s.Status, s.PID, "", s.Busy))
}

func formatCreate(w io.Writer, in *emu.InstanceInfo) {
	cfg := in.Config
	fmt.Fprintf(w, "已创建%s：内存 %s，%s 核，%s 帧，数据盘 %s\n", instLabel(in.Index, in.Name, in.Title),
		fmtMemory(cfg.MemoryMB), intOrDash(cfg.CPU), intOrDash(cfg.FPS), fmtGB(cfg.DataPartitionGB))
	fmt.Fprintf(w, "adb 地址：%s\n", in.AdbAddress)
	fmt.Fprintf(w, "路径：%s\n", in.Path)
}

// appliedOrder is the order changes are listed in; unknown keys follow alphabetically.
var appliedOrder = []string{"title", "memory_mb", "cpu", "fps", "data_partition_gb"}

func appliedParts(applied map[string]any) []string {
	keys := make([]string, 0, len(applied))
	for k := range applied {
		keys = append(keys, k)
	}
	rank := func(k string) int {
		for n, o := range appliedOrder {
			if o == k {
				return n
			}
		}
		return len(appliedOrder)
	}
	sort.Slice(keys, func(a, b int) bool {
		ra, rb := rank(keys[a]), rank(keys[b])
		if ra != rb {
			return ra < rb
		}
		return keys[a] < keys[b]
	})
	var parts []string
	for _, k := range keys {
		v := applied[k]
		switch k {
		case "title":
			parts = append(parts, fmt.Sprintf("标题「%v」", v))
		case "memory_mb":
			if mb, ok := asInt(v); ok {
				parts = append(parts, "内存 "+fmtMemory(mb))
			} else {
				parts = append(parts, fmt.Sprintf("内存 %v", v))
			}
		case "cpu":
			parts = append(parts, fmt.Sprintf("核数 %v", v))
		case "fps":
			parts = append(parts, fmt.Sprintf("帧率 %v", v))
		case "data_partition_gb":
			parts = append(parts, fmt.Sprintf("数据盘 %v GB（下次开机扩大）", v))
		default:
			parts = append(parts, fmt.Sprintf("%s %v", k, v))
		}
	}
	return parts
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	}
	return 0, false
}

func formatUpdate(w io.Writer, r *emu.UpdateResult) {
	parts := appliedParts(r.Applied)
	if len(parts) == 0 {
		fmt.Fprintf(w, "%s没有改动\n", subject(r.Instance.Index, r.Instance.Name, r.Instance.Title))
		return
	}
	fmt.Fprintf(w, "已修改%s：%s\n", instLabel(r.Instance.Index, r.Instance.Name, r.Instance.Title), strings.Join(parts, "，"))
	if r.AppliesNextBoot {
		fmt.Fprintln(w, "实例正在运行，改动下次开机生效")
	}
}

func formatDelete(w io.Writer, r *emu.DeleteResult) {
	fmt.Fprintf(w, "已删除实例 %d（%s），连同数据目录 %s\n", r.Index, r.Name, r.Path)
}

func formatStart(w io.Writer, r *emu.StartResult) {
	in := r.Instance
	label := subject(in.Index, in.Name, in.Title)
	if r.Launched {
		fmt.Fprintf(w, "%s已开机：开机 %.1f 秒，合计 %.1f 秒\n", label, r.BootSeconds, r.TotalSeconds)
	} else {
		fmt.Fprintf(w, "%s本来就在运行，已重新做开机后的设置（%.1f 秒）\n", label, r.TotalSeconds)
	}
	fmt.Fprintf(w, "adb 地址：%s（私有 adb server 上是 %s）\n", in.AdbAddress, in.Serial)
	if r.FirstBoot {
		fmt.Fprintln(w, "已完成首次初始化")
	}
	if r.ZygoteRestarted {
		fmt.Fprintln(w, "挂上了客体 Vulkan 修复，zygote 已重启（应用都重新开始）")
	}
	if r.SetupPending {
		fmt.Fprintf(w, "这次开机的一次性设置（首次初始化、客体 Vulkan 修复）还没做完，再运行一次 modavd start %d 补完\n",
			in.Index)
	}
	if r.LogFile != "" {
		fmt.Fprintf(w, "模拟器日志：%s\n", r.LogFile)
	}
	if r.LogcatFile != "" {
		fmt.Fprintf(w, "logcat：%s\n", r.LogcatFile)
	}
	writeWarnings(w, r.Warnings)
}

// stopMethodText describes StopResult.Method.
func stopMethodText(method string) string {
	base, terminated := strings.CutSuffix(method, "+terminate")
	var s string
	switch base {
	case "already_offline":
		return "本来就没在运行"
	case "graceful":
		s = "已正常关机"
	case "sync_kill":
		s = "正常关机没完成，已先 sync 再从控制台结束"
	case "console_kill":
		s = "私有 adb 用不了，已从控制台结束（没能先 sync）"
	case "launching_kill":
		// Not console_kill's text: here no data was at risk (Android had not booted), so "没能先 sync" would alarm.
		s = "模拟器刚拉起、安卓还没开机，已直接关掉（不用 sync）"
	case "force":
		s = "已强制关机（先 sync 再结束）"
	default:
		s = "已关机（" + method + "）"
	}
	if terminated {
		s += "，最后强行结束了 qemu 进程"
	}
	return s
}

func stopLine(r *emu.StopResult) string {
	s := fmt.Sprintf("实例 %d %s", r.Index, stopMethodText(r.Method))
	if r.Method != "already_offline" {
		s += fmt.Sprintf("，用时 %.1f 秒", r.Seconds)
	}
	return s
}

func formatStop(w io.Writer, r *emu.StopResult) {
	fmt.Fprintln(w, stopLine(r))
	writeWarnings(w, r.Warnings)
}

func formatStopAll(w io.Writer, r *StopAllResult) {
	if len(r.Results) == 0 && len(r.Failed) == 0 {
		fmt.Fprintln(w, "没有正在运行的实例")
		return
	}
	for k := range r.Results {
		formatStop(w, &r.Results[k])
	}
	for _, f := range r.Failed {
		fmt.Fprintf(w, "实例 %d 关机失败：%s\n", f.Index, f.Error.Message)
		if f.Error.Advice != "" {
			fmt.Fprintf(w, "  建议：%s\n", f.Error.Advice)
		}
	}
}

func checkMark(ok *bool) string {
	switch {
	case ok == nil:
		return "?"
	case *ok:
		return "✓"
	}
	return "✗"
}

func formatCheck(w io.Writer, r *emu.CheckResult) {
	fmt.Fprintf(w, "根目录：%s\n", r.Root)
	if r.Ready {
		fmt.Fprintln(w, "结论：可以开机")
	} else {
		fmt.Fprintln(w, "结论：有拦截项没通过，现在不能开机")
	}
	for _, it := range r.Items {
		line := checkMark(it.OK) + " " + it.Title
		if it.Reason != "" {
			line += "：" + it.Reason
		}
		if it.OK == nil {
			line += "（现在查不了）"
		} else if !*it.OK {
			if it.Blocking {
				line += "（拦截）"
			} else {
				line += "（只提醒）"
			}
		}
		fmt.Fprintln(w, line)
		if it.Advice != "" && (it.OK == nil || !*it.OK) {
			fmt.Fprintf(w, "  建议：%s\n", it.Advice)
		}
	}
	if len(r.Components) > 0 {
		fmt.Fprintln(w, "组件：")
		for _, c := range r.Components {
			ok := c.Installed
			line := "  " + checkMark(&ok) + " " + c.Name
			if c.Installed {
				if c.Version != "" {
					line += " " + c.Version
				}
				if c.Build != "" {
					line += "（ModAVD " + c.Build + "）"
				}
			} else {
				line += " 未安装"
			}
			if c.Optional {
				line += "（可选）"
			}
			if c.LocalSDK {
				line += "（本地 SDK）"
			}
			fmt.Fprintln(w, line)
		}
	}
}

func formatWindow(w io.Writer, r *emu.WindowResult) {
	switch {
	case r.Windows == 0:
		fmt.Fprintf(w, "实例 %d 没有窗口（无头开机的实例没有窗口；要窗口请用 start --window 开机）\n", r.Index)
	case r.Visible:
		fmt.Fprintf(w, "实例 %d 的窗口已显示（%d 个）\n", r.Index, r.Windows)
	default:
		fmt.Fprintf(w, "实例 %d 的窗口已隐藏（%d 个）\n", r.Index, r.Windows)
	}
}

func formatInstall(w io.Writer, r *emu.InstallResult) {
	s := fmt.Sprintf("已把 %s 装到实例 %d", r.APK, r.Index)
	if r.Package != "" {
		s += "（包名 " + r.Package + "）"
	}
	fmt.Fprintln(w, s)
	if out := strings.TrimSpace(r.Output); out != "" {
		fmt.Fprintln(w, out)
	}
}

func formatBridge(w io.Writer, r *emu.BridgeResult) {
	state := "没装"
	if r.Installed {
		state = "已装"
	}
	fmt.Fprintf(w, "实例 %d 执行桥 %s：%s（脚本退出码 %d）\n", r.Index, r.Action, state, r.ExitCode)
	if r.Message != "" {
		fmt.Fprintln(w, r.Message)
	}
	if r.Restarted {
		fmt.Fprintln(w, "为了按可写系统开机，关机后又开了一次")
	}
	if r.Stopped {
		fmt.Fprintln(w, "实例原来是关着的，做完已经关掉")
	}
	if r.LogFile != "" {
		fmt.Fprintln(w, "脚本输出："+r.LogFile)
	}
}

func formatShell(w io.Writer, r *emu.ShellResult) {
	if r.Output == "" {
		return
	}
	io.WriteString(w, r.Output)
	if !strings.HasSuffix(r.Output, "\n") {
		io.WriteString(w, "\n")
	}
}

func upText(up bool) string {
	if up {
		return "在运行"
	}
	return "没在运行"
}

func formatServers(w io.Writer, r *emu.ServersResult) {
	fmt.Fprintf(w, "私有 adb server（端口 %d）：%s\n", r.AdbServerPort, upText(r.AdbServerUp))
	fmt.Fprintf(w, "脚本用的 adb server（端口 %d）：%s\n", r.ScriptAdbServerPort, upText(r.ScriptAdbServerUp))
}

func formatVersion(w io.Writer, r *VersionResult) {
	fmt.Fprintf(w, "modavd %s（协议 %d，%s）\n", r.Version, r.Protocol, r.Go)
}

func formatHelp(w io.Writer, r *HelpResult) {
	if !r.all {
		for _, e := range r.Commands {
			fmt.Fprintf(w, "用法：%s\n\n%s\n", e.Usage, e.Summary)
			if e.Details != "" {
				fmt.Fprintf(w, "\n%s\n", e.Details)
			}
			if len(e.Options) > 0 {
				fmt.Fprintln(w, "\n选项：")
				rows := make([][]string, 0, len(e.Options))
				for _, o := range e.Options {
					rows = append(rows, []string{"  " + o.Flag, o.Help})
				}
				writeTable(w, rows)
			}
		}
		return
	}
	fmt.Fprintln(w, "modavd：魔改 AVD 实例管理（新建、删除、修改、列出、开机、关机）和 MCP 服务")
	fmt.Fprintf(w, "\n用法：%s\n\n命令：\n", r.Usage)
	// Usage and summary on separate lines: create's usage alone is ~80 columns.
	for _, e := range r.Commands {
		fmt.Fprintf(w, "  %s\n      %s\n", strings.TrimPrefix(e.Usage, "modavd "), e.Summary)
	}
	fmt.Fprint(w, helpTail)
}

const helpTail = `
全局选项（写在命令前后都行；adb、shell 要写在命令本身前面）：
  --root <目录>  ModAVD 根目录；不给就用环境变量 MODAVD_ROOT，再不行用 modavd.exe 所在目录或它的上一级
  --json         stdout 只输出一行 JSON：{"ok":true,"data":…} 或 {"ok":false,"error":{"code","message","advice"}}；
                 日志以每行一个 JSON 对象写到 stderr
  --quiet        只输出警告和错误日志
  --verbose      输出调试日志和错误原因

<i> 写成 3 或 mas_3（0–9，5 除外）。内存写成 6144、6144M 或 6G。

退出码：0 成功，1 内部错误，2 用法或参数不对，3 实例不存在，4 根目录没就绪、检查没通过或不支持，
5 实例忙、已存在或端口被占，6 超时，7 启动失败，8 已取消（Ctrl+C）。
例外：shell 不带 --json 时是命令自己的退出码；adb 是 adb 的退出码；stop --all 有失败时是第一个失败的退出码。

运行 modavd help <命令> 查看某个命令的说明和选项。
`
