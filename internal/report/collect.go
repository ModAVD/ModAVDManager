package report

// collect-info: port of collect-info.ps1, the diagnostics zip. Section by section in the script's order, each one on
// its own: a section that fails is written into summary.txt ("[本节出错] …") and the next one runs.

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"modavd/internal/appproxy"
	"modavd/internal/buildinfo"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// The script's limits. Variables so tests can lower them.
var (
	logCapBytes     int64 = 300 * mb // $LogCapBytes: emulator logs and logcats together
	masLogTailBytes int64 = 30 * mb  // $MasLogTailBytes: only the end of a longer AUTO-MAS log
	dumpMaxBytes    int64 = 100 * mb // $DumpMaxBytes: larger crash dumps are listed, not taken
	// modavd's own: the failed boots' directories (bootDiagDirs); not in the script
	bootDiagCapBytes int64 = 100 * mb
)

const (
	nativeTimeout   = 20 * time.Second // Invoke-Native -TimeoutSec default
	accelTimeout    = 30 * time.Second
	collectZipStart = "魔改AVD问题信息-"
)

// CollectOptions for Collect; the first three are the script's -OutDir, -StageDir and -KeepStage.
type CollectOptions struct {
	OutDir     string // where the zip goes; "" = the desktop
	StageDir   string // parent of the stage directory; "" = %TEMP%
	KeepStage  bool   // keep the stage directory
	NoExplorer bool   // do not open Explorer on the zip (tests, automation)
	// Progress gets the lines the script prints while it works ("正在收集：…").
	Progress func(msg string)
	Host     Host // nil: this computer

	// tests
	now      func() time.Time
	getenv   func(string) string
	pid      int
	explorer func(path string) error
}

// CollectResult describes the zip that was written.
type CollectResult struct {
	ZipPath  string   `json:"zip_path"`
	ZipBytes int64    `json:"zip_bytes"`
	ZipSize  string   `json:"zip_size"` // as printed: 6.8 MB
	Entries  []string `json:"entries"`  // in zip order
	Stage    string   `json:"stage"`
	// StageKept: the stage directory is still there (KeepStage, or it could not be removed: StageError).
	StageKept  bool   `json:"stage_kept"`
	StageError string `json:"stage_error"`
	// Problems are the sections (and instances) that failed, "名称：原因", as in summary.txt.
	Problems    []string `json:"problems"`
	MasFound    bool     `json:"mas_found"` // the AUTO-MAS installation was found (its logs are in mas/)
	Summary     string   `json:"summary"`   // summary.txt (CRLF, without the BOM)
	PrivacyNote string   `json:"privacy_note"`
}

// Collect is collect-info.ps1: collects into a stage directory, writes summary.txt, zips the stage into OutDir and
// opens Explorer on the zip. Read-only apart from the stage directory (its own, removed afterwards) and the zip.
func Collect(ctx context.Context, r *root.Root, opt CollectOptions) (*CollectResult, error) {
	if runtime.GOOS != "windows" {
		return nil, errs.New(errs.Unsupported, "收集问题信息目前只支持 Windows")
	}
	h := opt.Host
	if h == nil {
		var err error
		if h, err = newHost(); err != nil {
			return nil, err
		}
	}
	return collect(ctx, r.Dir, h, opt)
}

// liveTarget is a running instance to take the live state of.
type liveTarget struct {
	name        string
	pid         uint32
	consolePort int
	inferred    bool // from the adb device list, the qemu command line being unreadable
}

type collector struct {
	ctx   context.Context
	root  string
	h     Host
	opt   CollectOptions
	stage string
	start time.Time

	lines    []string
	problems []string
	timings  []string

	instances         []string // every <name>.ini in avd\
	sdkDir            string   // as mas-avd.json / the root gives it (shown)
	sdkSource         string
	adbPort           int
	plan              root.PortPlan // mas-avd.json's portBase (ModAVD/ModAVD#75); the script's PORT_BASE otherwise
	running           []liveTarget
	cmdlineUnreadable bool
	masFound          bool
}

func (c *collector) L(s string) { c.lines = append(c.lines, s) }

func (c *collector) section(title string) {
	c.L("")
	c.L("== " + title + " ==")
}

func (c *collector) progress(msg string) {
	if c.opt.Progress != nil {
		c.opt.Progress(msg)
	}
}

// step is Invoke-Step: runs one section, records its failure and how long it took.
func (c *collector) step(name string, fn func() error) {
	if c.ctx.Err() != nil {
		return
	}
	c.progress("正在收集：" + name)
	t0 := c.opt.now()
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("内部错误：%v", p)
			}
		}()
		return fn()
	}()
	if err != nil {
		msg := errText(err)
		c.L("[本节出错] " + name + "：" + msg)
		c.problems = append(c.problems, name+"："+msg)
	}
	c.timings = append(c.timings, fmt.Sprintf("%s：%s 秒", name, formatN(c.opt.now().Sub(t0).Seconds(), 1)))
}

