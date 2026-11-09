package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/components"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/mcp"
	"modavd/internal/report"
	"modavd/internal/root"
	"modavd/internal/update"
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
	optIndex         = flagSpec{"index", kindString, "N", "实例编号（不给就取最小的空闲编号，端口也要空着）"}
	optTitle         = flagSpec{"title", kindString, "T", "显示名称"}
	optMemory        = flagSpec{"memory", kindString, "M", "内存：3072 / 4096 / 5120 / 6144 MB，也可写 6G"}
	optCPU           = flagSpec{"cpu", kindInt, "N", "核数：2 / 4 / 6"}
	optDataGB        = flagSpec{"data-gb", kindInt, "N", "数据盘大小（GB）：16–512"}
	optResolution    = flagSpec{"resolution", kindInt, "", "分辨率：1080 / 720，DPI 随档位切换；新建默认 1080"}
	optFPS           = flagSpec{"fps", kindInt, "F", "帧率：30 / 60"}
	optDiagnostics   = flagSpec{"diagnostics", kindString, "on|off", "诊断模式：即时增加日志保留；off 立即删回平常规则"}
	optYes           = flagSpec{"yes", kindBool, "", "确认删除（连同游戏数据）"}
	optWindow        = flagSpec{"window", kindBool, "", "带窗口开机（默认无头）；实例已经无头运行时报错，不接管"}
	optHeadless      = flagSpec{"headless", kindBool, "", "无头开机（不带 --window 时本来就是无头）；实例已经带窗口运行时报错，不接管"}
	optTimeout       = flagSpec{"timeout", kindInt, "秒", "最多等 Android 开机多少秒（默认 300；到点只是不再等，模拟器不会被结束）"}
	optCreateTimeout = flagSpec{"timeout", kindInt, "秒", "新建后的初始化最多做多少秒（默认 1200；到点先正常关机，记为新建失败）"}
	optResume        = flagSpec{"resume", kindInt, "N", "重做编号 N 的新建初始化（上次失败或被打断的，它开的模拟器还开着就先正常关机；其他选项不用）"}
	optForce         = flagSpec{"force", kindBool, "", "跳过 Android 正常关机：先 sync，再直接结束"}
	optCancelCreate  = flagSpec{"cancel-create", kindBool, "", "允许打断新建中的初始化，之后用 create --resume N 重做"}
	optAll           = flagSpec{"all", kindBool, "", "同时关掉所有在运行的实例"}
	optShellTO       = flagSpec{"timeout", kindInt, "秒", "命令最多执行多少秒（不给用默认值）"}
	optCheckMB       = flagSpec{"memory", kindString, "M", "按这个实例内存判断电脑内存够不够（默认 6G）"}
	optNoBridge      = flagSpec{"no-bridge", kindBool, "", "不装 arm64 执行桥，以后开机也不装（默认 modavd / 管理器开机时自动装上）"}
	optAutoMAS       = flagSpec{"automas-version", kindString, "V", "调用方 AUTO-MAS 的版本（新版要求更高的 AUTO-MAS 时不装）"}
	optBootNoBridge  = flagSpec{"no-bridge", kindBool, "",
		"这次开机不自动装执行桥（实例有执行桥的开机标记、桥还没装时默认会装）"}
	optAcceptLicense = flagSpec{"accept-license", kindBool, "",
		"不询问，直接视为同意《Android 软件开发套件许可协议》（自动化用；协议全文照样取回并打印，取不到照样退出）；--json 时必须给"}
	optKeepDownloads = flagSpec{"keep-downloads", kindBool, "", "装好后保留 downloads 里的 zip（默认装好即删，省 1.5 GB）"}
	optFetchStore    = flagSpec{"play-store", kindBool, "",
		playStoreFetchHelp()}
	optMirror    = flagSpec{"mirror", kindString, "ID", "只用一个内置镜像：googledownloads_cn / tencent / google"}
	optMirrorURL = flagSpec{"mirror-url", kindStrings, "URL",
		"自建或测试用：用这个目录地址代替内置的三个镜像，可写多次或用逗号隔开，按顺序试；给了就不看 --mirror"}
	optConnectTO = flagSpec{"connect-timeout", kindInt, "秒",
		"等服务器回应（连接、TLS 握手、返回响应头）的上限，1–600，默认 20，超时换下一个镜像"}
	optStallTO = flagSpec{"stall-timeout", kindInt, "秒",
		"下载或取清单时，连续这么久一个字节都没收到就算卡住，1–600，默认 30，超时换下一个镜像"}
	optManifestTO    = flagSpec{"manifest-timeout", kindInt, "秒", "取许可协议清单（repository2-3.xml）整体的上限，1–1800，默认 40"}
	optTestFreeSpace = flagSpec{"test-free-space-mb", kindInt, "MB", "仅测试用：假装磁盘只剩这么多 MB，用来演示「空间不够」的提示"}
	optNoClipboard   = flagSpec{"no-clipboard", kindBool, "", "不复制到剪贴板"}
	optOutDir        = flagSpec{"out-dir", kindString, "目录", "问题包输出目录（默认桌面）"}
	optStageDir      = flagSpec{"stage-dir", kindString, "目录", "暂存目录的父目录（默认 %TEMP%）"}
	optKeepStage     = flagSpec{"keep-stage", kindBool, "", "保留暂存目录"}
	optNoExplorer    = flagSpec{"no-explorer", kindBool, "", "不打开资源管理器"}
)

// fetchFlags are the options only components fetch takes.
var fetchFlags = []flagSpec{optAcceptLicense, optKeepDownloads, optFetchStore, optMirror, optMirrorURL, optConnectTO,
	optStallTO, optManifestTO, optTestFreeSpace}

var commands []*command

