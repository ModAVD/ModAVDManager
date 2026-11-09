//go:build darwin && arm64 && cgo

package report

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/buildinfo"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/macos"
	"modavd/internal/root"
)

const (
	darwinTextLimit   = 2 << 20
	darwinTotalLimit  = 20 << 20
	darwinPrivacyNote = "隐私提示：问题包只保存在本机，管理器和命令行都不会上传它。\n" +
		"包含硬件、macOS / HVF / Metal、组件版本、实例配置白名单和经过脱敏的本目录模拟器日志。" +
		"不会收集磁盘镜像、截图、账号配置、进程环境、系统崩溃转储或其他应用日志。\n" +
		"常见凭据字段、路径和设备标识会被过滤；上传前仍请检查全部内容。"
)

func (*macHost) configuration(ctx context.Context, r *root.Root) *ConfigResult {
	return buildDarwinConfig(ctx, r, &macHost{})
}

// readDarwinText has a bounded source and never follows links below the chosen
// root. A growing log cannot expand the planned read or move its tail snapshot.
func readDarwinText(rootDir, relative string, tail bool) ([]byte, error) {
	f, err := macos.OpenDiagnostic(rootDir, relative, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !tail && info.Size() > darwinTextLimit {
		return nil, errors.New("诊断文件超过 2 MiB")
	}
	start := int64(0)
	if tail && info.Size() > darwinTextLimit {
		start = info.Size() - darwinTextLimit
	}
	data, err := io.ReadAll(io.NewSectionReader(f, start, min(info.Size()-start, darwinTextLimit)))
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, errors.New("跳过二进制诊断文件")
	}
	if start > 0 {
		// A tail can begin inside a credential-bearing line. Drop that partial
		// line so its missing field name cannot bypass the line filter.
		boundary := bytes.IndexByte(data, '\n')
		if boundary < 0 {
			return nil, nil
		}
		data = data[boundary+1:]
	}
	return data, nil
}

type darwinSettings struct {
	object  *jsonord.Obj
	runtime *root.Root
	problem string
}

func readDarwinSettings(r *root.Root) darwinSettings {
	data, err := readDarwinText(r.Dir, root.MetadataFile, false)
	if os.IsNotExist(err) {
		data, err = []byte("{}"), nil
	}
	if err != nil {
		return darwinSettings{problem: "实例元数据不可读或存在链接"}
	}
	v, err := jsonord.Parse(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}))
	if err != nil || v.Obj() == nil {
		return darwinSettings{problem: "实例元数据格式不支持"}
	}
	cfg := v.Obj()
	plan, problem := root.PortPlanOf(cfg)
	if problem != "" {
		return darwinSettings{object: cfg, problem: "实例端口配置不合规"}
	}
	private, script := root.DefaultAdbServerPort, root.DefaultScriptAdbServerPort
	for _, item := range []struct {
		key   string
		value *int
	}{{"adbServerPort", &private}, {"scriptAdbServerPort", &script}} {
		if cfg.Has(item.key) {
			p, ok := cfg.Get(item.key).Int()
			if !ok || p < 1024 || p > 65535 || p == 5037 || plan.InstancePorts()[int(p)] {
				return darwinSettings{object: cfg, problem: "私有 ADB 端口配置不合规"}
			}
			*item.value = int(p)
		}
	}
	if private == script && !cfg.Has("scriptAdbServerPort") {
		script = root.FallbackScriptAdbServerPort
	}
	sdk := sdkRootOf(cfg)
	if cfg.Has("sdkRoot") && cfg.Get("sdkRoot").Kind() != jsonord.Null {
		if _, ok := cfg.Get("sdkRoot").Str(); !ok {
			return darwinSettings{object: cfg, problem: "运行 SDK 配置不合规"}
		}
	}
	if sdk != "" {
		sdk = absUnder(r.Dir, sdk)
	}
	frozen, err := r.WithRuntime(root.RuntimeConfig{SDKRoot: sdk, PortBase: plan.Base, AdbServerPort: private, ScriptAdbServerPort: script})
	if err != nil {
		return darwinSettings{object: cfg, problem: "无法固定本目录的运行目标"}
	}
	return darwinSettings{object: cfg, runtime: frozen}
}