// errText is the message of an error as summary.txt shows it.
func errText(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

// sdk is the SDK directory for reading files: sdkDir made absolute against the root.
func (c *collector) sdk() string { return absUnder(c.root, c.sdkDir) }

func collect(ctx context.Context, rootDir string, h Host, opt CollectOptions) (*CollectResult, error) {
	if opt.now == nil {
		opt.now = time.Now
	}
	if opt.getenv == nil {
		opt.getenv = os.Getenv
	}
	if opt.pid == 0 {
		opt.pid = os.Getpid()
	}
	if opt.explorer == nil {
		opt.explorer = openExplorer
	}
	start := opt.now()
	stamp := start.Format("20060102-150405")
	outDir := opt.OutDir
	if outDir == "" {
		d, err := desktopDir()
		if err != nil {
			return nil, err
		}
		outDir = d
	}
	stageDir := opt.StageDir
	if stageDir == "" {
		if stageDir = opt.getenv("TEMP"); stageDir == "" {
			stageDir = os.TempDir()
		}
	}
	stageDir, err := filepath.Abs(stageDir)
	if err != nil {
		return nil, errs.Wrap(errs.InvalidArgument, err, "暂存目录写法不对："+opt.StageDir)
	}
	if err := os.MkdirAll(stageDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.Internal, err, "建不了暂存目录："+err.Error())
	}
	stage := filepath.Join(stageDir, "modavd-collect-"+stamp+"-"+strconv.Itoa(opt.pid))
	if err := os.Mkdir(stage, 0o755); err != nil {
		return nil, errs.Wrap(errs.Internal, err, "建不了暂存目录："+err.Error())
	}

	c := &collector{ctx: ctx, root: rootDir, h: h, opt: opt, stage: stage, start: start, adbPort: root.DefaultAdbServerPort}
	c.progress("魔改 AVD 问题信息收集")
	c.progress("ModAVD 目录：" + rootDir)
	c.L("魔改 AVD 问题信息")
	c.L(PrivacyNote)
	c.L("生成时间：" + start.Format("2006-01-02 15:04:05 -07:00"))
	// the script names the PowerShell that ran it here
	c.L("modavd：" + buildinfo.Version + "（" + runtime.Version() + "）")

	c.step("ModAVD 目录与版本", c.rootAndVersions)
	c.step("Windows / CPU / 内存 / 显卡", c.computer)
	c.step("虚拟化功能", c.features)
	c.step("加速检查（emulator -accel-check）", c.accelCheck)
	c.step("杀毒软件", c.antivirus)
	c.step("实例列表与实例文件", c.instanceFiles)
	c.step("正在运行的模拟器与 adb", c.runningAndAdb)
	c.step("模拟器日志", c.emulatorLogs)
	c.step("模拟器崩溃转储", c.crashDumps)
	c.step("AUTO-MAS 日志", c.autoMAS)
	c.step("系统事件日志", c.events)
	c.step("现场截图与状态", c.live)
	if err := ctx.Err(); err != nil {
		// Ctrl+C: no half-collected zip; the stage directory is ours to remove
		_ = os.RemoveAll(stage)
		return nil, errs.Wrap(errs.Canceled, err, "已取消")
	}

	elapsed := opt.now().Sub(start).Seconds()
	c.section("收集过程")
	c.L("收集耗时：" + formatN(elapsed, 1) + " 秒")
	for _, t := range c.timings {
		c.L("  " + t)
	}
	if len(c.problems) > 0 {
		c.L("出错的节：")
		for _, p := range c.problems {
			c.L("  " + p)
		}
	} else {
		c.L("各节都没出错。")
	}
	if !c.masFound {
		c.L("")
		c.L("提示：没找到 AUTO-MAS 安装目录，反馈时请附上 AUTO-MAS 的 debug 文件夹里的日志。")
	}

	res := &CollectResult{Stage: stage, Problems: append([]string{}, c.problems...), MasFound: c.masFound,
		PrivacyNote: PrivacyNote, Entries: []string{}}
	res.Summary = strings.Join(c.lines, "\r\n") + "\r\n"
	if err := writeLinesBOM(filepath.Join(stage, "summary.txt"), c.lines); err != nil {
		return nil, packFailed(err, stage)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, packFailed(err, stage)
	}
	zipPath := filepath.Join(outDir, collectZipStart+stamp+".zip")
	if exists(zipPath) {
		zipPath = filepath.Join(outDir, collectZipStart+stamp+"-"+strconv.Itoa(opt.pid)+".zip")
	}
	entries, err := zipStage(zipPath, stage)
	if err != nil {
		return nil, packFailed(err, stage)
	}
	res.ZipPath, res.Entries = zipPath, entries
	if fi, err := os.Stat(zipPath); err == nil {
		res.ZipBytes = fi.Size()
	}
	res.ZipSize = formatSize(float64(res.ZipBytes))

	if opt.KeepStage {
		res.StageKept = true
	} else if err := os.RemoveAll(stage); err != nil {
		res.StageKept, res.StageError = true, err.Error()
	}
	if !opt.NoExplorer {
		_ = opt.explorer(zipPath)
	}
	return res, nil
}

// packFailed: the zip could not be written; the collected files stay in the stage directory.
func packFailed(err error, stage string) error {
	return errs.Wrap(errs.Internal, err, "打包失败："+err.Error()).WithAdvice("收集到的文件留在：" + stage)
}

// ---- 1. ModAVD 目录与版本 ----------------------------------------------------------------------------------------

