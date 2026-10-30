//go:build windows

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestDisplayWidth(t *testing.T) {
	tests := map[string]int{
		"":           0,
		"abc":        3,
		"编号":         4,
		"adb 地址":     8,
		"实例（星铁）":     12,
		"ｆｕｌｌ":       8,
		"✓ ok":       4,
		"127.0.0.1:": 10,
	}
	for s, want := range tests {
		if got := displayWidth(s); got != want {
			t.Errorf("displayWidth(%q) = %d, want %d", s, got, want)
		}
	}
}

func TestFmtMemory(t *testing.T) {
	tests := map[int]string{6144: "6 GB", 3072: "3 GB", 5000: "5000 MB", 0: "-", -1: "-"}
	for mb, want := range tests {
		if got := fmtMemory(mb); got != want {
			t.Errorf("fmtMemory(%d) = %q, want %q", mb, got, want)
		}
	}
}

func TestFormatListTable(t *testing.T) {
	a := sampleInstance(0)
	a.Title = "星铁一号"
	a.Status, a.Running, a.Booted, a.Initialized = emu.StatusOnline, true, true, true
	b := sampleInstance(3)
	b.Status, b.Busy, b.SoftwareRenderer = emu.StatusStarting, "starting", true
	b.Config.MemoryMB, b.Config.CPU, b.Config.FPS = 4096, 4, 30
	r := &emu.ListResult{
		RootInfo:  emu.RootInfo{Root: `D:\r`, Version: "0.35.0", AdbServerPort: 20050, ScriptAdbServerPort: 20049},
		Instances: []emu.InstanceInfo{a, b},
	}
	var buf bytes.Buffer
	formatList(&buf, r)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if lines[0] != `根目录：D:\r（ModAVD 0.35.0）` || lines[1] != "adb server：私有 20050，脚本用 20049" {
		t.Errorf("header = %q", lines[:2])
	}
	table := lines[2:]
	if len(table) != 3 {
		t.Fatalf("table = %q", table)
	}
	for _, col := range []string{"编号", "名称", "状态", "adb 地址", "内存", "核数", "帧率", "备注"} {
		if !strings.Contains(table[0], col) {
			t.Errorf("header lacks %s: %q", col, table[0])
		}
	}
	if !strings.Contains(table[1], "星铁一号") || !strings.Contains(table[1], "运行中") ||
		!strings.Contains(table[1], "127.0.0.1:20001") || !strings.Contains(table[1], "6 GB") {
		t.Errorf("row 0 = %q", table[1])
	}
	if !strings.Contains(table[2], "mas_3") || !strings.Contains(table[2], "开机中") ||
		!strings.Contains(table[2], "正在开机，未初始化，软件渲染") || !strings.Contains(table[2], "4 GB") {
		t.Errorf("row 1 = %q", table[2])
	}
	// Columns line up by display width: the adb address starts at the same column in every row.
	col := func(line, cell string) int { return displayWidth(line[:strings.Index(line, cell)]) }
	h, r0, r1 := col(table[0], "adb 地址"), col(table[1], "127.0.0.1"), col(table[2], "127.0.0.1")
	if h != r0 || r0 != r1 {
		t.Errorf("adb column at %d / %d / %d:\n%s", h, r0, r1, strings.Join(table, "\n"))
	}
	if strings.HasSuffix(table[1], " ") {
		t.Errorf("trailing spaces: %q", table[1])
	}

	buf.Reset()
	formatList(&buf, &emu.ListResult{RootInfo: emu.RootInfo{Root: `D:\r`, LocalSDK: true, SDK: `C:\emu-src\sdk-mas35`}})
	out := buf.String()
	if !strings.Contains(out, "ModAVD 版本未知") || !strings.Contains(out, `SDK：C:\emu-src\sdk-mas35（mas-avd.json 的 sdkRoot）`) ||
		!strings.Contains(out, "还没有实例") {
		t.Errorf("empty list = %q", out)
	}
}