func buildDarwinConfig(ctx context.Context, r *root.Root, h Host) *ConfigResult {
	sanitize := darwinSanitizer(r.Dir)
	res := &ConfigResult{CPUs: []string{}, GPUs: []string{}, Instances: []ConfigInstance{}, EmulatorBuild: "读不到"}
	lines := []string{configTitle}
	if info, err := h.OS(ctx); err == nil {
		res.System = fmt.Sprintf("%s %s（%s，%s）", info.Caption, info.Version, info.BuildNumber, info.OSArchitecture)
		res.Memory = fmt.Sprintf("%.0f GiB（可回收及空闲 %.1f GiB）", info.TotalVisibleKB/(1<<20), info.FreePhysicalKB/(1<<20))
	} else {
		res.System, res.Memory = "读不到（"+errText(err)+"）", "读不到"
	}
	lines = append(lines, "系统："+res.System)
	if cpus, err := h.Processors(ctx); err == nil {
		for _, cpu := range cpus {
			res.CPUs = append(res.CPUs, fmt.Sprintf("%s（%s 物理核，%s 逻辑核）", cpu.Name, optInt(cpu.Cores), optInt(cpu.Threads)))
		}
	} else {
		res.CPUs = append(res.CPUs, "读不到（"+errText(err)+"）")
	}
	for _, cpu := range res.CPUs {
		lines = append(lines, "CPU："+cpu)
	}
	lines = append(lines, "内存："+res.Memory)
	if gpus, err := h.VideoControllers(ctx); err == nil {
		for _, gpu := range gpus {
			res.GPUs = append(res.GPUs, gpu.Name+"（宿主 Metal）")
		}
		if len(gpus) == 0 {
			res.GPUs = append(res.GPUs, "未报告支持 Metal 的显卡")
		}
	} else {
		res.GPUs = append(res.GPUs, "读不到（"+errText(err)+"）")
	}
	for _, gpu := range res.GPUs {
		lines = append(lines, "显卡："+gpu)
	}
	lines = append(lines, "图形说明：宿主 Metal 支持不代表模拟器 Vulkan 或游戏兼容性已通过验证")
	res.Acceleration = "HVF 支持未知"
	if native, ok := h.(interface {
		HVFSupport(context.Context) (bool, error)
	}); ok {
		if supported, err := native.HVFSupport(ctx); err == nil {
			res.Acceleration = "Hypervisor.framework（HVF）内核支持：" + optBool(&supported)
		}
	}
	settings := readDarwinSettings(r)
	if settings.runtime != nil {
		runtimeRoot := settings.runtime
		if props, err := readDarwinText(runtimeRoot.RuntimeSDK(), "emulator/source.properties", false); err == nil {
			_, values := root.ReadKeyValues(props, false)
			res.EmulatorBuild = values["Pkg.BuildId"]
			if res.EmulatorBuild == "" {
				res.EmulatorBuild = "读不到"
			}
			res.Emulator = "Pkg.Revision=" + values["Pkg.Revision"] + "，Build=" + res.EmulatorBuild
		} else {
			res.Emulator = "组件版本读不到"
		}
		if check, err := h.Run(ctx, runtimeRoot.EmulatorExe(), []string{"-accel-check"}, 20*time.Second); err == nil {
			if check.TimedOut {
				res.Acceleration += "；模拟器检查超时"
			} else {
				res.Acceleration += fmt.Sprintf("；模拟器检查退出码 %d：%s", check.ExitCode, collapseSpace(string(check.Stdout)+" "+string(check.Stderr)))
			}
		} else {
			res.Acceleration += "；模拟器检查未完成"
		}
	} else {
		res.Emulator = "读不到（" + settings.problem + "）"
	}
	lines = append(lines, "模拟器："+res.Emulator, "硬件加速："+res.Acceleration)
	instances := darwinInstances(r)
	for _, index := range instances {
		instance := ConfigInstance{Index: index, Games: []string{}}
		if settings.object != nil {
			if value := getCI(getCI(settings.object, "instances").Obj(), strconv.Itoa(index)); value != nil {
				instance.MemoryMB, _ = psDouble(getCI(value.Obj(), "memoryMb"))
			}
		}
		if settings.runtime != nil {
			frozen := settings.runtime
			p, owned := adb.QemuOnAdbPort(frozen, frozen.Ports().Console(index))
			instance.Running = owned
			if owned && adb.ExistingServerOwned(frozen) {
				instance.Games = darwinGames(ctx, frozen, index, p.PID, p.Created)
			}
		}
		state := "未运行或归属未确认"
		if instance.Running {
			state = "运行中"
		}
		if len(instance.Games) > 0 {
			state += "；" + strings.Join(instance.Games, "、")
		}
		lines = append(lines, fmt.Sprintf("实例 %d（%.0f MiB）：%s", index, instance.MemoryMB, state))
		res.Instances = append(res.Instances, instance)
	}
	if len(instances) == 0 {
		lines = append(lines, "实例：没有找到本目录的有效实例配置")
	}
	lines = append(lines, "效果（能跑 / 有问题，问题是…）：")
	for n := range lines {
		lines[n] = sanitize(lines[n])
	}
	res.Lines, res.Text = lines, strings.Join(lines, "\n")
	// Structured fields are exposed by --json and must follow the same filtering.
	res.System, res.Memory, res.Emulator, res.EmulatorBuild, res.Acceleration = sanitize(res.System), sanitize(res.Memory), sanitize(res.Emulator), sanitize(res.EmulatorBuild), sanitize(res.Acceleration)
	for n := range res.CPUs {
		res.CPUs[n] = sanitize(res.CPUs[n])
	}
	for n := range res.GPUs {
		res.GPUs[n] = sanitize(res.GPUs[n])
	}
	for n := range res.Instances {
		for g := range res.Instances[n].Games {
			res.Instances[n].Games[g] = sanitize(res.Instances[n].Games[g])
		}
	}
	return res
}

