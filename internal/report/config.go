package report

// report-config: port of report-config.ps1, the computer and the games of the running instances in a few lines to
// paste into the trial group.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// gameNames: report-config.ps1's $GameNames (package -> Chinese name); unknown packages show as they are.
var gameNames = [][2]string{
	{"com.hypergryph.arknights", "明日方舟（官服）"},
	{"com.hypergryph.arknights.bilibili", "明日方舟（B 服）"},
	{"com.hypergryph.endfield", "明日方舟：终末地"},
	{"com.shenlan.m.reverse1999", "重返未来：1999"},
	{"com.miHoYo.hkrpg", "崩坏：星穹铁道（官服）"},
	{"com.miHoYo.hkrpg.bilibili", "崩坏：星穹铁道（B 服）"},
	{"com.miHoYo.enterprise.NGHSoD", "崩坏3（官服）"},
}

// notGames: $NotGames, apps that come with the emulator.
var notGames = []string{"de.jrpie.android.launcher"}

// The lines the user fills in.
var configTail = []string{"AUTO-MAS 版本：", "用的脚本（MAA / M9A / …）：", "效果（能跑 / 有问题，问题是…）："}

const (
	configTitle    = "【魔改 AVD 试用配置】"
	configNotOpen  = "游戏包：没有实例开着，读不到。在 AUTO-MAS 里开着实例时再双击运行一次，就会列出来"
	emuCheckWait   = 20 * time.Second
	clipboardOK    = "已复制到剪贴板：粘贴到群里，再把最后三行补上就行。"
	clipboardError = "复制到剪贴板失败（%s），请手动选中上面的文字复制。"
)

// ConfigOptions for BuildConfig.
type ConfigOptions struct {
	NoClipboard bool // report-config.ps1 -NoClipboard
	Host        Host // nil: this computer

	clipboard func(string) error // tests: instead of the system clipboard
}

// ConfigResult is the report. Text is what the script prints and copies; the other fields are its parts.
type ConfigResult struct {
	Text  string   `json:"text"`  // the lines joined with CRLF, as copied to the clipboard
	Lines []string `json:"lines"` // the same lines
	// System, Memory, Emulator and Acceleration are their lines without the label; CPUs and GPUs one per line.
	System        string           `json:"system"`
	CPUs          []string         `json:"cpus"`
	Memory        string           `json:"memory"`
	GPUs          []string         `json:"gpus"`
	Emulator      string           `json:"emulator"`
	EmulatorBuild string           `json:"emulator_build"` // Pkg.BuildId, "读不到" when missing
	Acceleration  string           `json:"acceleration"`
	Instances     []ConfigInstance `json:"instances"`
	// Clipboard: copied, failed (ClipboardError says why) or skipped (--no-clipboard).
	Clipboard      string `json:"clipboard"`
	ClipboardError string `json:"clipboard_error"`
	// Message is the line printed after the text ("" with --no-clipboard).
	Message string `json:"message"`
}

// ConfigInstance is one instance of the report: a running one (its games asked over adb) or, when none runs, one
// of mas-avd.json.
type ConfigInstance struct {
	Index    int      `json:"index"`
	MemoryMB float64  `json:"memory_mb"` // mas-avd.json memoryMb, 0 when not set
	Running  bool     `json:"running"`
	Games    []string `json:"games"` // as listed: "名字 版本", or 没装, or 读不到（原因）
}