// The table is built in init: help's run function refers to it.
func init() {
	componentDetails := "status（默认）查看组件，和 check 里的组件一样。fetch 补全缺少的组件：" +
		"先从镜像取《Android 软件开发套件许可协议》全文并显示，输入 y 同意才继续，否则什么也不下；" +
		"再依次试 googledownloads.cn、腾讯云镜像、dl.google.com，下载缺少的 adb 和系统镜像（约 1.5 GB，中断了再运行会接着下），" +
		"校验后装进 sdk\\。已经齐全的跳过；不完整的旧目录改名为 <目录>.incomplete-<时间> 留着，不删。" +
		"--play-store 再补全可选的 Google Play 商店（playstore <i> install 要用；已经有了就跳过）：下 Google Play 版系统镜像，" +
		"从里面的 system.img 只取出商店、Play 服务、GSF、合作伙伴设置和商店的证书这五个文件，核对 SHA-256 后放进 sdk\\playstore，" +
		"解出的镜像和下载的压缩包随即删掉，只留这五个文件。" +
		"import-playstore 用自己的文件补全它，不下载、不问许可协议：给 Google 官方的 Google Play 版镜像压缩包（x86_64-34_r14.zip，名字不限，" +
		"按 Google 仓库清单核对大小和 SHA-1），或者那五个文件本身（名字不限，逐个核对大小和 SHA-256；给文件夹就取里面同名的几个）；" +
		"认不出或对不上的一律拒收，什么也不改；只读你的文件，不动它们。" +
		"status 列出这五个文件在不在、从哪来（download / import），不把它算进必需组件。" +
		"不带参数时默认查看组件；source 查看或修改记住的下载源（auto、googledownloads_cn、tencent、google）。" +
		"--json 时必须给 --accept-license（仅 fetch）。只有 Windows 有组件清单，darwin、linux 返回 unsupported。" +
		"退出码：没同意许可协议是 2（license_declined），路径有中文、空间不够、不是魔改 AVD 的文件夹是 4，" +
		"正在补全是 5，下不成是 9（components_unavailable），下载或解压后没通过校验、导入的文件被拒收也是 9（components_rejected，" +
		"--json 的 error.details.reason 是 unknown_file、mismatch 或 incomplete）。"
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		componentDetails = "status（默认）查看组件，和 check 里的组件一样。fetch 补全缺少的 adb 和 Android 15 Google APIs ARM64 系统镜像：" +
			"先从镜像取待下载组件的 Google 许可协议全文，逐份输入 y 同意才继续，否则不下载归档。" +
			"默认依次试 googledownloads.cn、腾讯云镜像、dl.google.com，下载约 1.8 GB，中断后再次运行会接着下；" +
			"校验归档、版本和平台后装进 sdk/，保留执行权限。已经齐全的跳过；不完整的旧目录改名为 <目录>.incomplete-<时间> 留着，不删。" +
			"SDK 使用中会拒绝补全，请先自行关闭使用它的进程。" +
			"--play-store 可选下载 Android 15 Google Play arm64-v8a r9 镜像（约 1.8 GB），校验后只保留商店等五个文件。" +
			"import-playstore 可导入官方 arm64-v8a-35_r09.zip 或五个文件，按固定大小和摘要检查，不下载。" +
			"文件就绪后用 playstore <实例编号> install 装进 API35 ARM64 实例；macOS 使用实例自己的可写系统覆盖层，不需要执行桥。" +
			"source 查看或修改记住的下载源（auto、googledownloads_cn、tencent、google）；--mirror 或 --mirror-url 可覆盖本次选择。" +
			"--json 时必须给 --accept-license（仅 fetch），表示已同意待下载组件的全部许可协议。" +
			"退出码：未同意许可是 2（license_declined），未就绪或预检失败是 4，SDK 使用中或正在补全是 5，下载不可用或校验失败是 9。"
	}
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
			details: "status：offline 已关机 / starting 开机中 / online 运行中 / creating 新建中 / create_failed 新建失败；" +
				"running 是 qemu 进程在不在，booted 是 Android 开完没有（新建中这两个可能是 true，以 status 为准）；" +
				"busy 是另一个 modavd 正在开机（starting）或关机（stopping）它；新建中 creation_progress 是做完的百分比（0–100，按做完的步骤算）。",
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			run: runStatus, human: humanOf(formatStatus),
		},
		{
			name: "create", args: "[--index N] [--title T] [--memory 6G] [--cpu N] [--data-gb N] [--fps 30|60] [--resolution 1080|720] [--no-bridge] [--timeout 1200] | --resume N",
			summary: "新建一台实例，后台开机做完初始化后关机",
			details: "不给的选项用默认值：内存 6 GB、6 核、数据盘 64 GB、60 帧、1080p（1920×1080 / DPI 280）。内存和核数只能取列出的档位，不会自动凑整。" +
				"建好文件后在后台无头开机一次，做完开机设置、宿主输入法、首次初始化和 arm64 执行桥（只对内置名单里的游戏生效，" +
				"其余应用照旧；--no-bridge 不装，以后也不装），再先 sync 后正常关机，一般要几分钟，进度逐步打印（「新建中 35%」，按做完的步骤算）；" +
				"之后第一次开机和平常一样快。新建的实例不带 Google Play 商店，要的话建好后用 playstore <i> install 补全。" +
				"这期间实例的状态是 creating（status、list 里 creation_progress 是百分比），不能开机、修改或删除；失败（或被打断）是 create_failed，" +
				"用 create --resume N 重做初始化，或者 delete N --yes 删掉。",
			flags: []flagSpec{optIndex, optTitle, optMemory, optCPU, optDataGB, optFPS, optResolution, optNoBridge, optCreateTimeout, optResume},
			final: true,
			run:   runCreate, human: humanOf(formatCreate),
		},
		{
			name: "set", args: "<i> [--title T] [--memory M] [--cpu N] [--fps F] [--resolution 1080|720] [--data-gb N] [--diagnostics on|off]",
			summary: "修改实例设置和诊断模式",
			details: "至少给一个选项。诊断模式立即生效；其它改动下次开机生效。诊断模式最多多用约 4 GiB 磁盘和 48 MiB 客体内存，日志可能含账号信息，只存本机；关闭立即删回平常保留规则。" +
				"--data-gb 只能改大（16–512），游戏数据不动：下次开机时模拟器（0.36 起）扩大磁盘镜像，开机后再在线扩大文件系统。",
			flags:    []flagSpec{optTitle, optMemory, optCPU, optFPS, optResolution, optDataGB, optDiagnostics},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			final: true,
			run:   runSet, human: humanOf(formatUpdate),
		},
		proxyCommand(),
		{
			name: "delete", args: "<i> --yes", summary: "删除一台已关机的实例（连同游戏数据）",
			details:  "删掉 avd\\mas_<i>.avd（含数据盘）、mas_<i>.ini 和 mas-avd.json 里的记录。实例在运行或正在开关机时拒绝。",
			flags:    []flagSpec{optYes},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			final: true,
			run:   runDelete, human: humanOf(formatDelete),
		},
		{
			name: "start", args: "<i> [--window | --headless] [--timeout 300] [--no-bridge]", summary: "开机，等 Android 开完并做好开机后的设置",
			details: "默认无头启动，--window 带窗口。运行中不能换：实例已经在运行（或正被另一次 start 开着）时，" +
				"带了 --window 或 --headless 而它不是这种方式，就报错（busy），不接管也不重启，要换先 stop 再 start；" +
				"两个都不带就照它现在的方式接管。第一次开机还会做首次初始化。" +
				"按 Ctrl+C 或 --timeout 到点都只是不再等：模拟器不会被结束，会接着开机。" +
				"之后再运行一次 start 会接管它，把这次开机还没做完的设置补完（包括首次初始化和客体 Vulkan 修复）。" +
				"实例已经在运行、设置也都做完时，只重做每次开机都做的设置。" +
				"返回前确认 /sdcard 能写进文件（一般这时早已就绪，不多花时间；最多再等 60 秒，到点只警告）。" +
				"实例还没有 arm64 执行桥时，设置做完后自动装上（中间关机再开机一两次）；--no-bridge 这次不装，" +
				"用 bridge uninstall 卸过或新建时带 --no-bridge 的实例不装，接管已经在运行的实例时也不装。",
			flags:    []flagSpec{optWindow, optHeadless, optTimeout, optBootNoBridge},
			posNames: []string{"实例编号"}, minPos: 1, maxPos: 1,
			final: true,
			run:   runStart, human: humanOf(formatStart),
		},
		{
			name: "stop", args: "<i> [--force] [--cancel-create] | stop --all [--force]", summary: "关机",
			details: "先让 Android 正常关机，不成再 sync 后结束 qemu；--force 跳过正常关机，先 sync 再结束。" +
				"新建中的实例默认报 busy，--force 也不例外；确实要打断请加 --cancel-create，之后用 create --resume N 重做。" +
				"--all 同时关掉所有运行中或新建中的实例（包括两次开机之间），被打断的结果标记 interrupted_create: true；" +
				"结果分 results 和 failed，有失败时退出码是第一个失败的退出码。",
			flags:    []flagSpec{optForce, optAll, optCancelCreate},
			posNames: []string{"实例编号"}, minPos: 0, maxPos: 1,
			final: true,
			run:   runStop, human: humanStop, exit: exitStop,
		},
		{
			name: "check", args: "[--memory M]", summary: "开机前的电脑检查",
			details: "路径、组件、模拟器版本、硬件加速、Vulkan、磁盘、内存。没通过也返回 0：看 ready（--json）或结论那一行。",
			flags:   []flagSpec{optCheckMB},
			run:     runCheck, human: humanOf(formatCheck),
		},
		{
			name: "report-config", args: "[--no-clipboard]", summary: "复制电脑配置和实例里装的游戏",
			details: "电脑、模拟器、硬件加速和开着的实例里装的游戏，显示出来并复制到剪贴板（--no-clipboard 不复制）。" +
				"只读：只在实例开着、它的 adb server 已经在监听时按 adb 主机协议去问，不起 adb server、不调用 adb.exe、不碰 5037；" +
				"不带用户名、电脑名和完整路径。目前只支持 Windows。",
			flags: []flagSpec{optNoClipboard}, final: true,
			run: runReportConfig, human: humanReportConfig,
		},
		{
			name: "collect-info", args: "[--out-dir 目录] [--stage-dir 目录] [--keep-stage] [--no-explorer]",
			summary: "收集问题信息，打成 zip",
			details: "打成「魔改AVD问题信息-<时间>.zip」，默认放桌面，再打开资源管理器选中它（--no-explorer 不打开）。" +
				"--stage-dir 是暂存目录的父目录（默认 %TEMP%），--keep-stage 保留暂存目录。每一节单独收，出错写进 summary.txt 接着收下一节。" +
				"只读：不收磁盘镜像，不收 AUTO-MAS 的配置和账号数据，不起 adb server、不碰 5037。目前只支持 Windows。",
			flags: []flagSpec{optOutDir, optStageDir, optKeepStage, optNoExplorer}, final: true,
			run: runCollectInfo, human: humanCollectInfo,
		},
		{
			name: "components", args: "[status] | source [auto|googledownloads_cn|tencent|google] | fetch [--accept-license] [--mirror ID] [--mirror-url URL] [--keep-downloads] [--play-store] | import-playstore <文件或文件夹>…",
			summary:  "查看或补全 Google 组件（adb 和系统镜像，可选的 Google Play 商店）",
			details:  componentDetails,
			flags:    fetchFlags,
			posNames: []string{"status、source、fetch 或 import-playstore", "下载源或要导入的文件"}, maxPos: -1,
			final: true,
			run:   runComponents, human: formatComponents,
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
				"实例原来关着的，做完再关掉；开着的，中间冷启动后照原来的方式开回来（带窗口或无头）。" +
				"verify：只读核对，实例要开着。",
			posNames: []string{"实例编号", "install、uninstall 或 verify"}, minPos: 2, maxPos: 2,
			final: true,
			run:   runBridge, human: humanOf(formatBridge), exit: exitBridge,
		},
		{
			name: "playstore", args: "<i> install|uninstall|status|gsfid", summary: "装、卸、查看 Google Play 商店，取 GSF ID",
			details: "商店的五个文件取自 Google 官方的 Google Play 版系统镜像（和实例的镜像同一期），要先在根目录补全：" +
				"components fetch --play-store（下载）或 components import-playstore（用自己的文件）；没有就报 not_ready（details.reason 为 playstore_files_missing）。" +
				playStoreOverlayHelp() + "install 放入商店和 Google 签名的 Play 服务、GSF、合作伙伴设置，" +
				"藏起镜像自带的同名占位应用，再冷启动生效；uninstall 去掉 Google Play 装的更新、删掉这些文件，冷启动后恢复占位应用。期间可能冷启动多次。" +
				"实例原来关着的，做完再关掉（没做成也关掉）；开着的，冷启动后照原来的方式开回来（带窗口或无头）。" +
				"install 中途失败时撤回这次放进去的文件；撤不干净的记为只装了一部分（list 的 play_store_partial），用 uninstall 移除或再 install 补全。" +
				"status：只读，实例要开着。gsfid：取这台实例的 GSF ID（十进制和十六进制，--json 是 decimal、hex），" +
				"设备未经 Play 保护机制认证时到 " + emu.PlayStoreUncertifiedURL + " 登记；实例要开着，还没有 ID 时报 not_ready（details.reason 为 no_gsf_id），开着网等一两分钟再试。" +
				playStoreBridgeHelp() + "新建的实例都不带商店。",
			posNames: []string{"实例编号", "install、uninstall、status 或 gsfid"}, minPos: 2, maxPos: 2,
			final: true,
			run:   runPlayStore, human: humanPlayStore, exit: exitPlayStore,
		},
		{
			name: "disk", args: "<i> trim|info|compact", summary: "磁盘回收：触发回收或整理把客体已删文件占的空间还给宿主；看数据盘大小",
			details: "trim：在客体的数据盘（/data）上触发一次回收（FITRIM，经 vold 空闲维护），" +
				"让模拟器释放已删除文件在数据盘镜像里占着的空间。实例要开着并已开机完成；" +
				"命令成功只表示已触发，不等待客体回收完成，也不确认回收成功。" +
				"模拟器版本太旧、数据盘不支持回收时返回不支持（退出码 4），不会白跑。" +
				"info：报数据盘的逻辑大小、在宿主上实际占的空间，以及（实例关机时）已分配的大小和估计能回收多少；" +
				"实例开着时已分配和可回收的数要关机后才有，只读，不改任何东西。" +
				"compact：关机状态下把数据盘覆盖层重写一遍、丢掉已释放的簇、把文件缩小；" +
				"转换后先校验（check + compare）再原子替换，原盘在替换成功前不动；实例有快照时拒绝。",
			posNames: []string{"实例编号", "trim、info 或 compact"}, minPos: 2, maxPos: 2,
			final: true,
			run:   runDisk, human: formatDiskHuman,
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
			name: "update", args: "[status|check|prepare|install|recover] [--automas-version V] [--package ZIP --sha256 HEX]",
			summary: "更新这个根目录的 ModAVD 包（模拟器、组件、脚本和管理器）",
			details: "按管理器「设置」里选的更新源（GitHub 或 Mirror 酱，Mirror 酱下载要填 CDK）。不自动联网，只做给出的那一步：" +
				"status（默认）只看本地；check 查最新版本；prepare 下载、按清单核对、暂存好，实例开着也行；" +
				"install 需要时先 prepare，再换上 sdk\\emulator 和包里的其他文件，要先关掉所有实例、管理器窗口和 MCP 客户端；" +
				"recover 补完或退回上次没做完的换上。和 AUTO-MAS 用同一个 .update 目录，能接着对方没做完的做。" +
				"--automas-version 是调用方 AUTO-MAS 的版本，新版要求更高的 AUTO-MAS 时不装。" +
				"macOS 可用 --package ZIP --sha256 HEX 准备或安装本地完整签名包；摘要需从可信来源独立取得。" +
				"查不了是退出码 9（update_unavailable），下载没通过核对也是 9（update_rejected），有程序在用是 5。",
			posNames: []string{"status、check、prepare、install 或 recover"}, maxPos: 1,
			flags: []flagSpec{optAutoMAS,
				{name: "package", kind: kindString, arg: "ZIP", help: "macOS 本地完整更新包"},
				{name: "sha256", kind: kindString, arg: "HEX", help: "本地更新包的独立可信 SHA-256"}}, final: true,
			run: runUpdate, human: humanOf(formatUpdateStatus),
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

// removedCommands are commands that went away, with what to run instead: an old caller is told that, not 未知命令.
var removedCommands = map[string]string{
	"restart": "0.3.0 起没有重启：先 modavd stop <i>，再 modavd start <i>（要窗口加 --window）",
}

// removedError is the error of removed command name, nil when name was never removed.
func removedError(name string) error {
	instead, ok := removedCommands[name]
	if !ok {
		return nil
	}
	return errs.Newf(errs.Usage, "modavd %s 已经去掉了", name).WithAdvice(instead)
}

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
	Index         int        `json:"index"`
	Name          string     `json:"name"`
	Status        emu.Status `json:"status"`
	Running       bool       `json:"running"`
	Booted        bool       `json:"booted"`
	Window        bool       `json:"window"` // emu.InstanceInfo.Window
	Busy          string     `json:"busy"`
	PID           int        `json:"pid"`
	Creation      string     `json:"creation"`
	CreationError string     `json:"creation_error"`
	CreationLog   string     `json:"creation_log"`
	// CreationProgress: 0–100 while status is creating (emu.InstanceInfo.CreationProgress)
	CreationProgress int `json:"creation_progress"`
	// Where to reach the instance, in its root's port plan (mas-avd.json portBase): take these rather than counting
	// from 20000 (ModAVD/ModAVD#75).
	Serial     string    `json:"serial"`
	AdbAddress string    `json:"adb_address"`
	Ports      emu.Ports `json:"ports"`
}