func (c *collector) rootAndVersions() error {
	c.section("ModAVD 目录")
	c.L("路径：" + c.root)
	if hasNonASCII(c.root) {
		c.L(`  警告：路径里有中文或其他非 ASCII 字符，模拟器可能起不来，请把 ModAVD 放到纯英文路径下（例如 D:\ModAVD）。`)
	}
	if !exists(c.root) {
		c.L("  警告：这个目录不存在。")
	}
	if d, err := c.h.Drive(pathRoot(c.root)); err == nil {
		c.L(fmt.Sprintf("所在盘：%s 剩余 %s / 共 %s（%s）", d.Name, formatSize(float64(d.Free)), formatSize(float64(d.Total)), d.Format))
	} else {
		c.L("所在盘剩余空间：读不到（" + errText(err) + "）")
	}

	// as the backend's read_metadata: missing, unreadable or not a JSON object all count as empty
	cfg, msg := readConfig(filepath.Join(c.root, root.MetadataFile))
	if msg != "" {
		c.L(msg)
	}

	// the port plan: modavd's mas-avd.json portBase (the scripts know only PORT_BASE 20000, so nothing is said for it)
	plan, problem := root.PortPlanOf(cfg)
	c.plan = plan
	if problem != "" {
		c.L(fmt.Sprintf("端口段：%d（%s；看状态时按 %d 算，modavd 不让开机）", plan.Base, problem, plan.Base))
	} else if plan.Base != root.DefaultPortBase {
		c.L(fmt.Sprintf("端口段：%d–%d（mas-avd.json 的 portBase；实例 i 的控制台端口是 %d + 10 × i）", plan.Base,
			plan.Last(), plan.Base))
	}

	// the adb server port: the backend's adb_server_port(), back to 20050 when not valid; never 5037
	raw := getCI(cfg, "adbServerPort")
	hasPort := hasCI(cfg, "adbServerPort")
	if port, ok := scriptPort(c.plan, raw); ok {
		c.adbPort = port
		c.L(fmt.Sprintf("adb server 端口：%d（mas-avd.json 的 adbServerPort）", port))
	} else {
		c.adbPort = root.DefaultAdbServerPort
		if hasPort {
			c.L(fmt.Sprintf("adb server 端口：%[1]d（mas-avd.json 的 adbServerPort = %[2]s 不合规：要 1024–65535 的整数、不是 5037、不占实例端口；按后端规则回到 %[1]d）",
				root.DefaultAdbServerPort, psJSON(raw)))
		} else {
			c.L(fmt.Sprintf("adb server 端口：%d（mas-avd.json 没有配置，按默认）", root.DefaultAdbServerPort))
		}
	}

	// the SDK: as the backend's runtime_sdk_dir(), mas-avd.json's sdkRoot (a non-blank string) first, else <root>\sdk
	if s := sdkRootOf(cfg); s != "" {
		c.sdkDir, c.sdkSource = s, "mas-avd.json 的 sdkRoot"
		c.L(fmt.Sprintf("SDK：%s（mas-avd.json 的 sdkRoot；后端优先用它，包内 sdk 不用）", s))
		if !exists(filepath.Join(c.sdk(), "emulator")) {
			c.L("  警告：sdkRoot 指向的目录里没有 emulator，后端照样会用它，模拟器起不来。")
		}
	} else {
		c.sdkDir, c.sdkSource = filepath.Join(c.root, root.SDKDir), "包内 sdk"
		c.L(fmt.Sprintf("SDK：%s（包内 sdk）", c.sdkDir))
		if !exists(filepath.Join(c.sdkDir, "emulator")) {
			c.L(`  警告：找不到 sdk\emulator，模拟器文件不全。`)
		}
	}

	c.section("模拟器版本")
	emu, err := readProps(filepath.Join(c.sdk(), "emulator", "source.properties"))
	if err != nil {
		return err
	}
	c.L(fmt.Sprintf("emulator：Pkg.Revision=%s  Pkg.BuildId=%s", prop(emu, "Pkg.Revision"), prop(emu, "Pkg.BuildId")))
	pt, err := readProps(filepath.Join(c.sdk(), "platform-tools", "source.properties"))
	if err != nil {
		return err
	}
	c.L("platform-tools：Pkg.Revision=" + prop(pt, "Pkg.Revision"))
	sdk := c.sdk()
	imgs := findFiles(filepath.Join(sdk, "system-images"), func(name string) bool {
		return strings.EqualFold(name, "source.properties")
	})
	if len(imgs) == 0 {
		c.L(`系统镜像：没找到 system-images\…\source.properties`)
	}
	for _, f := range imgs {
		sp, err := readProps(f.path)
		if err != nil {
			return err
		}
		rel := filepath.Dir(f.path)
		if len(rel) >= len(sdk) {
			rel = strings.TrimLeft(rel[len(sdk):], `\`)
		}
		c.L(fmt.Sprintf("系统镜像 %s：Pkg.Revision=%s ApiLevel=%s Abi=%s TagId=%s Desc=%s", rel, prop(sp, "Pkg.Revision"),
			prop(sp, "AndroidVersion.ApiLevel"), prop(sp, "SystemImage.Abi"), prop(sp, "SystemImage.TagId"), prop(sp, "Pkg.Desc")))
	}
	return nil
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 0x7f {
			return true
		}
	}
	return false
}

// pathRoot is [IO.Path]::GetPathRoot: `E:\` for a drive path, `\\server\share` for a UNC one.
func pathRoot(dir string) string {
	vol := filepath.VolumeName(dir)
	if len(vol) == 2 && vol[1] == ':' {
		return vol + `\`
	}
	return vol
}

// foundFile is one file of a recursive search.
type foundFile struct {
	path string
	info fs.FileInfo
}

// findFiles is Get-ChildItem -LiteralPath dir -Recurse -File -Filter …: in each directory its matching files, then
// its subdirectories one by one; hidden entries skipped; unreadable directories skipped (-ErrorAction
// SilentlyContinue).
func findFiles(dir string, match func(name string) bool) []foundFile {
	ents, err := listDir(dir, false)
	if err != nil {
		return nil
	}
	var out []foundFile
	for _, e := range ents {
		if !e.IsDir() && match(e.Name()) {
			out = append(out, foundFile{filepath.Join(dir, e.Name()), e})
		}
	}
	for _, e := range ents {
		if e.IsDir() {
			out = append(out, findFiles(filepath.Join(dir, e.Name()), match)...)
		}
	}
	return out
}

// ---- 2. 本机 ----------------------------------------------------------------------------------------------------

func (c *collector) computer() error {
	c.section("本机")
	o, err := c.h.OS(c.ctx)
	if err != nil {
		return err
	}
	ubr := ""
	if cv, err := c.h.CurrentVersion(); err == nil {
		ubr = cv.UBR
	}
	c.L(fmt.Sprintf("Windows：%s  Version=%s  BuildNumber=%s  UBR=%s  %s", o.Caption, o.Version, o.BuildNumber, ubr, o.OSArchitecture))
	cpus, err := c.h.Processors(c.ctx)
	if err != nil {
		return err
	}
	for _, p := range cpus {
		c.L(fmt.Sprintf("CPU：%s  物理核 %s  逻辑核 %s  VirtualizationFirmwareEnabled=%s", netTrim(p.Name), optInt(p.Cores),
			optInt(p.Threads), optBool(p.VirtFirmware)))
	}
	if cs, err := c.h.ComputerSystem(c.ctx); err == nil {
		c.L(fmt.Sprintf("HypervisorPresent=%s（为 True 时 VirtualizationFirmwareEnabled 显示 False 是正常的）", optBool(cs.HypervisorPresent)))
	}
	c.L(fmt.Sprintf("内存：共 %s，可用 %s", formatSize(o.TotalVisibleKB*kb), formatSize(o.FreePhysicalKB*kb)))
	gpus, err := c.h.VideoControllers(c.ctx)
	if err != nil {
		return err
	}
	for _, g := range gpus {
		c.L(fmt.Sprintf("显卡：%s  DriverVersion=%s  DriverDate=%s", g.Name, g.DriverVersion, g.DriverDate))
	}
	return nil
}

// virtFeatures are the features collect-info.ps1 lists, in its order.
var virtFeatures = []string{"HypervisorPlatform", "VirtualMachinePlatform", "Microsoft-Hyper-V-All"}

func (c *collector) features() error {
	// with Hyper-V on, HypervisorPlatform shows as off and WHPX works anyway: the accel-check below decides
	c.section("虚拟化相关 Windows 功能（InstallState 1=已开 2=未开 3=缺失；仅供参考，能不能用看下一节 accel-check）")
	feats, err := c.h.OptionalFeatures(c.ctx)
	if err != nil {
		return err
	}
	state := map[string]*int64{}
	for _, f := range feats {
		state[strings.ToLower(f.Name)] = f.InstallState
	}
	for _, n := range virtFeatures {
		st, ok := state[strings.ToLower(n)]
		if !ok {
			c.L(n + "：没查到")
			continue
		}
		txt := "未知"
		if st != nil {
			switch *st {
			case 1:
				txt = "已开"
			case 2:
				txt = "未开"
			case 3:
				txt = "缺失"
			}
		}
		c.L(fmt.Sprintf("%s：InstallState=%s（%s）", n, optInt(st), txt))
	}
	return nil
}

func (c *collector) accelCheck() error {
	c.section("emulator -accel-check")
	if c.sdkDir == "" {
		c.L(`跳过：没有 sdk\emulator`)
		return nil
	}
	exe := filepath.Join(c.sdkDir, "emulator", "emulator.exe")
	if !exists(filepath.Join(c.sdk(), "emulator", "emulator.exe")) {
		c.L("跳过：没有 " + exe)
		return nil
	}
	r, err := c.h.Run(c.ctx, filepath.Join(c.sdk(), "emulator", "emulator.exe"), []string{"-accel-check"}, accelTimeout)
	if err != nil {
		return err
	}
	code := ""
	if r.TimedOut {
		c.L("超时（30 秒），已放弃")
	} else {
		code = strconv.Itoa(r.ExitCode)
	}
	c.L("退出码：" + code)
	for _, line := range splitCRLF(decodeText(r.Stdout) + "\n" + decodeText(r.Stderr)) {
		if netTrim(line) != "" {
			c.L("  " + netTrimEnd(line))
		}
	}
	return nil
}

func (c *collector) antivirus() error {
	c.section("杀毒软件（SecurityCenter2）")
	av, err := c.h.Antivirus(c.ctx)
	if err != nil {
		return err
	}
	if len(av) == 0 {
		c.L("没查到")
	}
	for _, a := range av {
		c.L(fmt.Sprintf("%s  productState=0x%06X", a.DisplayName, uint32(int32(a.ProductState))))
	}
	return nil
}

// ---- 3. 实例 ----------------------------------------------------------------------------------------------------

// avdFiles are the instance files collected; never the disk images, snapshots or data next to them.
var avdFiles = []string{"config.ini", "hardware-qemu.ini", "emulator-user.ini"}

func (c *collector) instanceFiles() error {
	c.section("实例")
	avdDir := filepath.Join(c.root, root.AvdDir)
	var inis []fs.FileInfo
	if exists(avdDir) {
		files, err := listFiles(avdDir)
		if err != nil {
			return err
		}
		for _, f := range files {
			if strings.HasSuffix(strings.ToLower(f.Name()), ".ini") {
				inis = append(inis, f)
			}
		}
	}
	if len(inis) == 0 {
		c.L("还没有实例（avd 目录不存在或里面没有 *.ini）。")
	}
	// every instance is registered first (its ini is there), so a broken one does not hide it from the live part
	c.instances = nil
	for _, ini := range inis {
		c.instances = append(c.instances, strings.TrimSuffix(ini.Name(), filepath.Ext(ini.Name())))
	}
	for k, ini := range inis {
		name := c.instances[k]
		// one at a time: a broken instance is written down and the next one collected
		if err := c.instanceFilesOf(avdDir, ini, name); err != nil {
			c.L(fmt.Sprintf("%s：收集这台实例时出错：%s", name, errText(err)))
			c.problems = append(c.problems, "实例 "+name+"："+errText(err))
		}
	}
	cfgPath := filepath.Join(c.root, root.MetadataFile)
	if exists(cfgPath) {
		if _, err := copyShared(cfgPath, filepath.Join(c.stage, root.MetadataFile), 0); err != nil {
			c.L("复制 mas-avd.json 失败：" + errText(err))
		}
	} else {
		c.L("没有 mas-avd.json")
	}
	return nil
}

func (c *collector) instanceFilesOf(avdDir string, ini fs.FileInfo, name string) error {
	if _, err := copyShared(filepath.Join(avdDir, ini.Name()), filepath.Join(c.stage, root.AvdDir, ini.Name()), 0); err != nil {
		return err
	}
	dir := filepath.Join(avdDir, name+".avd")
	if !exists(dir) {
		c.L(name + "：残缺——有 " + ini.Name() + "，但缺少 " + name + ".avd 目录")
		return nil
	}
	outDir := filepath.Join(c.stage, root.AvdDir, name+".avd")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	var present []string
	for _, fn := range avdFiles {
		if isFile(filepath.Join(dir, fn)) {
			present = append(present, fn)
		}
	}
	if len(present) == 0 {
		c.L(fmt.Sprintf("%[1]s：残缺——%[1]s.avd 里 config.ini、hardware-qemu.ini、emulator-user.ini 一个都没有（多半是删实例删到一半）", name))
	} else {
		cfg, err := readProps(filepath.Join(dir, "config.ini"))
		if err != nil {
			return err
		}
		c.L(fmt.Sprintf("%s：hw.ramSize=%s  hw.cpu.ncore=%s  hw.lcd=%sx%s density=%s  image.sysdir.1=%s", name,
			prop(cfg, "hw.ramSize"), prop(cfg, "hw.cpu.ncore"), prop(cfg, "hw.lcd.width"), prop(cfg, "hw.lcd.height"),
			prop(cfg, "hw.lcd.density"), prop(cfg, "image.sysdir.1")))
		if !containsFold(present, "config.ini") {
			c.L(fmt.Sprintf("  警告：%s.avd 里没有 config.ini", name))
		}
	}
	for _, fn := range present {
		if _, err := copyShared(filepath.Join(dir, fn), filepath.Join(outDir, fn), 0); err != nil {
			c.L("  复制 " + fn + " 失败：" + errText(err))
		}
	}
	// the file list: names and sizes only, never the contents
	items, err := listDir(dir, true)
	if err != nil {
		return err
	}
	var lst []string
	for _, it := range items {
		if it.IsDir() {
			lst = append(lst, padRight(it.Name(), 40)+" <DIR>")
		} else {
			lst = append(lst, padRight(it.Name(), 40)+" "+padLeft(formatN(float64(it.Size()), 0), 16)+"  "+
				it.ModTime().Local().Format("2006-01-02 15:04:05"))
		}
	}
	if len(lst) == 0 {
		lst = append(lst, "（空目录）")
	}
	return writeLinesBOM(filepath.Join(outDir, "_files.txt"), lst)
}

var (
	avdArg   = regexp.MustCompile(`(?i)(?:^|\s)-avd\s+"?([^\s"]+)`)
	portsArg = regexp.MustCompile(`(?i)(?:^|\s)-ports?\s+"?([0-9]+)`)
)

// otherRootsInstance: a qemu of instance avd on console port is another root's. Only with a plan of its own
// (portBase): then the same-named instance of another root can run at the same time, on that root's ports. Without
// one, as the script: two such instances could not run at once (they would share ports).
func otherRootsInstance(plan root.PortPlan, avd string, port int) bool {
	if plan.Base == root.DefaultPortBase {
		return false
	}
	i, err := strconv.Atoi(strings.TrimPrefix(strings.ToLower(avd), root.AvdNamePrefix))
	return err == nil && port != plan.Console(i)
}

func (c *collector) runningAndAdb() error {
	c.section("正在运行的 qemu-system 进程")
	qs, err := c.h.Processes(func(name string) bool { return hasPrefixFold(name, "qemu-system") })
	if err != nil {
		return err
	}
	if len(qs) == 0 {
		c.L("没有")
	}
	var running []liveTarget
	for _, q := range qs {
		ct := ""
		if !q.Started.IsZero() {
			ct = q.Started.Local().Format("2006-01-02 15:04:05")
		}
		c.L(fmt.Sprintf("pid %d  启动于 %s", q.PID, ct))
		cmd := q.CommandLine
		if cmd == "" {
			c.L("  qemu 命令行读不到（多半是 AUTO-MAS 以管理员运行），现场按 adb 设备列表推算")
			c.cmdlineUnreadable = true
			continue
		}
		c.L("  " + cmd)
		avd := ""
		if m := avdArg.FindStringSubmatch(cmd); m != nil {
			avd = m[1]
		}
		port := 0
		if m := portsArg.FindStringSubmatch(cmd); m != nil {
			port, _ = strconv.Atoi(m[1])
		}
		if avd != "" && containsFold(c.instances, avd) {
			if otherRootsInstance(c.plan, avd, port) {
				c.L(fmt.Sprintf("  端口 %d 不是本目录 %s 的（端口段 %d），是别的根目录的同名实例，跳过", port, avd, c.plan.Base))
				continue
			}
			running = append(running, liveTarget{name: avd, pid: q.PID, consolePort: port})
		}
	}
	c.running = running

	c.section("正在监听的 adb server")
	ls, err := c.adbListeners()
	if err != nil {
		return err
	}
	if len(ls) == 0 {
		c.L("没有")
	}
	for _, a := range ls {
		p := a.path
		if p == "" {
			p = "（路径读不到）"
		}
		c.L(fmt.Sprintf("端口 %d（%s）  pid %d  %s", a.port, a.addr, a.pid, p))
	}
	return nil
}

// adbListener is one listening adb server.
type adbListener struct {
	port int
	pid  uint32
	addr string
	path string
}

// adbListeners is collect-info.ps1's Get-AdbListeners: the listening sockets owned by an adb.exe, once per
// port / pid, by port.
func (c *collector) adbListeners() ([]adbListener, error) {
	procs, err := c.h.Processes(func(name string) bool { return strings.EqualFold(name, "adb.exe") })
	if err != nil {
		return nil, err
	}
	byPID := map[uint32]Process{}
	for _, p := range procs {
		byPID[p.PID] = p
	}
	pairs, err := c.h.Listeners()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var list []adbListener
	for _, l := range pairs {
		p, ok := byPID[l.PID]
		if !ok {
			continue
		}
		key := fmt.Sprintf("%d/%d", l.Port, l.PID)
		if seen[key] {
			continue
		}
		seen[key] = true
		list = append(list, adbListener{port: l.Port, pid: l.PID, addr: l.Addr, path: p.ExePath})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].port < list[j].port })
	return list, nil
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// ---- 4. 日志 ----------------------------------------------------------------------------------------------------