// BuildConfig is report-config.ps1: prints nothing itself (the CLI prints Text and Message), copies Text to the
// clipboard unless NoClipboard. Read-only; asks only adb servers that already listen (see the package comment).
func BuildConfig(ctx context.Context, r *root.Root, opt ConfigOptions) (*ConfigResult, error) {
	if runtime.GOOS != "windows" {
		return nil, errs.New(errs.Unsupported, "复制配置信息目前只支持 Windows")
	}
	h := opt.Host
	if h == nil {
		var err error
		if h, err = newHost(); err != nil {
			return nil, err
		}
	}
	res := buildConfig(ctx, r.Dir, h)
	if err := ctx.Err(); err != nil {
		// Ctrl+C: what was read by then is incomplete, nothing is copied
		return nil, errs.Wrap(errs.Canceled, err, "已取消")
	}
	if opt.NoClipboard {
		res.Clipboard = "skipped"
		return res, nil
	}
	copyFn := opt.clipboard
	if copyFn == nil {
		copyFn = copyClipboard
	}
	if err := copyFn(res.Text); err != nil {
		res.Clipboard, res.ClipboardError = "failed", errs.As(err).Message
		res.Message = fmt.Sprintf(clipboardError, res.ClipboardError)
	} else {
		res.Clipboard, res.Message = "copied", clipboardOK
	}
	return res, nil
}

// configTarget is a running instance found in an adb server's device list.
type configTarget struct {
	port   int
	serial string
}

var (
	buildIDLine   = regexp.MustCompile(`(?i)^Pkg\.BuildId=(.+)$`)
	accelFrame    = regexp.MustCompile(`(?i)^(accel:?|\p{Nd}+)$`)
	accelUsable   = regexp.MustCompile(`(?i)^(.+?) is installed and usable\.?$`)
	emulatorLine  = regexp.MustCompile(`(?i)^(emulator-([0-9]+))[\s\p{Z}]+device[\s\p{Z}]*$`)
	loopbackLine  = regexp.MustCompile(`(?i)^(127\.0\.0\.1:([0-9]+))[\s\p{Z}]+device[\s\p{Z}]*$`)
	versionPrefix = regexp.MustCompile(`(?i)^[\s\p{Z}]*versionName=`)
)