func TestFormatInfo(t *testing.T) {
	in := sampleInstance(3)
	in.Title = "星铁"
	in.Status, in.PID, in.StartedAt, in.Busy = emu.StatusOnline, 1234, "2026-10-28T15:04:05+01:00", "stopping"
	in.Screencap = emu.Screencap{Shm: "SHM_videmulator20030", ShmReady: true}
	in.Renderer, in.SoftwareRenderer = "SwiftShader", true
	var buf bytes.Buffer
	formatInfo(&buf, &in)
	out := buf.String()
	for _, want := range []string{
		"编号：3\n", "名称：mas_3\n", "标题：星铁\n",
		"状态：运行中（PID 1234，启动于 2026-10-28T15:04:05+01:00，正在关机）\n",
		"adb 地址：127.0.0.1:20031\n", "序列号：emulator-20030（私有 adb server 上）\n",
		"端口：控制台 20030，adb 20031，gRPC 20032\n", "截图共享内存：SHM_videmulator20030（已就绪）\n",
		"内存：6 GB\n", "核数：6\n", "帧率：60\n", "数据盘：64 GB\n", "显示：1280x720，240 dpi\n",
		"首次初始化：还没做", "渲染器：SwiftShader 软件渲染", `路径：D:\r\avd\mas_3.avd`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("info lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "桌面") {
		t.Error("empty launcher printed")
	}
}

func TestStopMethodText(t *testing.T) {
	tests := []struct {
		r    emu.StopResult
		want string
	}{
		{emu.StopResult{Index: 3, Method: "already_offline"}, "实例 3 本来就没在运行"},
		{emu.StopResult{Index: 3, Method: "graceful", Seconds: 4.26}, "实例 3 已正常关机，用时 4.3 秒"},
		{emu.StopResult{Index: 3, Method: "sync_kill", Seconds: 20}, "实例 3 正常关机没完成，已先 sync 再从控制台结束，用时 20.0 秒"},
		{emu.StopResult{Index: 0, Method: "console_kill+terminate", Seconds: 24},
			"实例 0 私有 adb 用不了，已从控制台结束（没能先 sync），最后强行结束了 qemu 进程，用时 24.0 秒"},
		{emu.StopResult{Index: 0, Method: "console_kill", Seconds: 4},
			"实例 0 私有 adb 用不了，已从控制台结束（没能先 sync），用时 4.0 秒"},
		{emu.StopResult{Index: 2, Method: "launching_kill", Seconds: 6.5},
			"实例 2 模拟器刚拉起、安卓还没开机，已直接关掉（不用 sync），用时 6.5 秒"},
		{emu.StopResult{Index: 2, Method: "launching_kill+terminate", Seconds: 21},
			"实例 2 模拟器刚拉起、安卓还没开机，已直接关掉（不用 sync），最后强行结束了 qemu 进程，用时 21.0 秒"},
		{emu.StopResult{Index: 1, Method: "force", Seconds: 3}, "实例 1 已强制关机（先 sync 再结束），用时 3.0 秒"},
		{emu.StopResult{Index: 1, Method: "weird", Seconds: 1}, "实例 1 已关机（weird），用时 1.0 秒"},
	}
	for _, tc := range tests {
		if got := stopLine(&tc.r); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.r.Method, got, tc.want)
		}
	}
	var buf bytes.Buffer
	formatStop(&buf, &emu.StopResult{Index: 2, Method: "graceful", Warnings: []string{"sync 超时"}})
	if !strings.HasSuffix(buf.String(), "警告：sync 超时\n") {
		t.Errorf("warnings: %q", buf.String())
	}
	buf.Reset()
	formatStopAll(&buf, &StopAllResult{
		Results: []emu.StopResult{{Index: 0, Method: "graceful", Seconds: 2}},
		Failed:  []StopFailure{{Index: 2, Error: ErrorInfo{Code: errs.Busy, Message: "实例 2 正在开机", Advice: "等它开完"}}},
	})
	if buf.String() != "实例 0 已正常关机，用时 2.0 秒\n实例 2 关机失败：实例 2 正在开机\n  建议：等它开完\n" {
		t.Errorf("stop all: %q", buf.String())
	}
}

