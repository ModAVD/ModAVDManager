//go:build windows

package emu

// Host checks before a boot. Port of AUTO-MAS precheck.py (+ host.py check_acceleration / parse_accel_check):
// directory path, components, emulator version, hardware virtualization, Vulkan, disk, memory. Each item has ok,
// reason and advice; a boot refuses on the blocking ones that fail. Read only: nothing on the host is changed.
//
//   - path: no non-ASCII characters in the root (emulator and qemu are unreliable there); spaces are fine. Blocking.
//   - components: platform tools and the system image (fetched by 补全组件.bat). Blocking.
//   - emulator: only our own build from the ModAVD package, with Pkg.BuildId=mas-N >= MinModBuild. Blocking.
//   - acceleration: emulator -accel-check (WHPX). Blocking.
//   - memory: available host memory >= this boot's -memory + 1.5 GB. Blocking.
//   - disk: free space on the instances' volume >= 8 GB (data disks are sparse and grow). Blocking.
//   - vulkan: a child process loads vulkan-1.dll and lists the devices. Only a warning: without hardware Vulkan the
//     emulator falls back to SwiftShader, boots and renders correctly, only slowly; the probe can also be wrong
//     (loader trouble, remote desktop sessions), and blocking would stop machines that work.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

const (
	accelAdvice = "先在 BIOS/UEFI 里打开 VT-x/SVM，再在「启用或关闭 Windows 功能」里勾选" +
		"「Windows 虚拟机监控程序平台」，然后重启"
	vulkanAdvice = "安装或更新显卡驱动（NVIDIA / AMD / Intel 官网驱动自带 Vulkan）。没有可用的显卡时模拟器只能" +
		"软件渲染，游戏会占满 CPU，性能会很差"
	testPackageAdvice = `请把魔改 AVD 内测包解压到这个目录（解压后目录里应有 sdk\emulator）`

	// How long a result of the same check is reused within one process (MCP server): AUTO-MAS
	// _ACCEL_CACHE_SECONDS, VULKAN_PROBE_CACHE_SECONDS, VULKAN_PROBE_TIMEOUT_CACHE_SECONDS.
	accelCacheFor         = 120 * time.Second
	vulkanCacheFor        = 600 * time.Second
	vulkanTimeoutCacheFor = 60 * time.Second
)

// Check runs all host checks (path, components, emulator, acceleration, vulkan, disk, memory).
func (m *Manager) Check(ctx context.Context, opt CheckOptions) (*CheckResult, error) {
	memoryMB := opt.MemoryMB
	if memoryMB <= 0 {
		memoryMB = avd.DefaultMemoryMB
	}
	comps := m.Root.Components()

	// The two slow ones (a child process each) run side by side, like AUTO-MAS run_prechecks.
	var accel, vulkan CheckItem
	var wg sync.WaitGroup
	wg.Go(func() { accel = m.accelerationItem(ctx, !opt.Refresh) })
	wg.Go(func() { vulkan = vulkanItemFromProbe(m.probeVulkan(ctx, !opt.Refresh)) })
	wg.Wait()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}

	items := []CheckItem{
		pathItem(m.Root.Dir),
		componentsItem(comps, m.missingAdvice),
		emulatorItem(m.emulatorFacts()),
		accel,
		vulkan,
		m.diskItemNow(),
		memoryItemNow(memoryMB),
	}
	res := &CheckResult{RootInfo: m.rootInfo(), Ready: true, Items: items, Components: []ComponentStatus{}}
	if _, failed := firstBlockingFailure(items); failed {
		res.Ready = false
	}
	for _, c := range comps {
		res.Components = append(res.Components, ComponentStatus{
			ID: c.ID, Name: c.Name, Version: c.Version, Build: c.Build,
			Installed: c.Installed, Optional: c.Optional, LocalSDK: c.LocalSDK,
		})
	}
	return res, nil
}