// ErrorInfo is an error in JSON: the envelope's "error" and each stop --all failure.
type ErrorInfo struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Advice  string            `json:"advice,omitempty"`
	Details map[string]string `json:"details,omitempty"` // errs.DetailsOf: boot_diag_dir ...
}

func errorInfo(err error) ErrorInfo {
	e := errs.As(err)
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	return ErrorInfo{Code: e.Code, Message: msg, Advice: e.Advice, Details: errs.DetailsOf(e)}
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

// ComponentsStatusResult is `components status`: check's component list (emu.ComponentStatus, every field present).
type ComponentsStatusResult struct {
	Root       string                `json:"root"`
	Components []emu.ComponentStatus `json:"components"`
	// PlayStore is the optional Google Play Store's five files (not one of Components: never counted as missing).
	PlayStore components.PlayStoreStatus `json:"play_store"`
}

// ComponentsSourceResult is `components source`: the saved per-user choice.
// Source is auto, one of the built-in mirror IDs, or custom when URLs are set.
type ComponentsSourceResult struct {
	Source     string   `json:"source"`
	Mirror     string   `json:"mirror"`
	MirrorURLs []string `json:"mirror_urls"`
	Configured bool     `json:"configured"`
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
	req.Resolution = intPtrOpt(inv, "resolution")
	req.NoBridge = inv.boolOpt("no-bridge")
	if inv.isSet("timeout") {
		n := inv.intOpt("timeout")
		if n <= 0 {
			return req, errs.New(errs.InvalidArgument, "timeout 必须是正整数")
		}
		req.Timeout = time.Duration(n) * time.Second
	}
	if inv.isSet("resume") {
		for _, o := range []string{"index", "title", "memory", "cpu", "data-gb", "fps", "resolution", "no-bridge"} {
			if inv.isSet(o) {
				return req, errs.Newf(errs.Usage, "--resume 只重做初始化，不能再给 --%s", o)
			}
		}
		i := inv.intOpt("resume")
		if !root.ValidIndex(i) {
			return req, errs.Newf(errs.InvalidArgument, "实例编号 %d 不可用（只能是 0–9，5 除外）", i)
		}
		req.Index = &i
		req.Resume = true
	}
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
	req.Resolution = intPtrOpt(inv, "resolution")
	req.DataPartitionGB = intPtrOpt(inv, "data-gb")
	if inv.isSet("diagnostics") {
		value := strings.ToLower(inv.strOpt("diagnostics"))
		if value != "on" && value != "off" {
			return req, errs.New(errs.InvalidArgument, "--diagnostics 只能是 on 或 off")
		}
		enabled := value == "on"
		req.Diagnostics = &enabled
	}
	if req.Title == nil && req.MemoryMB == nil && req.CPU == nil && req.FPS == nil && req.Resolution == nil && req.DataPartitionGB == nil && req.Diagnostics == nil {
		return req, errs.New(errs.Usage, "没有要改的内容：至少给 --title、--memory、--cpu、--fps、--resolution、--data-gb、--diagnostics 中的一个").
			WithAdvice(findCommand("set").usageAdvice())
	}
	return req, nil
}

func buildStartOptions(inv *invocation) (emu.StartOptions, error) {
	opt := emu.StartOptions{Window: inv.boolOpt("window"), Headless: inv.boolOpt("headless"), NoBridge: inv.boolOpt("no-bridge")}
	if opt.Window && opt.Headless {
		return opt, errs.New(errs.Usage, "--window 和 --headless 只能给一个").WithAdvice(findCommand("start").usageAdvice())
	}
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
	return &StatusResult{Index: in.Index, Name: in.Name, Status: in.Status, Running: in.Running, Booted: in.Booted, Window: in.Window,
		Busy: in.Busy, PID: in.PID, Creation: in.Creation, CreationError: in.CreationError, CreationLog: in.CreationLog,
		CreationProgress: in.CreationProgress, Serial: in.Serial, AdbAddress: in.AdbAddress, Ports: in.Ports}
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
	req.Progress = func(p emu.Progress) { c.log.Infof("%s", createProgressLine(p)) }
	in, err := m.Create(c.ctx, req)
	if err != nil {
		return nil, err
	}
	return in, nil
}

// createProgressLine is one progress line of create: "新建中 35%：…" until the setup is done.
func createProgressLine(p emu.Progress) string {
	if p.Step == "done" {
		return p.Message
	}
	return fmt.Sprintf("新建中 %d%%：%s", p.Percent(), p.Message)
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
	opt := emu.StopOptions{Force: c.inv.boolOpt("force"), CancelCreate: c.inv.boolOpt("cancel-create")}
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

// stopAll stops every running or actively creating instance concurrently: the
// caller (AUTO-MAS before a power action) gives it about 30 s, the budget one stop has.
//
// The targets come from the process table and creation metadata / locks, without adb. List would
// first start the private server and run a getprop with re-registration on each instance, up to 5 s for a stuck
// adbd, before the first stop begins; and a server it started would make each Stop trust it and skip its own "start
// the server and register" step, so an instance not registered yet would be killed without a sync.
func stopAll(c *cmdCtx, m manager, opt emu.StopOptions) (*StopAllResult, error) {
	// A power action must also stop creations, with the same sync / graceful shutdown as other instances.
	opt.CancelCreate = true
	targets, err := m.StopTargets(c.ctx)
	if err != nil {
		return nil, err
	}
	targets = slices.Clone(targets)
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

func runReportConfig(c *cmdCtx) (any, error) {
	r, err := c.deps.resolveRoot(c.inv.root)
	if err != nil {
		return nil, err
	}
	return report.BuildConfig(c.ctx, r, report.ConfigOptions{NoClipboard: c.inv.boolOpt("no-clipboard")})
}

// humanReportConfig prints what report-config.ps1 prints: a blank line, the lines, a blank line, then whether they
// were copied.
func humanReportConfig(w io.Writer, data any) {
	r, ok := data.(*report.ConfigResult)
	if !ok || r == nil {
		return
	}
	fmt.Fprintln(w)
	for _, l := range r.Lines {
		fmt.Fprintln(w, l)
	}
	fmt.Fprintln(w)
	if r.Message != "" {
		fmt.Fprintln(w, r.Message)
	}
}

func runCollectInfo(c *cmdCtx) (any, error) {
	r, err := c.deps.resolveRoot(c.inv.root)
	if err != nil {
		return nil, err
	}
	return report.Collect(c.ctx, r, report.CollectOptions{
		OutDir: c.inv.strOpt("out-dir"), StageDir: c.inv.strOpt("stage-dir"),
		KeepStage: c.inv.boolOpt("keep-stage"), NoExplorer: c.inv.boolOpt("no-explorer"),
		Progress: func(msg string) { c.log.Infof("%s", msg) },
	})
}

// humanCollectInfo prints the end of collect-info.ps1's output (its progress lines went to the log).
func humanCollectInfo(w io.Writer, data any) {
	r, ok := data.(*report.CollectResult)
	if !ok || r == nil {
		return
	}
	if r.StageError != "" {
		fmt.Fprintln(w, "暂存目录没删掉："+r.Stage)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "收集完成。")
	fmt.Fprintln(w, "文件："+r.ZipPath)
	fmt.Fprintln(w, "大小："+r.ZipSize)
	for _, l := range strings.Split(r.PrivacyNote, "\r\n") {
		fmt.Fprintln(w, l)
	}
	if !r.MasFound {
		fmt.Fprintln(w, "没找到 AUTO-MAS 安装目录，反馈时请附上 AUTO-MAS 的 debug 文件夹里的日志")
	}
	if len(r.Problems) > 0 {
		fmt.Fprintf(w, "有 %d 节没收集成功，原因写在 zip 里的 summary.txt，照样提交就行。\n", len(r.Problems))
	}
	if r.StageKept && r.StageError == "" {
		fmt.Fprintln(w, "暂存目录保留在："+r.Stage)
	}
}

func runComponents(c *cmdCtx) (any, error) {
	action := "status"
	if len(c.inv.pos) > 0 {
		action = strings.ToLower(c.inv.pos[0])
	}
	if action != "fetch" && action != "status" && action != "source" && action != "import-playstore" {
		return nil, errs.Newf(errs.Usage, "只能是 status、source、fetch 或 import-playstore，收到「%s」", c.inv.pos[0]).WithAdvice(findCommand("components").usageAdvice())
	}
	if action == "import-playstore" {
		return runImportPlayStore(c)
	}
	if action == "source" {
		for _, f := range fetchFlags {
			if c.inv.isSet(f.name) {
				return nil, errs.Newf(errs.Usage, "%s 只用于 components fetch", strings.Join([]string{"--" + f.name}, "、")).
					WithAdvice(findCommand("components").usageAdvice())
			}
		}
		if len(c.inv.pos) > 2 {
			return nil, errs.New(errs.Usage, "components source 最多带一个下载源").WithAdvice(findCommand("components").usageAdvice())
		}
		if len(c.inv.pos) == 1 {
			return componentsSourceResult(components.LoadSourceSettings()), nil
		}
		choice := strings.ToLower(strings.TrimSpace(c.inv.pos[1]))
		if choice == "auto" {
			choice = ""
		}
		if choice != "" {
			if _, err := components.ChooseMirrors(choice, nil); err != nil {
				return nil, err
			}
		}
		s, err := components.SaveSourceSettings(choice, nil)
		if err != nil {
			return nil, err
		}
		return componentsSourceResult(s), nil
	}
	if len(c.inv.pos) > 1 {
		return nil, errs.Newf(errs.Usage, "components %s 只接受一个参数", action).WithAdvice(findCommand("components").usageAdvice())
	}
	if action == "status" {
		var given []string
		for _, f := range fetchFlags {
			if c.inv.isSet(f.name) {
				given = append(given, "--"+f.name)
			}
		}
		if len(given) > 0 {
			return nil, errs.Newf(errs.Usage, "%s 只用于 components fetch", strings.Join(given, "、")).
				WithAdvice(findCommand("components").usageAdvice())
		}
		r, err := c.deps.resolveRoot(c.inv.root)
		if err != nil {
			return nil, err
		}
		return &ComponentsStatusResult{Root: r.Dir, Components: emu.ComponentStatuses(r.Components()),
			PlayStore: components.PlayStoreState(r)}, nil
	}
	var (
		r   *root.Root
		err error
	)
	// Legacy releases kept the choice in <root>\downloads\source.txt. Read it
	// once before the first interactive fetch, then use the per-user setting.
	if !c.inv.isSet("mirror") && !c.inv.isSet("mirror-url") && !c.inv.json && !c.inv.boolOpt("accept-license") && c.deps.stdinTerminal(c.stdin) {
		r, err = c.deps.resolveRoot(c.inv.root)
		if err != nil {
			return nil, err
		}
		if _, _, err := components.ImportLegacySource(r.Dir); err != nil {
			return nil, err
		}
	}
	opt, err := fetchOptions(c)
	if err != nil {
		return nil, err
	}
	if r == nil {
		r, err = c.deps.resolveRoot(c.inv.root)
		if err != nil {
			return nil, err
		}
	}
	fetch := c.deps.fetchComponents
	if fetch == nil {
		fetch = components.Fetch
	}
	result, err := fetch(c.ctx, r, opt)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func componentsSourceResult(s components.SourceSettings) *ComponentsSourceResult {
	source := "auto"
	if len(s.MirrorURLs) > 0 {
		source = "custom"
	} else if strings.TrimSpace(s.Mirror) != "" {
		source = strings.ToLower(strings.TrimSpace(s.Mirror))
	}
	urls := append([]string{}, s.MirrorURLs...)
	return &ComponentsSourceResult{Source: source, Mirror: s.Mirror, MirrorURLs: urls, Configured: s.Set}
}

// fetchOptions checks every option of components fetch (before the root is looked up) and wires the output: the
// script's messages and progress lines go to the log, progress at most every 2 s like update; the license text and
// the question are written whatever the log level (raw on stderr, or one log line each with --json).
func fetchOptions(c *cmdCtx) (components.Options, error) {
	inv := c.inv
	input := c.stdin
	if input == nil {
		input = os.Stdin
	}
	o := components.Options{
		AcceptLicense: inv.boolOpt("accept-license"), JSON: inv.json, Input: input,
		KeepDownloads: inv.boolOpt("keep-downloads"), Mirror: inv.strOpt("mirror"), MirrorURLs: inv.strsOpt("mirror-url"),
		PlayStore: inv.boolOpt("play-store"),
		Log:       c.log,
	}
	// --mirror / --mirror-url win. Otherwise the saved per-user source; with none saved, someone at a console is
	// asked once (and the answer saved), while --accept-license (automation), --json and a stdin that is not a
	// console (pipe, file, NUL) are never asked: they use the default order and save nothing.
	if !inv.isSet("mirror") && !inv.isSet("mirror-url") {
		saved := components.LoadSourceSettings()
		if !saved.Set && !o.AcceptLicense && !o.JSON && c.deps.stdinTerminal(input) {
			buffered := bufio.NewReader(input)
			choice, err := askComponentSource(c, buffered)
			if err != nil {
				return o, err
			}
			// Keep the reader that may already have buffered the answer to the
			// subsequent license question.
			o.Input = buffered
			saved = choice
		}
		if saved.Set {
			o.Mirror, o.MirrorURLs = saved.Mirror, append([]string(nil), saved.MirrorURLs...)
			o.SourceOrigin = components.SourceFromSettings
		}
	}
	for _, t := range []struct {
		name string
		d    *time.Duration
		max  time.Duration
	}{
		{"connect-timeout", &o.ConnectTimeout, components.MaxConnectTimeout},
		{"stall-timeout", &o.StallTimeout, components.MaxStallTimeout},
		{"manifest-timeout", &o.ManifestTimeout, components.MaxManifestTimeout},
	} {
		if !inv.isSet(t.name) {
			continue
		}
		n := inv.intOpt(t.name)
		if n < 1 || int64(n) > int64(t.max/time.Second) {
			return o, errs.Newf(errs.InvalidArgument, "--%s 要在 1–%.0f 秒之间，收到 %d", t.name, t.max.Seconds(), n)
		}
		*t.d = time.Duration(n) * time.Second
	}
	if inv.isSet("test-free-space-mb") {
		n := int64(inv.intOpt("test-free-space-mb"))
		if n < 0 {
			return o, errs.New(errs.InvalidArgument, "--test-free-space-mb 不能是负数")
		}
		o.TestFreeSpaceMB = &n
	}
	if _, err := components.ChooseMirrors(o.Mirror, o.MirrorURLs); err != nil {
		return o, err
	}
	// whatever the root holds: a script that sometimes gets ok and sometimes usage for the same command line (by
	// whether components were missing) would only learn of the license at the worst moment
	if o.JSON && !o.AcceptLicense {
		return o, components.JSONNeedsLicense()
	}
	var last time.Time
	o.Progress = func(p components.Progress) {
		if !p.Final && !last.IsZero() && time.Since(last) < 2*time.Second {
			return
		}
		last = time.Now()
		c.log.Infof("%s", p.Message)
	}
	unfiltered := logx.JSONSink(c.stderr, logx.Debug)
	o.Notice = func(text string) {
		if !inv.json {
			fmt.Fprint(c.stderr, text)
		} else if s := strings.Trim(text, "\n"); s != "" {
			unfiltered(logx.Info, s)
		}
	}
	return o, nil
}

func askComponentSource(c *cmdCtx, input io.Reader) (components.SourceSettings, error) {
	fmt.Fprintln(c.stderr, "组件下载源还没有设置，请选择：")
	fmt.Fprintln(c.stderr, "  1. 自动（googledownloads_cn、tencent、google，按顺序尝试）")
	fmt.Fprintln(c.stderr, "  2. googledownloads_cn")
	fmt.Fprintln(c.stderr, "  3. tencent")
	fmt.Fprintln(c.stderr, "  4. google")
	fmt.Fprint(c.stderr, "选择 [1]：")
	line, err := bufio.NewReader(input).ReadString('\n')
	if err == io.EOF {
		// EOF without an interactive answer means automatic order for this run;
		// do not persist a choice inferred from a closed pipe.
		return components.SourceSettings{}, nil
	}
	choice := strings.TrimSpace(line)
	if choice == "" || choice == "1" {
		return components.SaveSourceSettings("", nil)
	}
	ids := map[string]string{"2": "googledownloads_cn", "3": "tencent", "4": "google"}
	mirror, ok := ids[choice]
	if !ok {
		return components.SourceSettings{}, errs.Newf(errs.InvalidArgument, "组件下载源编号无效：%s", choice)
	}
	return components.SaveSourceSettings(mirror, nil)
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

func runPlayStore(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	action := strings.ToLower(c.inv.pos[1])
	switch action {
	case "install", "uninstall", "status", "gsfid":
	default:
		return nil, errs.Newf(errs.Usage, "第二个参数只能是 install、uninstall、status 或 gsfid，收到「%s」", c.inv.pos[1]).
			WithAdvice(findCommand("playstore").usageAdvice())
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	if action == "gsfid" {
		return m.PlayStoreGSFID(c.ctx, i)
	}
	return m.PlayStore(c.ctx, i, action)
}

func humanPlayStore(w io.Writer, data any) {
	switch r := data.(type) {
	case *emu.PlayStoreResult:
		formatPlayStore(w, r)
	case *emu.PlayStoreGSFIDResult:
		formatPlayStoreGSFID(w, r)
	}
}

// exitPlayStore: 1 when the store did not end up as asked (installed after install, absent after uninstall). status
// and --json: 0, the result says it.
func exitPlayStore(data any, json bool) int {
	r, ok := data.(*emu.PlayStoreResult)
	if json || !ok {
		return 0
	}
	if r.Action == "install" && !r.Installed || r.Action == "uninstall" && r.State != "absent" {
		return 1
	}
	return 0
}

// runDisk dispatches `disk <i> <action>` (trim, info, compact); add a case per new action.
func runDisk(c *cmdCtx) (any, error) {
	i, err := parseIndex(c.inv.pos[0])
	if err != nil {
		return nil, err
	}
	action := strings.ToLower(c.inv.pos[1])
	switch action {
	case "trim", "info", "compact":
	default:
		return nil, errs.Newf(errs.Usage, "第二个参数只能是 trim、info 或 compact，收到「%s」", c.inv.pos[1]).
			WithAdvice(findCommand("disk").usageAdvice())
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	switch action {
	case "trim":
		return m.DiskTrim(c.ctx, i)
	case "info":
		return m.DiskInfo(c.ctx, i)
	case "compact":
		return m.DiskCompact(c.ctx, i)
	}
	return nil, errs.Newf(errs.Internal, "disk 动作 %s 没有实现", action)
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

func runUpdate(c *cmdCtx) (any, error) {
	action := "status"
	if len(c.inv.pos) > 0 {
		action = strings.ToLower(c.inv.pos[0])
	}
	switch action {
	case "status", "check", "prepare", "install", "recover":
	default:
		return nil, errs.Newf(errs.Usage, "只能是 status、check、prepare、install 或 recover，收到「%s」", c.inv.pos[0]).
			WithAdvice(findCommand("update").usageAdvice())
	}
	if (c.inv.strOpt("package") != "" || c.inv.strOpt("sha256") != "") && action != "prepare" && action != "install" {
		return nil, errs.New(errs.Usage, "--package 和 --sha256 只用于 update prepare 或 install")
	}
	r, err := c.deps.resolveRoot(c.inv.root)
	if err != nil {
		return nil, err
	}
	var last time.Time
	u := update.New(r, c.log, update.Options{AutoMASVersion: c.inv.strOpt("automas-version"),
		PackagePath: c.inv.strOpt("package"), PackageSHA256: c.inv.strOpt("sha256"),
		Progress: func(p update.Progress) {
			if p.Stage != "downloading" || (time.Since(last) < 2*time.Second && p.Done != p.Total) {
				return
			}
			last = time.Now()
			c.log.Infof("%s：%.1f%%（%.1f / %.1f MB）", p.Message, p.Percent, float64(p.Done)/(1<<20), float64(p.Total)/(1<<20))
		}})
	switch action {
	case "check":
		return u.Check(c.ctx)
	case "prepare":
		return u.Prepare(c.ctx)
	case "install":
		return u.Install(c.ctx)
	case "recover":
		return u.Recover(c.ctx)
	}
	return u.Status(), nil
}

func formatUpdateStatus(w io.Writer, r *update.Status) {
	cur := "未知"
	if r.CurrentVersion != "" {
		cur = strings.TrimPrefix(r.CurrentVersion, "v")
	}
	if r.CurrentBuild != "" {
		cur += "（" + r.CurrentBuild + "）"
	}
	fmt.Fprintf(w, "ModAVD：%s，根目录 %s\n", cur, r.Root)
	src := "GitHub"
	if r.Source == update.SourceMirrorChyan {
		src = "Mirror 酱（CDK " + map[bool]string{true: "已填", false: "没填，只能从 GitHub 下载"}[r.CDKSet] + "）"
	}
	fmt.Fprintf(w, "更新源：%s\n", src)
	if !r.Updatable {
		fmt.Fprintf(w, "不能更新：%s\n", r.NotUpdatableReason)
	}
	if r.Checked {
		fmt.Fprintf(w, "最新版本：%s（%s）\n", strings.TrimPrefix(r.LatestVersion, "v"), r.LatestSource)
	}
	if r.PreparedVersion != "" {
		fmt.Fprintf(w, "已下载并核对好、待安装：%s\n", strings.TrimPrefix(r.PreparedVersion, "v"))
	}
	if r.Recovered != "" {
		fmt.Fprintln(w, r.Recovered)
	}
	for _, n := range r.Notices {
		fmt.Fprintln(w, n)
	}
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
		if err := removedError(c.inv.pos[0]); err != nil {
			return nil, err
		}
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