func TestFormatStart(t *testing.T) {
	r := &emu.StartResult{Instance: sampleInstance(3), Launched: true, BootSeconds: 31.24, TotalSeconds: 58.0,
		FirstBoot: true, ZygoteRestarted: true, LogFile: `D:\r\logs\mas_3-1.log`, Warnings: []string{"idlepoll 没装上"}}
	r.Instance.Title = "星铁"
	var buf bytes.Buffer
	formatStart(&buf, r)
	want := "实例 3（星铁）已开机：开机 31.2 秒，合计 58.0 秒\n" +
		"adb 地址：127.0.0.1:20031（私有 adb server 上是 emulator-20030）\n" +
		"已完成首次初始化\n" +
		"挂上了客体 Vulkan 修复，zygote 已重启（应用都重新开始）\n" +
		"模拟器日志：D:\\r\\logs\\mas_3-1.log\n" +
		"警告：idlepoll 没装上\n"
	if buf.String() != want {
		t.Errorf("start:\n%s\nwant:\n%s", buf.String(), want)
	}
	buf.Reset()
	formatStart(&buf, &emu.StartResult{Instance: sampleInstance(2), TotalSeconds: 3})
	if !strings.HasPrefix(buf.String(), "实例 2 本来就在运行，已重新做开机后的设置（3.0 秒）\n") {
		t.Errorf("adopted: %q", buf.String())
	}
	if strings.Contains(buf.String(), "还没做完") {
		t.Errorf("setup pending without SetupPending: %q", buf.String())
	}
	buf.Reset()
	formatStart(&buf, &emu.StartResult{Instance: sampleInstance(2), TotalSeconds: 3, SetupPending: true})
	if !strings.Contains(buf.String(), "\n这次开机的一次性设置（首次初始化、客体 Vulkan 修复）还没做完，再运行一次 modavd start 2 补完\n") {
		t.Errorf("setup pending: %q", buf.String())
	}
}

// TestStartHelpTellsWhatCancelDoes: the help must say what a cancel / timeout leaves behind and how to finish it.
func TestStartHelpTellsWhatCancelDoes(t *testing.T) {
	start := entryOf(findCommand("start"))
	for _, s := range []string{"Ctrl+C", "--timeout 到点", "模拟器不会被结束，会接着开机", "再运行一次 start 会接管它",
		"首次初始化和客体 Vulkan 修复"} {
		if !strings.Contains(start.Details, s) {
			t.Errorf("start details lack %q: %s", s, start.Details)
		}
	}
	if opt := start.Options[1]; opt.Flag != "--timeout 秒" || !strings.Contains(opt.Help, "模拟器不会被结束") {
		t.Errorf("timeout option = %+v", opt)
	}
	if d := entryOf(findCommand("restart")).Details; !strings.Contains(d, "等 qemu 进程退出（最多 15 秒") {
		t.Errorf("restart details: %s", d)
	}
	if d := entryOf(findCommand("stop")).Details; !strings.Contains(d, "按进程表找出") {
		t.Errorf("stop details: %s", d)
	}
}

func TestFormatCheck(t *testing.T) {
	yes, no := true, false
	r := &emu.CheckResult{
		RootInfo: emu.RootInfo{Root: `D:\r`},
		Ready:    false,
		Items: []emu.CheckItem{
			{ID: "path", Title: "路径", OK: &yes, Reason: "路径里没有特殊字符"},
			{ID: "acceleration", Title: "硬件加速", OK: &no, Blocking: true, Reason: "WHPX 没开", Advice: "打开 Windows 虚拟机平台"},
			{ID: "vulkan", Title: "Vulkan", OK: nil, Reason: "探测超时", Advice: "更新显卡驱动"},
			{ID: "disk", Title: "磁盘", OK: &no, Blocking: false, Reason: "剩 10 GB"},
		},
		Components: []emu.ComponentStatus{
			{ID: "emulator", Name: "模拟器", Version: "37.2.10", Build: "0.35.0", Installed: true},
			{ID: "launcher", Name: "轻量桌面", Installed: false, Optional: true},
		},
	}
	var buf bytes.Buffer
	formatCheck(&buf, r)
	want := "根目录：D:\\r\n" +
		"结论：有拦截项没通过，现在不能开机\n" +
		"✓ 路径：路径里没有特殊字符\n" +
		"✗ 硬件加速：WHPX 没开（拦截）\n" +
		"  建议：打开 Windows 虚拟机平台\n" +
		"? Vulkan：探测超时（现在查不了）\n" +
		"  建议：更新显卡驱动\n" +
		"✗ 磁盘：剩 10 GB（只提醒）\n" +
		"组件：\n" +
		"  ✓ 模拟器 37.2.10（ModAVD 0.35.0）\n" +
		"  ✗ 轻量桌面 未安装（可选）\n"
	if buf.String() != want {
		t.Errorf("check:\n%s\nwant:\n%s", buf.String(), want)
	}
	r.Ready = true
	buf.Reset()
	formatCheck(&buf, r)
	if !strings.Contains(buf.String(), "结论：可以开机") {
		t.Errorf("ready: %q", buf.String())
	}
}