// precheckBeforeLaunch: the blocking checks before a boot (path, components, emulator, acceleration, memory against
// memoryMB, disk), errs.PrecheckFailed / errs.NotReady for the first failing one; vulkan only warns. memoryMB must be
// the -memory of this boot. Order and messages as AUTO-MAS check_before_launch.
func (m *Manager) precheckBeforeLaunch(ctx context.Context, memoryMB int) error {
	comps := m.Root.Components()
	items := []CheckItem{
		pathItem(m.Root.Dir),
		componentsItem(comps, m.missingAdvice),
		emulatorItem(m.emulatorFacts()),
		m.accelerationItem(ctx, true),
		memoryItemNow(memoryMB),
		m.diskItemNow(),
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	// Components that are not installed (ok == nil) are stopped by the components item first; only clear failures
	// stop the boot.
	if bad, failed := firstBlockingFailure(items); failed {
		return precheckError(bad, func() error {
			// MissingError returns *errs.Error: keep a nil pointer from becoming a non-nil error.
			if e := m.Root.MissingError(requiredMissing(comps)); e != nil {
				return e
			}
			return nil
		})
	}
	vulkan := vulkanItemFromProbe(m.probeVulkan(ctx, true))
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if vulkan.OK != nil && !*vulkan.OK {
		m.Log.Warnf("魔改 AVD 开机前检查：%s", itemMessage(vulkan))
	}
	items = append(items, vulkan)
	m.Log.Infof("%s", precheckSummary(items))
	return nil
}

// ---- item builders (pure, so they are testable with injected values) ---------------------------------------------

func checkOK(b bool) *bool { return &b }

// itemMessage is reason + "。" + advice (AUTO-MAS PrecheckItem.message, same as errs.Error.Error()).
func itemMessage(it CheckItem) string {
	if it.Advice != "" {
		return it.Reason + "。" + it.Advice
	}
	return it.Reason
}

// firstBlockingFailure is the first item with blocking && ok == false.
func firstBlockingFailure(items []CheckItem) (CheckItem, bool) {
	for _, it := range items {
		if it.Blocking && it.OK != nil && !*it.OK {
			return it, true
		}
	}
	return CheckItem{}, false
}

// precheckError maps a failed blocking item to the error a start returns: components -> the root's NotReady error
// (with the package / 补全组件.bat advice), emulator -> NotReady, the rest -> PrecheckFailed.
func precheckError(it CheckItem, missingErr func() error) error {
	switch it.ID {
	case "components":
		if err := missingErr(); err != nil {
			return err
		}
		return errs.New(errs.NotReady, it.Reason).WithAdvice(it.Advice)
	case "emulator":
		return errs.New(errs.NotReady, it.Reason).WithAdvice(it.Advice)
	default:
		return errs.New(errs.PrecheckFailed, it.Reason).WithAdvice(it.Advice)
	}
}

// precheckSummary is AUTO-MAS's one log line: "魔改 AVD 开机前检查：目录路径通过（<根目录>）；组件通过（...）；...".
func precheckSummary(items []CheckItem) string {
	parts := make([]string, len(items))
	for k, it := range items {
		verdict := "未查"
		if it.OK != nil {
			verdict = "未通过"
			if *it.OK {
				verdict = "通过"
			}
		}
		parts[k] = fmt.Sprintf("%s%s（%s）", it.Title, verdict, it.Reason)
	}
	return "魔改 AVD 开机前检查：" + strings.Join(parts, "；")
}

func pathIsASCII(s string) bool {
	for k := 0; k < len(s); k++ {
		if s[k] >= 0x80 {
			return false
		}
	}
	return true
}

func pathItem(dir string) CheckItem {
	if pathIsASCII(dir) {
		return CheckItem{ID: "path", Title: "目录路径", OK: checkOK(true), Blocking: true, Reason: dir}
	}
	return CheckItem{
		ID: "path", Title: "目录路径", OK: checkOK(false), Blocking: true,
		Reason: fmt.Sprintf("模拟器目录路径里有中文等非英文字符（%s），模拟器在这类路径下不可靠", dir),
		Advice: "把模拟器目录放到不含中文的路径下",
	}
}

// requiredComponents are the components of the components item: required ones except the emulator, which has its
// own item (AUTO-MAS REQUIRED_COMPONENTS = platform tools, system image).
func requiredComponents(comps []root.ComponentStatus) []root.ComponentStatus {
	var out []root.ComponentStatus
	for _, c := range comps {
		if !c.Optional && c.ID != "emulator" {
			out = append(out, c)
		}
	}
	return out
}

func requiredMissing(comps []root.ComponentStatus) []string {
	var missing []string
	for _, c := range requiredComponents(comps) {
		if !c.Installed {
			missing = append(missing, c.Name)
		}
	}
	return missing
}

// componentsItem: advice gives the advice for the missing names.
func componentsItem(comps []root.ComponentStatus, advice func(missing []string) string) CheckItem {
	required := requiredComponents(comps)
	missing := requiredMissing(comps)
	if len(missing) == 0 {
		names := make([]string, len(required))
		for k, c := range required {
			names[k] = c.Name
		}
		return CheckItem{ID: "components", Title: "组件", OK: checkOK(true), Blocking: true, Reason: strings.Join(names, "、")}
	}
	return CheckItem{
		ID: "components", Title: "组件", OK: checkOK(false), Blocking: true,
		Reason: "这个目录里缺少：" + strings.Join(missing, "、"),
		Advice: advice(missing),
	}
}

// missingAdvice is the root's advice for missing components (package / 补全组件.bat).
func (m *Manager) missingAdvice(missing []string) string {
	if e := m.Root.MissingError(missing); e != nil && e.Advice != "" {
		return e.Advice
	}
	return root.FetchComponentsAdvice
}

// emulatorFacts is what emulatorItem judges.
type emulatorFacts struct {
	Present   bool   // the key files are there (any build)
	SelfBuilt bool   // our own qemu (root.SelfBuiltMark)
	Version   string // Pkg.Revision, "" when unknown
	Shown     string // the ModAVD version for people ("0.35.0"), "" when unknown
	Build     int    // N of Pkg.BuildId=mas-N
	HasBuild  bool
}

func (m *Manager) emulatorFacts() emulatorFacts {
	f := emulatorFacts{
		Present:   m.Root.EmulatorPresent(),
		SelfBuilt: m.Root.EmulatorSelfBuilt(),
		Version:   m.Root.EmulatorVersion(),
		Shown:     m.Root.VersionText(),
	}
	f.Build, f.HasBuild = m.Root.ModBuild()
	return f
}

// emulatorItem: only the self-built emulator of the ModAVD package (user decision: Google's builds are not
// supported), and its build must be new enough. Blocking.
func emulatorItem(f emulatorFacts) CheckItem {
	const title = "模拟器版本"
	if !f.Present {
		return CheckItem{
			ID: "emulator", Title: title, OK: checkOK(false), Blocking: true,
			Reason: "这个目录里还没有魔改 AVD 内测包", Advice: testPackageAdvice,
		}
	}
	version := f.Version
	if version == "" {
		version = "版本未知"
	}
	if !f.SelfBuilt {
		return CheckItem{
			ID: "emulator", Title: title, OK: checkOK(false), Blocking: true,
			Reason: fmt.Sprintf("这里的模拟器（%s）不是魔改 AVD 内测包里的版本，只支持内测包", version),
			Advice: testPackageAdvice,
		}
	}
	if f.HasBuild && f.Build >= root.MinModBuild {
		return CheckItem{
			ID: "emulator", Title: title, OK: checkOK(true), Blocking: true,
			Reason: fmt.Sprintf("内测包 %s（%s）", f.Shown, version),
		}
	}
	shown := f.Shown
	if shown == "" {
		shown = "未标版本号"
	}
	return CheckItem{
		ID: "emulator", Title: title, OK: checkOK(false), Blocking: true,
		Reason: fmt.Sprintf("内测包版本过旧（%s）", shown),
		Advice: fmt.Sprintf("请把魔改 AVD 更新到 0.%d.0 或更新的版本，实例和游戏数据不受影响", root.MinModBuild),
	}
}

// memoryItem: memoryMB is the -memory this boot passes (the instance's own setting).
func memoryItem(memoryMB int, availableMB uint64, err error) CheckItem {
	if err != nil {
		return CheckItem{ID: "memory", Title: "内存", OK: nil, Blocking: true, Reason: fmt.Sprintf("查不了电脑可用内存: %v", err)}
	}
	needed := memoryMB + hostMemoryOverheadMB
	text := fmt.Sprintf("实例内存 %.0f GB，启动约需 %.1f GB，当前可用 %.1f GB",
		float64(memoryMB)/1024, float64(needed)/1024, float64(availableMB)/1024)
	if availableMB >= uint64(needed) {
		return CheckItem{ID: "memory", Title: "内存", OK: checkOK(true), Blocking: true, Reason: text}
	}
	return CheckItem{
		ID: "memory", Title: "内存", OK: checkOK(false), Blocking: true,
		Reason: "电脑可用内存不足：" + text,
		Advice: "请关闭部分程序，或调小实例内存",
	}
}

func memoryItemNow(memoryMB int) CheckItem {
	avail, err := win.AvailableMemoryMB()
	return memoryItem(memoryMB, avail, err)
}

// diskItem: free space of the volume holding <root>\avd (drive like `E:\`).
func diskItem(drive string, free uint64, err error) CheckItem {
	if err != nil {
		return CheckItem{ID: "disk", Title: "磁盘", OK: nil, Blocking: true, Reason: fmt.Sprintf("查不了 %s 的剩余空间: %v", drive, err)}
	}
	text := fmt.Sprintf("实例所在盘 %s 剩余 %.1f GB", drive, float64(free)/(1<<30))
	if free >= minFreeDiskGBToBoot<<30 {
		return CheckItem{ID: "disk", Title: "磁盘", OK: checkOK(true), Blocking: true, Reason: text}
	}
	return CheckItem{
		ID: "disk", Title: "磁盘", OK: checkOK(false), Blocking: true,
		Reason: fmt.Sprintf("%s，低于开机所需的 %d GB：数据盘随游戏写入增长，盘写满时实例数据会损坏", text, minFreeDiskGBToBoot),
		Advice: fmt.Sprintf("清理 %s 的空间，或把魔改 AVD 根目录放到空间更大的盘", drive),
	}
}

func (m *Manager) diskItemNow() CheckItem {
	target := diskExistingAncestor(m.Root.AvdHome())
	drive := diskDrive(target)
	free, err := win.DiskFree(target)
	return diskItem(drive, free, err)
}

// diskExistingAncestor is path or its nearest existing parent.
func diskExistingAncestor(path string) string {
	cur := filepath.Clean(path)
	for !fileOrDirExists(cur) {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return cur
}

// diskDrive is the volume root of path (`E:\`, `\\server\share\`), like Python's Path.anchor; path itself when it has
// no volume.
func diskDrive(path string) string {
	if v := filepath.VolumeName(path); v != "" {
		return v + `\`
	}
	return path
}

// ---- hardware virtualization -------------------------------------------------------------------------------------

type accelResult struct {
	OK     bool
	Detail string
}

var accelCache = struct {
	sync.Mutex
	m map[string]accelCacheEntry
}{m: map[string]accelCacheEntry{}}

type accelCacheEntry struct {
	at  time.Time
	res accelResult
}

func (m *Manager) accelerationItem(ctx context.Context, useCache bool) CheckItem {
	if !fileOrDirExists(m.Root.EmulatorExe()) {
		return CheckItem{ID: "acceleration", Title: "硬件虚拟化", OK: nil, Blocking: true, Reason: "模拟器组件还没装好，装好后再检查"}
	}
	res := m.checkAcceleration(ctx, useCache)
	summary := accelSummary(res.Detail)
	m.Log.Debugf("-accel-check 输出: %s", summary)
	return accelItem(res, summary)
}

func accelItem(res accelResult, summary string) CheckItem {
	if res.OK {
		return CheckItem{ID: "acceleration", Title: "硬件虚拟化", OK: checkOK(true), Blocking: true, Reason: accelOKText(summary)}
	}
	if summary == "" {
		summary = "无输出"
	}
	return CheckItem{
		ID: "acceleration", Title: "硬件虚拟化", OK: checkOK(false), Blocking: true,
		Reason: fmt.Sprintf("硬件虚拟化（Windows 虚拟机监控程序平台）不可用，魔改 AVD 无法启动（检查结果：%s）", summary),
		Advice: accelAdvice,
	}
}

// checkAcceleration is `emulator.exe -accel-check` (AUTO-MAS check_acceleration), cached per root for 120 s: it runs
// before every boot, but several boots within seconds need not start it again.
func (m *Manager) checkAcceleration(ctx context.Context, useCache bool) accelResult {
	key := strings.ToLower(m.Root.Dir)
	if useCache {
		accelCache.Lock()
		e, ok := accelCache.m[key]
		accelCache.Unlock()
		if ok && time.Since(e.at) < accelCacheFor {
			return e.res
		}
	}
	cctx, cancel := context.WithTimeout(ctx, accelCheckTimeout)
	defer cancel()
	exe := m.Root.EmulatorExe()
	cmd := exec.CommandContext(cctx, exe, "-accel-check")
	cmd.Env = m.ADB.Env()
	// Like the boot itself (launch.go): our working directory may be anywhere, the emulator's own is always there.
	cmd.Dir = filepath.Dir(exe)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var res accelResult
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		res = parseAccelCheck(0, adb.DecodeOutput(out.Bytes()))
	case ctx.Err() != nil:
		// The caller gave up: not a result, not cached.
		return accelResult{OK: false, Detail: "已取消"}
	case cctx.Err() != nil:
		res = accelResult{OK: false, Detail: fmt.Sprintf("-accel-check 超时（%.0f 秒）", accelCheckTimeout.Seconds())}
	case errors.As(err, &exitErr):
		res = parseAccelCheck(exitErr.ExitCode(), adb.DecodeOutput(out.Bytes()))
	default:
		res = accelResult{OK: false, Detail: err.Error()}
	}
	accelCache.Lock()
	accelCache.m[key] = accelCacheEntry{at: time.Now(), res: res}
	accelCache.Unlock()
	return res
}

// parseAccelCheck: usable only with exit code 0 and "usable" in the output (AUTO-MAS parse_accel_check).
func parseAccelCheck(code int, output string) accelResult {
	text := strings.TrimSpace(output)
	return accelResult{OK: code == 0 && strings.Contains(strings.ToLower(text), "usable"), Detail: text}
}

// accelSummary drops the frame lines of the -accel-check output ("accel:", the return code, "accel") and keeps the
// explanation (AUTO-MAS _accel_summary).
func accelSummary(detail string) string {
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(detail, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || t == "accel" || t == "accel:" || accelDigitsOnly(strings.TrimLeft(t, "-")) {
			continue
		}
		lines = append(lines, t)
	}
	if len(lines) == 0 {
		return strings.TrimSpace(detail)
	}
	return strings.Join(lines, " ")
}

func accelDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for k := 0; k < len(s); k++ {
		if s[k] < '0' || s[k] > '9' {
			return false
		}
	}
	return true
}

var accelNameVersion = regexp.MustCompile(`(\w+)\s*\(([\d.]+)\)`)

// accelOKText: "WHPX(10.0.26200) is installed and usable." -> "可用（WHPX 10.0.26200）"; just "可用" when the name and
// version cannot be found.
func accelOKText(summary string) string {
	if mm := accelNameVersion.FindStringSubmatch(summary); mm != nil {
		return fmt.Sprintf("可用（%s %s）", mm[1], mm[2])
	}
	return "可用"
}