// buildConfig builds the lines in the script's order.
func buildConfig(ctx context.Context, rootDir string, h Host) *ConfigResult {
	res := &ConfigResult{CPUs: []string{}, GPUs: []string{}, Instances: []ConfigInstance{}}
	var lines []string
	L := func(s string) { lines = append(lines, s) }

	// The disk kind is a PowerShell run of its own; it runs while the rest is read.
	letter := driveLetter(rootDir)
	type kindResult struct{ bus, media string }
	kindCh := make(chan *kindResult, 1)
	go func() {
		if letter == "" {
			kindCh <- nil
			return
		}
		bus, media, err := h.DiskKind(ctx, letter)
		if err != nil {
			kindCh <- nil
			return
		}
		kindCh <- &kindResult{bus, media}
	}()

	// ---- computer
	L(configTitle)
	osInfo, osErr := h.OS(ctx)
	ver := ""
	if cv, err := h.CurrentVersion(); err == nil {
		if cv.DisplayVersion != "" {
			ver = cv.DisplayVersion
		} else if cv.ReleaseID != "" {
			ver = cv.ReleaseID
		}
		ver = netTrim(fmt.Sprintf("%s（%s.%s）", ver, cv.CurrentBuild, cv.UBR))
	}
	if osErr != nil {
		res.System = "读不到（" + osErr.Error() + "）"
	} else {
		res.System = fmt.Sprintf("%s %s", microsoftPrefix.ReplaceAllString(osInfo.Caption, ""), ver)
	}
	L("系统：" + res.System)
	if cpus, err := h.Processors(ctx); err != nil {
		res.CPUs = append(res.CPUs, "读不到（"+err.Error()+"）")
	} else {
		for _, c := range cpus {
			res.CPUs = append(res.CPUs, fmt.Sprintf("%s（%s 核 %s 线程）", collapseSpace(netTrim(c.Name)), optInt(c.Cores), optInt(c.Threads)))
		}
	}
	for _, c := range res.CPUs {
		L("CPU：" + c)
	}
	if osErr != nil {
		res.Memory = "读不到（" + osErr.Error() + "）"
	} else {
		res.Memory = fmt.Sprintf("%s GB（现在可用 %s GB）", formatN(osInfo.TotalVisibleKB/mb, 0), formatN(osInfo.FreePhysicalKB/mb, 0))
	}
	L("内存：" + res.Memory)
	// physical cards only (PCI devices): remote desktop and streaming tools add virtual display adapters
	if all, err := h.VideoControllers(ctx); err != nil {
		res.GPUs = append(res.GPUs, "读不到（"+err.Error()+"）")
	} else {
		var gpus []VideoController
		for _, g := range all {
			if strings.HasPrefix(strings.ToUpper(g.PNPDeviceID), `PCI\`) {
				gpus = append(gpus, g)
			}
		}
		if len(gpus) == 0 {
			gpus = all
		}
		for _, g := range gpus {
			dd := ""
			if g.DriverDate != "" {
				dd = "，" + g.DriverDate
			}
			res.GPUs = append(res.GPUs, fmt.Sprintf("%s（驱动 %s%s）", g.Name, g.DriverVersion, dd))
		}
	}
	for _, g := range res.GPUs {
		L("显卡：" + g)
	}

	// ---- ModAVD
	sdk := filepath.Join(rootDir, root.SDKDir)
	cfg, _ := readConfig(filepath.Join(rootDir, root.MetadataFile))
	if s := sdkRootOf(cfg); s != "" {
		sdk = s
	}
	sdk = absUnder(rootDir, sdk)
	res.EmulatorBuild = "读不到"
	if b, err := root.ReadFileShared(filepath.Join(sdk, "emulator", "source.properties")); err == nil {
		for _, line := range splitNetLines(decodeText(b)) {
			if m := buildIDLine.FindStringSubmatch(line); m != nil {
				res.EmulatorBuild = netTrim(m[1])
			}
		}
	}
	diskText := ""
	if letter != "" {
		if d, err := h.Drive(letter + `:\`); err == nil {
			diskText = fmt.Sprintf("，放在 %s: 盘（剩余 %s GB", letter, formatN(float64(d.Free)/gb, 0))
			if k := <-kindCh; k != nil {
				if kind := diskKindText(k.bus, k.media); kind != "" {
					diskText += "，" + kind
				}
			}
			diskText += "）"
		}
	}
	res.Emulator = res.EmulatorBuild + diskText
	L("模拟器：" + res.Emulator)

	// acceleration as the emulator's own check sees it: Windows feature names are unreliable (with Hyper-V on, the
	// hypervisor platform shows as off and WHPX works anyway)
	res.Acceleration = "读不到"
	if ec := filepath.Join(sdk, "emulator", "emulator-check.exe"); exists(ec) {
		r, err := h.Run(ctx, ec, []string{"accel"}, emuCheckWait)
		switch {
		case err != nil:
			res.Acceleration = "检查失败（" + startErrText(err) + "）"
		case r.TimedOut:
			res.Acceleration = "检查超时"
		default:
			var parts []string
			for _, line := range splitNetLines(decodeText(r.Stdout)) {
				if t := netTrim(line); t != "" && !accelFrame.MatchString(t) {
					parts = append(parts, t)
				}
			}
			msg := strings.Join(parts, " ")
			if m := accelUsable.FindStringSubmatch(msg); m != nil {
				res.Acceleration = m[1] + " 可用"
			} else if msg != "" {
				res.Acceleration = msg
			}
		}
	}
	L("硬件加速：" + res.Acceleration)

	// instance settings (mas-avd.json)
	instCfg := map[string]*jsonord.Value{}
	var instKeys []string
	if v := getCI(cfg, "instances"); truthy(v) {
		o := v.Obj()
		for _, k := range o.Keys() {
			// a hashtable: a key that differs only in case replaces the value under the first spelling
			if k0, dup := keyFold(instCfg, k); dup {
				instCfg[k0] = o.Get(k)
				continue
			}
			instCfg[k] = o.Get(k)
			instKeys = append(instKeys, k)
		}
	}
	memMB := func(idx string) (float64, bool) {
		v, ok := lookupFold(instCfg, idx)
		if !ok {
			return 0, false
		}
		m := getCI(v.Obj(), "memoryMb")
		if !truthy(m) {
			return 0, false
		}
		return psDouble(m)
	}
	mem := func(idx string) string {
		if m, ok := memMB(idx); ok {
			return fmt.Sprintf("，%s GB", formatN(m/1024, 0))
		}
		return ""
	}

	// ---- games of the running instances (only those)
	// The index from a serial is counted in this root's plan (mas-avd.json portBase), so that the production
	// root's instances, also listed by its servers 20050 / 20049, are not taken for this root's.
	plan, _ := root.PortPlanOf(cfg)
	var ports []int
	cfgPort := 0
	if v := getCI(cfg, "adbServerPort"); truthy(v) {
		cfgPort, _ = psInt(v)
	}
	for _, p := range []int{cfgPort, root.DefaultAdbServerPort, root.DefaultScriptAdbServerPort} {
		if p != 0 && p != 5037 && !containsInt(ports, p) {
			ports = append(ports, p)
		}
	}
	listeners, lerr := h.Listeners()
	seen := map[string]configTarget{}
	var seenKeys []string
	for _, port := range ports {
		if lerr != nil || !listening(listeners, port) {
			continue
		}
		list, err := adbDevices(ctx, port)
		if err != nil {
			list = ""
		}
		for _, line := range strings.Split(list, "\n") {
			var serial string
			cport := -1
			if m := emulatorLine.FindStringSubmatch(line); m != nil {
				serial = m[1]
				if n, err := strconv.Atoi(m[2]); err == nil {
					cport = n
				}
			} else if m := loopbackLine.FindStringSubmatch(line); m != nil {
				serial = m[1]
				if n, err := strconv.Atoi(m[2]); err == nil {
					cport = n - 1
				}
			}
			if cport < 0 {
				continue
			}
			idx, ok := configIndex(plan, cport)
			if !ok {
				continue
			}
			if _, ok := seen[idx]; ok {
				continue
			}
			seen[idx] = configTarget{port: port, serial: serial}
			seenKeys = append(seenKeys, idx)
		}
	}

	if len(seen) == 0 {
		names := append([]string(nil), instKeys...)
		sortCulture(names)
		if len(names) > 0 {
			var parts []string
			for _, n := range names {
				parts = append(parts, "实例 "+n+mem(n))
				if i, err := strconv.Atoi(n); err == nil {
					m, _ := memMB(n)
					res.Instances = append(res.Instances, ConfigInstance{Index: i, MemoryMB: m, Games: []string{}})
				}
			}
			L(fmt.Sprintf("实例：%d 个（%s）", len(names), strings.Join(parts, "；")))
		}
		L(configNotOpen)
	} else {
		sortCulture(seenKeys)
		for _, idx := range seenKeys {
			games := gamesOf(ctx, seen[idx])
			L(fmt.Sprintf("实例 %s%s：%s", idx, mem(idx), strings.Join(games, "、")))
			i, _ := strconv.Atoi(idx)
			m, _ := memMB(idx)
			res.Instances = append(res.Instances, ConfigInstance{Index: i, MemoryMB: m, Running: true, Games: games})
		}
	}

	// ---- what the tester fills in
	for _, l := range configTail {
		L(l)
	}
	res.Lines = lines
	res.Text = strings.Join(lines, "\r\n")
	return res
}

var microsoftPrefix = regexp.MustCompile(`(?i)^Microsoft[\s\p{Z}]+`)

// configIndex is the instance index report-config.ps1 infers from a device's console port, counted in the root's
// plan (the script knows only PORT_BASE 20000); ok=false when cport is not on it.
func configIndex(plan root.PortPlan, cport int) (string, bool) {
	off := cport - plan.Base
	if off < 0 || off%root.PortStep != 0 {
		return "", false
	}
	return strconv.Itoa(off / root.PortStep), true
}

// gamesOf asks one running instance for its third-party packages and their versions.
func gamesOf(ctx context.Context, t configTarget) []string {
	fail := func(err error) []string { return []string{"读不到（" + err.Error() + "）"} }
	boot, err := adbShell(ctx, t.port, t.serial, "getprop sys.boot_completed")
	if err != nil {
		return fail(err)
	}
	if boot != "1" {
		return []string{"读不到（还没开完机）"}
	}
	out, err := adbShell(ctx, t.port, t.serial, "pm list packages -3")
	if err != nil {
		return fail(err)
	}
	var pkgs []string
	for _, line := range strings.Split(out, "\n") {
		line = netTrim(line)
		if len(line) < 8 || !strings.EqualFold(line[:8], "package:") {
			continue
		}
		pkg := line[8:]
		if !containsFold(notGames, pkg) {
			pkgs = append(pkgs, pkg)
		}
	}
	sortCulture(pkgs)
	var games []string
	for _, pkg := range pkgs {
		// grep -m1 quitting early makes dumpsys add "Broken pipe" to stderr, which the shell protocol merges into
		// the output: only the versionName line counts
		v, err := adbShell(ctx, t.port, t.serial, "dumpsys package "+pkg+" 2>/dev/null | grep -m1 versionName=")
		if err != nil {
			return fail(err)
		}
		version := ""
		for _, l := range strings.Split(v, "\n") {
			if versionPrefix.MatchString(l) {
				version = netTrim(versionPrefix.ReplaceAllString(l, ""))
				break
			}
		}
		name := pkg
		for _, g := range gameNames {
			if strings.EqualFold(g[0], pkg) {
				name = g[1]
				break
			}
		}
		if version != "" {
			games = append(games, name+" "+version)
		} else {
			games = append(games, name)
		}
	}
	if len(games) == 0 {
		games = []string{"没装"}
	}
	return games
}

// startErrText is why a program did not start, without its path (exec and os errors name it; the report never
// carries a path).
func startErrText(err error) string {
	var ee *exec.Error
	var pe *fs.PathError
	switch {
	case errors.As(err, &ee):
		return ee.Err.Error()
	case errors.As(err, &pe):
		return pe.Err.Error()
	}
	return err.Error()
}

// driveLetter is ([IO.Path]::GetPathRoot($Root)).Substring(0, 1) when that is a drive letter, "" otherwise (a UNC
// root, where [IO.DriveInfo] throws).
func driveLetter(dir string) string {
	vol := filepath.VolumeName(dir)
	if len(vol) == 2 && vol[1] == ':' {
		return vol[:1]
	}
	return ""
}

// diskKindText is the bus type and the media type (not Unspecified) joined with a space: "USB SSD", "NVMe SSD".
func diskKindText(bus, media string) string {
	var parts []string
	if bus != "" {
		parts = append(parts, bus)
	}
	if media != "" && media != "Unspecified" {
		parts = append(parts, media)
	}
	return strings.Join(parts, " ")
}

// absUnder makes a relative path absolute against dir (mas-avd.json's sdkRoot, as the backend resolves it).
func absUnder(dir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

// listening is Test-Listening: somebody listens on port (any address).
func listening(ls []Listener, port int) bool {
	for _, l := range ls {
		if l.Port == port {
			return true
		}
	}
	return false
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// containsFold is -contains on strings (case-insensitive).
func containsFold(s []string, v string) bool {
	for _, x := range s {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// keyFold finds key in a map used as a PowerShell hashtable (keys case-insensitive).
func keyFold(m map[string]*jsonord.Value, key string) (string, bool) {
	if _, ok := m[key]; ok {
		return key, true
	}
	for k := range m {
		if strings.EqualFold(k, key) {
			return k, true
		}
	}
	return "", false
}

// lookupFold is a PowerShell hashtable lookup.
func lookupFold(m map[string]*jsonord.Value, key string) (*jsonord.Value, bool) {
	k, ok := keyFold(m, key)
	return m[k], ok
}