func TestFormatUpdateAndOthers(t *testing.T) {
	var buf bytes.Buffer
	formatUpdate(&buf, &emu.UpdateResult{Instance: sampleInstance(3),
		Applied:         map[string]any{"fps": 30, "memory_mb": 4096, "title": "新名字", "cpu": 4},
		AppliesNextBoot: true})
	if buf.String() != "已修改实例 3：标题「新名字」，内存 4 GB，核数 4，帧率 30\n实例正在运行，改动下次开机生效\n" {
		t.Errorf("update: %q", buf.String())
	}
	buf.Reset()
	formatUpdate(&buf, &emu.UpdateResult{Instance: sampleInstance(3), Applied: map[string]any{"memory_mb": float64(6144)}})
	if buf.String() != "已修改实例 3：内存 6 GB\n" {
		t.Errorf("update float: %q", buf.String())
	}

	buf.Reset()
	formatWindow(&buf, &emu.WindowResult{Index: 3, Visible: true, Windows: 0})
	formatWindow(&buf, &emu.WindowResult{Index: 3, Visible: true, Windows: 2})
	formatWindow(&buf, &emu.WindowResult{Index: 3, Visible: false, Windows: 2})
	if !strings.Contains(buf.String(), "实例 3 没有窗口") || !strings.Contains(buf.String(), "已显示（2 个）") ||
		!strings.Contains(buf.String(), "已隐藏（2 个）") {
		t.Errorf("window: %q", buf.String())
	}

	buf.Reset()
	formatServers(&buf, &emu.ServersResult{AdbServerPort: 20050, AdbServerUp: true, ScriptAdbServerPort: 20049})
	if buf.String() != "私有 adb server（端口 20050）：在运行\n脚本用的 adb server（端口 20049）：没在运行\n" {
		t.Errorf("servers: %q", buf.String())
	}

	buf.Reset()
	formatShell(&buf, &emu.ShellResult{Output: "a\nb"})
	formatShell(&buf, &emu.ShellResult{Output: "c\n"})
	formatShell(&buf, &emu.ShellResult{Output: ""})
	if buf.String() != "a\nb\nc\n" {
		t.Errorf("shell: %q", buf.String())
	}

	buf.Reset()
	formatInstall(&buf, &emu.InstallResult{Index: 3, APK: `D:\a.apk`, Package: "com.miHoYo.hkrpg", Output: "Performing Streamed Install\nSuccess"})
	if buf.String() != "已把 D:\\a.apk 装到实例 3（包名 com.miHoYo.hkrpg）\nPerforming Streamed Install\nSuccess\n" {
		t.Errorf("install: %q", buf.String())
	}

	buf.Reset()
	in := sampleInstance(4)
	in.Title = "方舟"
	formatCreate(&buf, &in)
	if !strings.HasPrefix(buf.String(), "已创建实例 4（方舟）：内存 6 GB，6 核，60 帧，数据盘 64 GB\n") {
		t.Errorf("create: %q", buf.String())
	}
}

func TestErrorOutput(t *testing.T) {
	var buf bytes.Buffer
	writeHumanError(&buf, errs.New(errs.Busy, "实例 3 正在运行").WithAdvice("先关机"))
	if buf.String() != "错误：实例 3 正在运行\n建议：先关机\n" {
		t.Errorf("human: %q", buf.String())
	}
	buf.Reset()
	writeHumanError(&buf, errs.New(errs.Busy, "忙"))
	if buf.String() != "错误：忙\n" {
		t.Errorf("no advice: %q", buf.String())
	}

	buf.Reset()
	writeJSONLine(&buf, errEnvelope{Error: errorInfo(errors.New("a <b> & c"))})
	if buf.String() != `{"ok":false,"error":{"code":"internal","message":"a <b> & c"}}`+"\n" {
		t.Errorf("json: %q", buf.String())
	}
	buf.Reset()
	writeJSONLine(&buf, okEnvelope{OK: true, Data: map[string]string{"msg": "第一行\n第二行"}})
	if strings.Count(buf.String(), "\n") != 1 {
		t.Errorf("multi-line value broke the line: %q", buf.String())
	}
	// A value JSON cannot encode still yields one valid line.
	buf.Reset()
	if writeJSONLine(&buf, okEnvelope{OK: true, Data: map[string]any{"bad": make(chan int)}}) {
		t.Error("encoding a channel reported success")
	}
	if !strings.HasPrefix(buf.String(), `{"ok":false,"error":{"code":"internal"`) {
		t.Errorf("fallback: %q", buf.String())
	}
}