var (
	logName  = regexp.MustCompile(`(?i)^(?:logcat-)?(.+)-[0-9]{8}-[0-9]{6}\.(?:log|txt)$`)
	logStamp = regexp.MustCompile(`^.*-([0-9]{8}-[0-9]{6})\..*$`)
	hangName = regexp.MustCompile(`(?i)^startup-.*-hang-threads\.txt$`)
	hangTime = regexp.MustCompile(`-([0-9]{8}-[0-9]{6})-`)
)

// logCandidate is one emulator log or logcat that may be copied.
type logCandidate struct {
	file  fs.FileInfo
	path  string
	kind  string // emu / logcat
	stamp string
	len   int64
}

func (c *collector) emulatorLogs() error {
	c.section(`模拟器日志（logs\）`)
	logDir := filepath.Join(c.root, root.LogsDir)
	if !exists(logDir) {
		c.L("没有 logs 目录")
		return nil
	}
	all, err := listFiles(logDir)
	if err != nil {
		return err
	}
	var names []string
	addName := func(n string) {
		if !containsFold(names, n) {
			names = append(names, n)
		}
	}
	for _, n := range c.instances {
		addName(n)
	}
	for _, f := range all {
		if m := logName.FindStringSubmatch(f.Name()); m != nil {
			addName(m[1])
		}
	}
	sortCulture(names)
	var cand []logCandidate
	for _, n := range names {
		esc := regexp.QuoteMeta(n)
		emuRe := regexp.MustCompile(`(?i)^` + esc + `-[0-9]{8}-[0-9]{6}\.log$`)
		catRe := regexp.MustCompile(`(?i)^logcat-` + esc + `-[0-9]{8}-[0-9]{6}\.txt$`)
		// the time stamp in the name is the boot time: by name, descending, is newest first
		emuLogs := newestByName(all, emuRe, 8)
		catLogs := newestByName(all, catRe, 8)
		for _, group := range []struct {
			kind  string
			files []fs.FileInfo
		}{{"emu", emuLogs}, {"logcat", catLogs}} {
			for _, f := range group.files {
				p := filepath.Join(logDir, f.Name())
				n, err := trueLength(p)
				if err != nil {
					return err
				}
				cand = append(cand, logCandidate{file: f, path: p, kind: group.kind, stamp: logStamp.ReplaceAllString(f.Name(), "$1"), len: n})
			}
		}
		c.L(fmt.Sprintf("%s：模拟器日志 %d 份，logcat %d 份（各取最新 8 份）", n, len(emuLogs), len(catLogs)))
	}
	var total int64
	for _, k := range cand {
		total += k.len
	}
	if total > logCapBytes {
		// over the cap: the oldest logcats go first, then the oldest emulator logs
		order := append([]logCandidate(nil), cand...)
		sort.SliceStable(order, func(i, j int) bool {
			ki, kj := order[i].kind != "logcat", order[j].kind != "logcat"
			if ki != kj {
				return !ki
			}
			return cultureCompare(order[i].stamp, order[j].stamp) < 0
		})
		drop := map[string]bool{}
		dropped := 0
		for _, k := range order {
			if total <= logCapBytes {
				break
			}
			drop[strings.ToLower(k.path)] = true
			total -= k.len
			dropped++
		}
		var kept []logCandidate
		for _, k := range cand {
			if !drop[strings.ToLower(k.path)] {
				kept = append(kept, k)
			}
		}
		cand = kept
		c.L(fmt.Sprintf("原始总量超过 %s，丢掉了最旧的 %d 份", formatSize(float64(logCapBytes)), dropped))
	}
	var copied int64
	n := 0
	for _, k := range cand {
		got, err := copyShared(k.path, filepath.Join(c.stage, root.LogsDir, k.file.Name()), 0)
		if err != nil {
			c.L("  复制 " + k.file.Name() + " 失败：" + errText(err))
			continue
		}
		copied += got
		n++
	}
	c.L(fmt.Sprintf("共复制 %d 份，%s", n, formatSize(float64(copied))))

	// thread dumps of a start-up that hung, a dozen KB each: the newest 3 by the time in the name, outside the cap
	var hang []fs.FileInfo
	for _, f := range all {
		if hangName.MatchString(f.Name()) {
			hang = append(hang, f)
		}
	}
	hangKey := func(f fs.FileInfo) string {
		if m := hangTime.FindStringSubmatch(f.Name()); m != nil {
			return m[1]
		}
		return f.ModTime().Local().Format("20060102-150405")
	}
	sort.SliceStable(hang, func(i, j int) bool {
		if d := cultureCompare(hangKey(hang[i]), hangKey(hang[j])); d != 0 {
			return d > 0
		}
		return cultureCompare(hang[i].Name(), hang[j].Name()) > 0
	})
	if len(hang) > 3 {
		hang = hang[:3]
	}
	if len(hang) == 0 {
		c.L("启动卡死线程转储（startup-*-hang-threads.txt）：没有")
	} else {
		hn := 0
		var hangNames []string
		for _, f := range hang {
			hangNames = append(hangNames, f.Name())
			if _, err := copyShared(filepath.Join(logDir, f.Name()), filepath.Join(c.stage, root.LogsDir, f.Name()), 0); err != nil {
				c.L("  复制 " + f.Name() + " 失败：" + errText(err))
				continue
			}
			hn++
		}
		c.L(fmt.Sprintf("启动卡死线程转储（startup-*-hang-threads.txt）：取了最新 %d 份：%s", hn, strings.Join(hangNames, "、")))
	}

	// the component fetch log: which mirror, which step failed
	fc := filepath.Join(logDir, "fetch-components.log")
	if exists(fc) {
		if _, err := copyShared(fc, filepath.Join(c.stage, root.LogsDir, "fetch-components.log"), 0); err != nil {
			c.L("补全组件日志（fetch-components.log）：复制失败：" + errText(err))
		} else {
			c.L("补全组件日志（fetch-components.log）：已收")
		}
	} else {
		c.L("补全组件日志（fetch-components.log）：没有")
	}
	c.bootDiagDirs(logDir)
	return nil
}