func darwinInstances(r *root.Root) []int {
	var out []int
	for i := 0; i <= root.MaxIndex; i++ {
		if !root.ValidIndex(i) {
			continue
		}
		data, err := readDarwinText(r.Dir, filepath.Join("avd", root.AvdName(i)+".avd", "config.ini"), false)
		if err != nil {
			continue
		}
		_, values := root.ReadKeyValues(data, false)
		if values["AvdId"] == root.AvdName(i) {
			out = append(out, i)
		}
	}
	return out
}

var darwinPackage = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*(?:\.[A-Za-z][A-Za-z0-9_]*)+$`)

func darwinGames(ctx context.Context, r *root.Root, index int, pid uint32, created uint64) []string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	query := func(command string) (string, error) {
		p, owned := adb.QemuOnAdbPort(r, r.Ports().Console(index))
		if !owned || p.PID != pid || p.Created != created || !adb.ExistingServerOwned(r) {
			return "", errors.New("运行归属已变化")
		}
		a, err := dialAdb(ctx, r.AdbServerPort())
		if err != nil {
			return "", err
		}
		defer a.Close()
		if err := a.request("host:transport:" + r.Ports().Serial(index)); err != nil {
			return "", err
		}
		if err := a.request("shell:" + command); err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(a, darwinTextLimit+1))
		if err != nil || len(data) > darwinTextLimit {
			return "", errors.New("安卓状态不可读或超过限制")
		}
		p, owned = adb.QemuOnAdbPort(r, r.Ports().Console(index))
		if !owned || p.PID != pid || p.Created != created || !adb.ExistingServerOwned(r) {
			return "", errors.New("运行归属已变化")
		}
		return strings.TrimSpace(string(data)), nil
	}
	boot, err := query("getprop sys.boot_completed")
	if err != nil || boot != "1" {
		return []string{"游戏列表读不到或尚未开完机"}
	}
	packages, err := query("pm list packages -3")
	if err != nil {
		return []string{"游戏列表读不到"}
	}
	var result []string
	for _, line := range strings.Split(packages, "\n") {
		pkg, ok := strings.CutPrefix(strings.TrimSpace(line), "package:")
		if !ok || len(pkg) > 255 || !darwinPackage.MatchString(pkg) || containsFold(notGames, pkg) || pkg == "org.modavd.ime" {
			continue
		}
		if len(result) == 100 {
			result = append(result, "[其余应用已省略]")
			break
		}
		name := pkg
		if label, ok := GameName(pkg); ok {
			name = label
		}
		version, err := query("dumpsys package " + pkg + " 2>/dev/null | grep -m1 versionName=")
		if err != nil {
			return []string{"游戏列表读取期间归属变化或查询失败"}
		}
		if _, value, ok := strings.Cut(version, "versionName="); ok {
			value = strings.TrimSpace(value)
			if len(value) <= 128 {
				name += " " + value
			}
		}
		result = append(result, name)
	}
	if len(result) == 0 {
		result = []string{"没装"}
	}
	sort.Strings(result)
	return result
}

func (*macHost) collect(ctx context.Context, r *root.Root, opt CollectOptions) (*CollectResult, error) {
	return collectDarwin(ctx, r, &macHost{}, opt)
}

var darwinConfigKeys = map[string]bool{
	"AvdId": true, "abi.type": true, "hw.cpu.arch": true, "tag.id": true, "PlayStore.enabled": true,
	"hw.cpu.ncore": true, "hw.ramSize": true, "hw.lcd.width": true, "hw.lcd.height": true,
	"hw.lcd.density": true, "hw.gpu.enabled": true, "hw.gpu.mode": true, "hw.keyboard": true,
	"hw.mainKeys": true, "hw.initialOrientation": true, "disk.dataPartition.size": true,
	"showDeviceFrame": true, "skin.dynamic": true, "fastboot.forceColdBoot": true, "fastboot.forceFastBoot": true,
	"disk.cachePartition.size": true, "hw.audioInput": true, "hw.audioOutput": true,
}

func collectDarwin(ctx context.Context, r *root.Root, h Host, opt CollectOptions) (result *CollectResult, err error) {
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.Canceled, err, "已取消")
	}
	stageParent := opt.StageDir
	if stageParent == "" {
		stageParent = os.TempDir()
	}
	if err := os.MkdirAll(stageParent, 0700); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(stageParent, "modavd-collect-")
	if err != nil {
		return nil, err
	}
	keep := opt.KeepStage
	defer func() {
		if err != nil && ctx.Err() != nil {
			e := errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
			if keep {
				e.WithAdvice("收集到的文件留在：" + stage)
			}
			err = e
		}
		if !keep {
			_ = os.RemoveAll(stage)
		}
	}()
	sanitize := darwinSanitizer(r.Dir)
	var problems, entries []string
	used := 0
	writeClean := func(name string, clean []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(clean) > darwinTextLimit || used+len(clean) > darwinTotalLimit {
			return errors.New("诊断文本预算已用完")
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(stage, name)), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(stage, name), clean, 0600); err != nil {
			return err
		}
		used += len(clean)
		return nil
	}
	write := func(name, text string) error {
		clean := []byte(sanitize(text))
		if len(clean) > darwinTextLimit {
			clean = bytes.ToValidUTF8(clean[:darwinTextLimit-128], []byte("�"))
			clean = append(clean, []byte("\n[脱敏输出达到 2 MiB，已截断]\n")...)
		}
		return writeClean(name, clean)
	}
	progress := func(message string) {
		if opt.Progress != nil {
			opt.Progress(message)
		}
	}
	progress("正在收集：macOS / CPU / 内存 / HVF / Metal")
	config := buildDarwinConfig(ctx, r, h)
	lines := []string{"modavd：" + buildinfo.Version, config.Text}
	if drive, err := h.Drive(r.Dir); err == nil {
		lines = append(lines, fmt.Sprintf("实例所在卷：%s，剩余 %s / 共 %s", drive.Format, formatSize(float64(drive.Free)), formatSize(float64(drive.Total))))
	}
	settings := readDarwinSettings(r)
	if settings.problem != "" {
		problems = append(problems, settings.problem)
	}
	if settings.runtime != nil {
		progress("正在收集：组件版本")
		for _, component := range append(root.DarwinComponents(), root.PlayStoreComponent()) {
			name := filepath.Join(component.Target, "source.properties")
			data, err := readDarwinText(settings.runtime.RuntimeSDK(), name, false)
			if err != nil {
				lines = append(lines, "组件 "+component.ID+"：未安装或元数据不可读")
				continue
			}
			_, props := root.ReadKeyValues(data, false)
			var values []string
			for _, key := range []string{"Pkg.Revision", "Pkg.BuildId", "AndroidVersion.ApiLevel", "SystemImage.Abi", "SystemImage.TagId"} {
				if value := props[key]; value != "" {
					values = append(values, key+"="+value)
				}
			}
			lines = append(lines, "组件 "+component.ID+"："+strings.Join(values, "，"))
		}
	}
	instances := darwinInstances(r)
	progress("正在收集：实例配置白名单")
	for _, index := range instances {
		for _, name := range []string{"config.ini", "hardware-qemu.ini", "emulator-user.ini"} {
			rel := filepath.Join("avd", root.AvdName(index)+".avd", name)
			data, err := readDarwinText(r.Dir, rel, false)
			if err != nil {
				continue
			}
			_, props := root.ReadKeyValues(data, false)
			var allowed []string
			for key, value := range props {
				if darwinConfigKeys[key] {
					allowed = append(allowed, key+"="+value)
				}
			}
			sort.Strings(allowed)
			if err := write(filepath.ToSlash(rel)+".txt", strings.Join(allowed, "\n")); err != nil {
				return nil, err
			}
		}
	}
	progress("正在收集：本目录模拟器日志（脱敏文本）")
	var logs []string
	for _, index := range instances {
		for _, name := range []string{"emulator.log", "qemu.log", "launch.log"} {
			logs = append(logs, filepath.Join("avd", root.AvdName(index)+".avd", name))
		}
	}
	if dir, err := macos.OpenDiagnostic(r.Dir, "logs", true); err == nil {
		files, readErr := dir.ReadDir(4096)
		dir.Close()
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			problems = append(problems, "日志目录无法完整读取")
		}
		for _, file := range files {
			if file.Type().IsRegular() && strings.HasSuffix(file.Name(), ".log") {
				for _, index := range instances {
					if strings.HasPrefix(file.Name(), root.AvdName(index)+"-") {
						logs = append(logs, filepath.Join("logs", file.Name()))
						break
					}
				}
			}
		}
	}
	type logCandidate struct {
		path     string
		modified time.Time
	}
	var candidates []logCandidate
	for _, relative := range logs {
		f, err := macos.OpenDiagnostic(r.Dir, relative, false)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			problems = append(problems, "一个模拟器日志因类型、链接或权限被跳过")
			continue
		}
		info, err := f.Stat()
		f.Close()
		if err == nil {
			candidates = append(candidates, logCandidate{relative, info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].modified.Equal(candidates[j].modified) {
			return candidates[i].path < candidates[j].path
		}
		return candidates[i].modified.After(candidates[j].modified)
	})
	copied := 0
	for n := 0; n < len(candidates) && copied < 8; n++ {
		if err := ctx.Err(); err != nil {
			return nil, errs.Wrap(errs.Canceled, err, "已取消")
		}
		data, err := readDarwinText(r.Dir, candidates[n].path, true)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			problems = append(problems, "一个模拟器日志因类型、链接或权限被跳过")
			continue
		}
		if err := write(fmt.Sprintf("logs/emulator-%02d.txt", copied), string(data)); err != nil {
			return nil, err
		}
		copied++
	}
	lines = append(lines, fmt.Sprintf("已收集本目录模拟器日志：%d 份（每份最多 2 MiB，最多 8 份）", copied))
	if len(problems) > 0 {
		lines = append(lines, "未完成项目："+strings.Join(problems, "；"))
	}
	summary := "ModAVD macOS 问题信息\n" + darwinPrivacyNote + "\n" + sanitize(strings.Join(lines, "\n")) + "\n"
	if err := writeClean("summary.txt", []byte(summary)); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.Canceled, err, "已取消")
	}
	out := opt.OutDir
	if out == "" {
		out, err = desktopDir()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(out, 0700); err != nil {
		return nil, err
	}
	reserved, err := os.CreateTemp(out, collectZipStart+time.Now().Format("20060102-150405")+"-*.zip")
	if err != nil {
		return nil, err
	}
	zipPath := reserved.Name()
	reservedInfo, err := reserved.Stat()
	if err != nil {
		reserved.Close()
		os.Remove(zipPath)
		return nil, err
	}
	progress("正在打包：已脱敏的本地问题信息")
	entries, err = zipStageWithFile(ctx, zipPath, stage, reserved)
	if err != nil {
		os.Remove(zipPath)
		return nil, err
	}
	info, err := os.Lstat(zipPath)
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(reservedInfo, info) {
		return nil, errors.New("问题包在生成过程中发生替换")
	}
	res := &CollectResult{ZipPath: zipPath, ZipBytes: info.Size(), ZipSize: formatSize(float64(info.Size())), Entries: entries,
		Stage: stage, StageKept: keep, Problems: problems, Summary: summary, PrivacyNote: darwinPrivacyNote}
	if !keep {
		if err := os.RemoveAll(stage); err != nil {
			keep, res.StageKept, res.StageError = true, true, err.Error()
		}
	}
	if !opt.NoExplorer {
		_ = openExplorer(zipPath)
	}
	return res, nil
}