var bootDiagName = regexp.MustCompile(`(?i)^.+-([0-9]{8}-[0-9]{6})-bootfail$`)

// bootDiagDirs copies the guest state of failed boots (modavd start / create, logs\<instance>-<time>-bootfail\): the
// newest 3 by the time in the name, outside the cap like the thread dumps, but no more once they reach
// bootDiagCapBytes (the newest is always taken). collect-info.ps1 does not know them, so nothing is said when there
// are none.
func (c *collector) bootDiagDirs(logDir string) {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() && bootDiagName.MatchString(e.Name()) {
			dirs = append(dirs, e.Name())
		}
	}
	if len(dirs) == 0 {
		return
	}
	stamp := func(n string) string { return bootDiagName.FindStringSubmatch(n)[1] }
	sort.SliceStable(dirs, func(i, j int) bool {
		if d := cultureCompare(stamp(dirs[i]), stamp(dirs[j])); d != 0 {
			return d > 0
		}
		return cultureCompare(dirs[i], dirs[j]) > 0
	})
	var taken []string
	var total int64
	for _, d := range dirs {
		if len(taken) == 3 || (len(taken) > 0 && total >= bootDiagCapBytes) {
			break
		}
		files, err := listFiles(filepath.Join(logDir, d))
		if err != nil {
			c.L("  读不了 " + d + "：" + errText(err))
			continue
		}
		for _, f := range files {
			got, err := copyShared(filepath.Join(logDir, d, f.Name()), filepath.Join(c.stage, root.LogsDir, d, f.Name()), 0)
			if err != nil {
				c.L("  复制 " + d + `\` + f.Name() + " 失败：" + errText(err))
				continue
			}
			total += got
		}
		taken = append(taken, d)
	}
	c.L(fmt.Sprintf("开机失败时的客体现场（*-bootfail）：共 %d 个，取了最新 %d 个，%s：%s", len(dirs), len(taken),
		formatSize(float64(total)), strings.Join(taken, "、")))
}

// newestByName: the files whose name matches re, by name descending (Sort-Object Name -Descending), at most n.
func newestByName(all []fs.FileInfo, re *regexp.Regexp, n int) []fs.FileInfo {
	var out []fs.FileInfo
	for _, f := range all {
		if re.MatchString(f.Name()) {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return cultureCompare(out[i].Name(), out[j].Name()) > 0 })
	if len(out) > n {
		out = out[:n]
	}
	return out
}

var crashDB = regexp.MustCompile(`(?i)^emu-crash-[0-9]+\.[0-9]+\.[0-9]+-mas\.db$`)

func (c *collector) crashDumps() error {
	c.section("模拟器崩溃转储")
	c.L("收集范围：仅 ModAVD 的 emu-crash-<version>-mas.db；近 7 天内最新 3 份")
	var bases []string
	for _, b := range []struct{ env, sub string }{{"LOCALAPPDATA", `Temp\AndroidEmulator`}, {"TEMP", "AndroidEmulator"}} {
		v := c.opt.getenv(b.env)
		if v == "" {
			return fmt.Errorf("环境变量 %s 没有设置", b.env)
		}
		p := filepath.Join(v, b.sub)
		if exists(p) {
			// [IO.Path]::GetFullPath: also the 8.3 short names expanded, or a %TEMP% given as an 8.3 short name (PROFIL~1 style) would be
			// a second folder and every dump in it found twice
			full, err := filepath.Abs(p)
			if err != nil {
				full = p
			}
			full = longPath(full)
			if !containsFold(bases, full) {
				bases = append(bases, full)
			}
		}
	}
	since := c.opt.now().AddDate(0, 0, -7)
	type dump struct {
		path, db string
		info     fs.FileInfo
	}
	var dumps []dump
	for _, b := range bases {
		ents, err := listDir(b, false)
		if err != nil {
			continue
		}
		for _, db := range ents {
			if !db.IsDir() || !crashDB.MatchString(db.Name()) {
				continue
			}
			for _, f := range findFiles(filepath.Join(b, db.Name()), func(n string) bool { return strings.HasSuffix(strings.ToLower(n), ".dmp") }) {
				if !f.info.ModTime().Before(since) {
					dumps = append(dumps, dump{path: f.path, db: db.Name(), info: f.info})
				}
			}
		}
	}
	sort.SliceStable(dumps, func(i, j int) bool { return dumps[i].info.ModTime().After(dumps[j].info.ModTime()) })
	if len(dumps) > 3 {
		dumps = dumps[:3]
	}
	if len(dumps) == 0 {
		c.L("近 7 天没有")
		return nil
	}
	for _, d := range dumps {
		info := fmt.Sprintf("%s  %s  %s", d.path, d.info.ModTime().Local().Format("2006-01-02 15:04:05"), formatSize(float64(d.info.Size())))
		if d.info.Size() > dumpMaxBytes {
			c.L(info + "（超过 " + formatSize(float64(dumpMaxBytes)) + "，没拿）")
			continue
		}
		if _, err := copyShared(d.path, filepath.Join(c.stage, "crash", d.db, d.info.Name()), 0); err != nil {
			return err
		}
		c.L(info)
	}
	return nil
}

var (
	masDisplayName = regexp.MustCompile(`(?i)^AUTO-MAS`)
	masLogZip      = regexp.MustCompile(`(?i)^app\..*\.log\.zip$`)
)

func (c *collector) autoMAS() error {
	c.section("AUTO-MAS")
	loc, ver := "", ""
	entries, _ := c.h.UninstallEntries()
	for _, e := range entries {
		if masDisplayName.MatchString(e.DisplayName) && e.InstallLocation != "" && exists(e.InstallLocation) {
			loc, ver = e.InstallLocation, e.DisplayVersion
			break
		}
	}
	how := "卸载注册表"
	if loc == "" {
		procs, err := c.h.Processes(func(name string) bool { return strings.EqualFold(name, "AUTO-MAS.exe") })
		if err != nil {
			return err
		}
		for _, p := range procs {
			if p.ExePath != "" {
				loc, how = filepath.Dir(p.ExePath), "正在运行的 AUTO-MAS.exe"
				break
			}
		}
	}
	if loc == "" {
		c.L("没找到 AUTO-MAS 安装目录，反馈时请附上 AUTO-MAS 的 debug 文件夹里的日志")
		return nil
	}
	c.masFound = true
	if exe := filepath.Join(loc, "AUTO-MAS.exe"); ver == "" && exists(exe) {
		ver, _ = c.h.ProductVersion(exe)
	}
	c.L(fmt.Sprintf("安装目录：%s（来自%s）", loc, how))
	c.L("版本：" + ver)
	dbg := filepath.Join(loc, "debug")
	if !exists(dbg) {
		c.L("安装目录下没有 debug 文件夹")
		return nil
	}
	// the logs in debug\ only: config\ and data\ hold the (encrypted) account passwords, never taken
	for _, fn := range []string{"app.log", "frontend.log"} {
		src := filepath.Join(dbg, fn)
		if !exists(src) {
			c.L(fn + "：没有")
			continue
		}
		n, err := trueLength(src)
		var got int64
		if err == nil {
			got, err = copyShared(src, filepath.Join(c.stage, "mas", fn), masLogTailBytes)
		}
		switch {
		case err != nil:
			c.L("复制 " + fn + " 失败：" + errText(err))
		case n > masLogTailBytes:
			c.L(fmt.Sprintf("%s：%s，只取了最后 %s", fn, formatSize(float64(n)), formatSize(float64(got))))
		default:
			c.L(fmt.Sprintf("%s：%s", fn, formatSize(float64(got))))
		}
	}
	files, err := listFiles(dbg)
	if err != nil {
		return err
	}
	var zips []fs.FileInfo
	for _, f := range files {
		if masLogZip.MatchString(f.Name()) {
			zips = append(zips, f)
		}
	}
	sort.SliceStable(zips, func(i, j int) bool { return zips[i].ModTime().After(zips[j].ModTime()) })
	if len(zips) == 0 {
		c.L("没有轮转压缩包 app.*.log.zip")
		return nil
	}
	got, err := copyShared(filepath.Join(dbg, zips[0].Name()), filepath.Join(c.stage, "mas", zips[0].Name()), 0)
	if err != nil {
		return err
	}
	c.L(fmt.Sprintf("%s：%s", zips[0].Name(), formatSize(float64(got))))
	return nil
}

// eventHit is the script's filter: qemu, emulator, AUTO-MAS, or adb as a word (the hex hashes in WER report paths
// often contain "adb").
var eventHit = regexp.MustCompile(`(?i)qemu|emulator|AUTO-MAS`)

func (c *collector) events() error {
	c.section("近 7 天 Application 日志（ID 1000/1001/1002，含 qemu / emulator / adb / AUTO-MAS）")
	ev, err := c.h.AppEvents(c.ctx)
	if err != nil {
		return err
	}
	var hits []AppEvent
	for _, e := range ev {
		if eventHit.MatchString(e.Message) || containsWordFold(e.Message, "adb") {
			hits = append(hits, e)
			if len(hits) == 30 {
				break
			}
		}
	}
	if len(hits) == 0 {
		c.L("没有")
		return nil
	}
	for _, e := range hits {
		c.L(fmt.Sprintf("%s  ID %d  %s", e.Time, e.ID, e.Provider))
		n := 0
		for _, ln := range splitCRLF(e.Message) {
			if netTrim(ln) == "" {
				continue
			}
			c.L("    " + netTrim(ln))
			if n++; n == 6 {
				break
			}
		}
	}
	return nil
}

// ---- 5. 现场 ----------------------------------------------------------------------------------------------------

var (
	deviceLine = regexp.MustCompile(`(?i)^emulator-([0-9]+)[\s\p{Z}]+(\S+)`)
	focusLine  = regexp.MustCompile(`(?i)mCurrentFocus|mFocusedApp`)
)

// liveCommands are the commands run on each running instance, with the tag of their file.
var liveCommands = []struct {
	tag, kind string
	args      []string
}{
	{"getprop", "shell", []string{"getprop"}},
	{"window-focus", "shell", []string{"dumpsys", "window"}},
	{"logcat-crash", "logcat", []string{"-d", "-b", "crash"}},
}

func (c *collector) live() error {
	c.section("现场")
	if len(c.running) == 0 && !c.cmdlineUnreadable {
		c.L("没有实例在运行，跳过")
		return nil
	}
	// only a server that already listens; never one of our own, never 5037
	if c.adbPort == 5037 {
		c.L("adb 端口配置成了 5037，按规定不用，跳过")
		return nil
	}
	ls, err := c.adbListeners()
	if err != nil {
		return err
	}
	var server *adbListener
	for i := range ls {
		if ls[i].port == c.adbPort {
			server = &ls[i]
			break
		}
	}
	if server == nil {
		c.L(fmt.Sprintf("端口 %d 上没有 adb server 在监听，跳过（不自己起 server）", c.adbPort))
		return nil
	}
	// the script runs the server's own adb.exe here; the host protocol needs no client at all
	c.L(fmt.Sprintf("adb：按 adb 主机协议直接连 -P %d（pid %d 的 adb server；不调用 adb.exe，不会起 server）", c.adbPort, server.pid))
	liveDir := filepath.Join(c.stage, "live")
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		return err
	}

	targets := append([]liveTarget(nil), c.running...)
	if c.cmdlineUnreadable {
		// fallback: the emulator-<port> serials of the device list, mapped to instance names with the port plan
		c.L("qemu 命令行读不到（多半是 AUTO-MAS 以管理员运行），现场按 adb 设备列表推算")
		res := c.adbDevicesCommand()
		if res.TimedOut {
			c.L("  adb devices：超时（20 秒），已放弃")
		} else {
			found := 0
			for _, line := range splitCRLF(string(res.Stdout)) {
				m := deviceLine.FindStringSubmatch(line)
				if m == nil {
					continue
				}
				found++
				cport, _ := strconv.Atoi(m[1])
				state := m[2]
				known := false
				for _, t := range targets {
					if t.consolePort == cport {
						known = true
					}
				}
				if known {
					continue
				}
				off := cport - c.plan.Base
				if off < 0 || off%root.PortStep != 0 {
					c.L(fmt.Sprintf("  emulator-%d：端口不符合 PORT_BASE=%d、PORT_STEP=%d，推算不出实例，跳过", cport, c.plan.Base, root.PortStep))
					continue
				}
				name := root.AvdNamePrefix + strconv.Itoa(off/root.PortStep)
				source := "PORT_BASE、PORT_STEP 是后端 constants.py 的常量"
				if c.plan.Base != root.DefaultPortBase {
					source = "PORT_BASE 是 mas-avd.json 的 portBase，PORT_STEP 是后端 constants.py 的常量"
				}
				c.L(fmt.Sprintf("  emulator-%[1]d（%[2]s）→ %[3]s：按 (%[1]d - %[4]d) / %[5]d 推算（%[6]s）",
					cport, state, name, c.plan.Base, root.PortStep, source))
				if state != "device" {
					c.L("    状态不是 device，跳过")
					continue
				}
				if !containsFold(c.instances, name) {
					c.L("    本 ModAVD 目录的 avd 下没有这个实例，跳过")
					continue
				}
				targets = append(targets, liveTarget{name: name, consolePort: cport, inferred: true})
			}
			if found == 0 {
				c.L(fmt.Sprintf("  adb devices 里没有 emulator-* 设备（退出码 %d）", res.ExitCode))
			}
		}
	}
	if len(targets) == 0 {
		c.L("没有可采集的实例，跳过")
		return nil
	}

	proxied := appProxyInstances(filepath.Join(c.root, root.MetadataFile))
	for _, t := range targets {
		if t.consolePort == 0 {
			c.L(t.name + "：命令行里没有 -port / -ports，取不到序列号，跳过")
			continue
		}
		serial := "emulator-" + strconv.Itoa(t.consolePort)
		if t.inferred {
			c.L(fmt.Sprintf("%s（%s，实例名是推算的）：", t.name, serial))
		} else {
			c.L(fmt.Sprintf("%s（pid %d，%s）：", t.name, t.pid, serial))
		}
		if err := c.screenshot(filepath.Join(liveDir, "screen-"+t.name+".png"), serial); err != nil {
			c.L("  截图出错：" + errText(err))
		}
		for _, lc := range liveCommands {
			if err := c.liveCommand(liveDir, t.name, serial, lc.tag, lc.kind, lc.args); err != nil {
				c.L(fmt.Sprintf("  %s 出错：%s", lc.tag, errText(err)))
			}
		}
		// not in the script: the per-app proxy's guest state and log, for the instances that have one set
		if proxied[strings.ToLower(t.name)] {
			if err := c.liveCommand(liveDir, t.name, serial, "app-proxy", "shell", appProxyCommand); err != nil {
				c.L("  app-proxy 出错：" + errText(err))
			}
		}
	}
	return nil
}

// appProxyCommand reads the per-app proxy daemon's state and the end of its log, as the shell user (the daemon writes
// them 0644 into a directory modavd made as that user). One argument: adbLive joins a shell command's arguments as
// they are.
var appProxyCommand = []string{"cat " + appproxy.GuestDir + "/" + appproxy.StateName + "; echo; tail -n 200 " +
	appproxy.GuestDir + "/" + appproxy.LogName}

// appProxyInstances are the instances (lower-case names) that have instances.<i>.appProxy in mas-avd.json; none
// when it cannot be read.
func appProxyInstances(metadataPath string) map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(metadataPath)
	if err != nil {
		return out
	}
	v, err := jsonord.Parse(b)
	if err != nil {
		return out
	}
	insts := v.Obj().Get("instances").Obj()
	for _, k := range insts.Keys() {
		i, err := strconv.Atoi(k)
		if err == nil && root.ValidIndex(i) && insts.Get(k).Obj().Has(appproxy.MetaKey) {
			out[strings.ToLower(root.AvdName(i))] = true
		}
	}
	return out
}

// adbDevicesCommand is `adb -P <port> devices`: the list as adb.exe prints it.
func (c *collector) adbDevicesCommand() adbResult {
	ctx, cancel := context.WithTimeout(c.ctx, nativeTimeout)
	defer cancel()
	list, err := adbDevices(ctx, c.adbPort)
	switch {
	case ctx.Err() == context.DeadlineExceeded:
		return adbResult{TimedOut: true}
	case err != nil:
		return adbResult{ExitCode: 1, Stderr: "adb: " + err.Error() + "\n"}
	}
	return adbResult{Stdout: []byte("List of devices attached\n" + list + "\n")}
}

func (c *collector) screenshot(png, serial string) error {
	f, err := os.Create(png)
	if err != nil {
		return err
	}
	res := adbLive(c.ctx, c.adbPort, serial, nativeTimeout, f, "exec-out", "screencap", "-p")
	if err := f.Close(); err != nil {
		return err
	}
	fi, err := os.Stat(png)
	if err != nil {
		return err
	}
	ok := false
	if fi.Size() > 8 {
		if b, err := os.ReadFile(png); err == nil {
			ok = bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G'})
		}
	}
	switch {
	case res.TimedOut:
		c.L("  截图：超时（20 秒），已放弃")
	case ok:
		c.L("  截图：" + formatSize(float64(fi.Size())))
	default:
		c.L(fmt.Sprintf("  截图：失败，退出码 %d %s", res.ExitCode, netTrim(res.Stderr)))
	}
	if !ok {
		_ = os.Remove(png)
	}
	return nil
}

// adbTextMode is what adb.exe makes of the guest's output on Windows: it writes stdout and stderr through the C
// runtime's text mode, which turns every LF into CRLF (a CRLF the guest sent becomes CR CR LF). The script gets that;
// the host protocol gets the guest's bytes as they are.
func adbTextMode(s string) string { return strings.ReplaceAll(s, "\n", "\r\n") }

func (c *collector) liveCommand(liveDir, name, serial, tag, kind string, args []string) error {
	res := adbLive(c.ctx, c.adbPort, serial, nativeTimeout, nil, kind, args...)
	if res.TimedOut {
		c.L(fmt.Sprintf("  %s：超时（20 秒），已放弃", tag))
		return nil
	}
	res.Stderr = adbTextMode(res.Stderr)
	text := adbTextMode(decodeText(res.Stdout))
	if tag == "window-focus" {
		var keep []string
		for _, l := range splitCRLF(text) {
			if focusLine.MatchString(l) {
				keep = append(keep, netTrim(l))
			}
		}
		text = strings.Join(keep, "\r\n")
		for _, l := range keep {
			if l != "" {
				c.L("  " + l)
			}
		}
	}
	if netTrim(res.Stderr) != "" {
		text += "\r\n# stderr:\r\n" + res.Stderr
	}
	if err := os.WriteFile(filepath.Join(liveDir, name+"-"+tag+".txt"), []byte(text), 0o644); err != nil {
		return err
	}
	c.L(fmt.Sprintf("  %s：退出码 %d，%d 字符", tag, res.ExitCode, utf16Len(text)))
	return nil
}

// ---- 6. 打包 ----------------------------------------------------------------------------------------------------

// zipStage writes every file of the stage into dst: summary.txt first, then by entry name (Sort-Object order),
// names relative to the stage with '/'. Disk images, snapshots, user data and AUTO-MAS config / data are never
// staged; if one ever were, it is left out here too.
func zipStage(dst, stage string) ([]string, error) {
	type item struct{ path, name string }
	var files []item
	queue := []item{{stage, ""}}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		ents, err := listDir(it.path, true)
		if err != nil {
			return nil, err
		}
		for _, e := range ents {
			if !e.IsDir() {
				files = append(files, item{filepath.Join(it.path, e.Name()), it.name + e.Name()})
			}
		}
		for _, e := range ents {
			if e.IsDir() {
				queue = append(queue, item{filepath.Join(it.path, e.Name()), it.name + e.Name() + "/"})
			}
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		si, sj := files[i].name == "summary.txt", files[j].name == "summary.txt"
		if si != sj {
			return si
		}
		return cultureCompare(files[i].name, files[j].name) < 0
	})
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(f)
	var names []string
	for _, it := range files {
		if forbiddenEntry(it.name) {
			continue
		}
		if err = addZipFile(zw, it.path, it.name); err != nil {
			break
		}
		names = append(names, it.name)
	}
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return nil, err
	}
	return names, nil
}

func addZipFile(zw *zip.Writer, path, name string) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	h, err := zip.FileInfoHeader(fi)
	if err != nil {
		return err
	}
	h.Name, h.Method = name, zip.Deflate
	w, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, in)
	return err
}

// forbiddenEntry: disk images (*.img, *.qcow2), snapshots, user data and AUTO-MAS's config\ and data\.
func forbiddenEntry(name string) bool {
	l := strings.ToLower(name)
	if strings.HasSuffix(l, ".img") || strings.HasSuffix(l, ".qcow2") || strings.HasPrefix(l, "mas/config/") ||
		strings.HasPrefix(l, "mas/data/") {
		return true
	}
	for _, p := range strings.Split(l, "/") {
		if p == "snapshots" || strings.HasPrefix(p, "userdata") {
			return true
		}
	}
	return false
}
