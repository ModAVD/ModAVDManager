package emu

import (
	"context"
	"fmt"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/guest"
	"modavd/internal/jsonord"
	"modavd/internal/root"
	"modavd/internal/win"
)

// allowInstallSourcesScript lets every app that asks for REQUEST_INSTALL_PACKAGES install APKs while that app op is
// still in its default mode (`android` itself excluded), printing who was allowed this time. Modes set otherwise
// (toggled by the user, or allowed at an earlier boot) are left alone; an app whose mode cannot be read
// (`appops get` non-zero) is not written and counts as failed. Enumeration failure exits 2; a failed app prints
// "failed: <package>", the rest go on, the exit code is then 1. dumpsys also exits 0 when the service is missing,
// so finding no package section counts as an enumeration failure. Same text as AUTO-MAS
// _ALLOW_INSTALL_SOURCES_SCRIPT and the launcher.
const allowInstallSourcesScript = `set -o pipefail; ` +
	`list=$(dumpsys package packages 2>/dev/null` +
	` | awk '/^  Package \[/ { n++; p = substr($2, 2, length($2) - 2) }` +
	` /android.permission.REQUEST_INSTALL_PACKAGES/ { if (p != "" && p != "android") print p }` +
	` END { if (n == 0) exit 3 }'` +
	` | sort -u) || { echo "dumpsys package failed"; exit 2; }; ` +
	`fail=0; ` +
	`for p in $list; do` +
	` m=$(appops get "$p" REQUEST_INSTALL_PACKAGES 2>&1)` +
	` || { echo "failed: $p"; fail=1; continue; };` +
	` case "$m" in` +
	` *"REQUEST_INSTALL_PACKAGES: default"*) ;;` +
	` *"REQUEST_INSTALL_PACKAGES: "*) continue ;; esac;` +
	` if appops set "$p" REQUEST_INSTALL_PACKAGES allow; then echo "$p";` +
	` else echo "failed: $p"; fail=1; fi;` +
	` done; exit $fail`

type afterBootResult struct {
	FirstBoot       bool
	HostIME         bool
	ZygoteRestarted bool
	LogcatFile      string
	// SetupPending: the Vulkan fix this boot still needs was left out (an app was in the foreground or an install was
	// running, see ensureVulkanFix); the setup marker has to stay so that a later start mounts it.
	SetupPending bool
}

// bootSetup is how much of a boot's one-time setup a start owes.
type bootSetup int

const (
	// setupAdopted: the instance was already running and nothing is owed: the guest Vulkan fix is only checked (the
	// zygote restart of mounting it would end the game running there), and the IME channel is judged by the build
	// alone (the emulator log may belong to another boot).
	setupAdopted bootSetup = iota
	// setupLaunched: this call launched qemu; nobody can have used the guest yet, so everything runs.
	setupLaunched
	// setupOwed: this call adopts the qemu of an earlier start that ended before finishing (the setup marker, see
	// Start). Everything runs as after a launch, except that the guest may be in use meanwhile: the Vulkan fix is
	// mounted only while no app is in the foreground and no install holds the guest lock (owedMountGate).
	setupOwed
)

// afterBoot is AUTO-MAS _after_boot: the per-boot tuning (gone after a reboot, also one of the guest alone: the boot
// watch then runs it again, see bootwatch.go) plus the one-time first-boot setup.
// Failures are only warnings and never stop the boot; ctx is checked between the steps (errs.Canceled then). mode
// says what this boot is owed (bootSetup); until is the deadline of its setup marker (zero without one), which the
// warning of a deferred Vulkan fix names.
//
// Not done any more (as in AUTO-MAS): `gsm meter on|off` (each call makes the image's MeterService install a 2 GB
// test plan, i.e. it creates the limit), and /data nobarrier (replaced by disk write-through at launch).
func (m *Manager) afterBoot(ctx context.Context, inst *avd.Instance, mode bootSetup, until time.Time, logPath string,
	prog *progressor) (afterBootResult, error) {
	var r afterBootResult
	i := inst.Index
	port := root.ConsolePort(i)
	var imeDefault bool
	cores, ok := inst.CPU()
	if !ok || cores <= 0 {
		cores = avd.DefaultCPU
	}

	steps := []func(){
		func() {
			prog.emit("tune", fmt.Sprintf("实例 %d 开机后调优", i), progressTune)
			// 1) host scheduling: priority, X3D pinning
			if p, ok := m.findQemu(i); ok {
				m.Log.Infof("实例 %d 宿主调度: %s", i, tuneHostScheduling(p, cores))
			}
		},
		func() {
			// 2) screenshot shared memory: SHM_videmulator<port> only exists while streaming, and a static screen
			//    stays black in it, so force one SurfaceFlinger repaint afterwards (1004 needs root)
			if _, err := adb.Console(ctx, port, "screenrecord webrtc start", 10*time.Second); err != nil {
				m.Log.Warnf("实例 %d 开启截图共享内存失败: %v", i, err)
			}
			m.shell(ctx, i, "su 0 service call SurfaceFlinger 1004", 0)
		},
		func() {
			// 3) disk and screenshot channel state into the log (write_cache is root-only)
			wc := m.su(ctx, i, "cat /sys/block/vd*/queue/write_cache | sort -u", 0)
			shm := "缺失"
			if win.SharedMemoryExists(shmName(i)) {
				shm = "在"
			}
			m.Log.Infof("实例 %d 磁盘缓存: %s；截图共享内存 %s: %s", i, orText(joinNonEmptyLines(wc.Out, " / "), "读不到"),
				shmName(i), shm)
		},
		// 3b) data partition raised by `set --data-gb`: the emulator grew the disk, grow the file system (online)
		func() { m.growData(ctx, i) },
		// 4) shorter guest dirty writeback (less lost on a hard kill; not persistent, so every boot)
		func() { m.setupGuestWriteback(ctx, i) },
		// 5) free page reporting: binding the driver resets the order to pageblock_order, set it afterwards
		func() { m.setupBalloon(ctx, i) },
		func() {
			// 6) cpuinfo of the ARM translation layer: one entry per vCPU, or Unity picks the lowest quality
			if remote, ok := m.pushGuestScript(ctx, i, guest.CPUInfoBind); ok {
				res := m.shell(ctx, i, "su 0 sh "+remote, 0)
				m.Log.Infof("实例 %d ARM 转译 cpuinfo: %s 核", i, orText(strings.TrimSpace(res.Out), "?"))
			}
		},
		func() {
			// 7) nobody taps crash / ANR dialogs on an unattended instance and they cover the game (persistent)
			m.shell(ctx, i, "settings put global hide_error_dialogs 1", 0)
		},
		func() {
			// 7b) NetworkWatchlistService hashes every APK in full once a day has passed since the last boot (about
			//     4 GB into the page cache, the whole guest RAM then stays resident on the host). Read when
			//     system_server starts: effective from the next boot, also after the zygote restart of step 11
			res := m.shell(ctx, i, "settings put global network_watchlist_enabled 0", 0)
			if res.Code != 0 {
				m.Log.Warnf("实例 %d 关闭 NetworkWatchlistService 失败（下次开机仍会把安装包整个读一遍）: %s",
					i, orText(tail(res.Out, 200), fmt.Sprintf("退出码 %d", res.Code)))
			}
		},
		func() {
			// 7c) installing APKs from any source (persistent; every boot covers newly installed apps): an emulator
			//     is no phone. Toggles the user changed stay; every install still asks in the system installer
			res := m.shell(ctx, i, allowInstallSourcesScript, 0)
			if res.Code != 0 {
				m.Log.Warnf("实例 %d 放开「安装未知应用」没做完（这些来源装 APK 仍会被拦）: %s",
					i, orText(tail(strings.Join(strings.Fields(res.Out), " "), 300), fmt.Sprintf("退出码 %d", res.Code)))
			} else if strings.TrimSpace(res.Out) != "" {
				m.Log.Debugf("实例 %d 本次允许安装未知应用: %s", i, strings.Join(strings.Fields(res.Out), " "))
			}
		},
		// 7d) host input method: install, enable, make it the default by the rules (every boot)
		// (logPath is this boot's emulator log unless the instance was simply adopted)
		func() { imeDefault = m.setupHostIme(ctx, i, mode != setupAdopted, logPath, prog) },
		// 8) network: Wi-Fi always on (games then skip "not on Wi-Fi" prompts); fastpath sends bulk traffic over
		//    eth0 (the virtual Wi-Fi alone does about 6 MB/s). The Wi-Fi switch persists in /data: set every boot
		func() { m.setupNetwork(ctx, i) },
		// 8b) no vCPU halts while the data disk is busy with small requests (see the script's header)
		func() { m.setupIdlepoll(ctx, i) },
		func() {
			// 9) guest log: 16 MB buffers (persistent), streamed to <root>\logs from now on
			m.shell(ctx, i, "su 0 setprop persist.logd.size "+logcatBufferSize+"; logcat -G "+logcatBufferSize, 0)
			r.LogcatFile = m.ensureLogcat(ctx, i)
		},
		func() {
			if !m.initialized(i) {
				prog.emit("first_boot", fmt.Sprintf("实例 %d 首次开机初始化", i), progressFirstBoot)
				m.firstBootInit(ctx, i)
				r.FirstBoot = true
			}
		},
		// 10) orientation: the display stays landscape (persistent; set every boot so old instances get it)
		func() { m.setupOrientation(ctx, i) },
		func() {
			// 11) the guest Vulkan fix. Must be last: mounting it restarts zygote, system_server and every app
			prog.emit("vulkan_fix", fmt.Sprintf("实例 %d 客体 Vulkan 驱动补丁", i), progressVulkanFix)
			restarted, pending := m.ensureVulkanFix(ctx, i, mode, until)
			r.SetupPending = pending
			if !restarted {
				return
			}
			r.ZygoteRestarted = true
			if ctx.Err() != nil {
				return
			}
			// the letterbox education switch lives in system_server's memory and went with the restart
			m.setupOrientation(ctx, i)
			// the default input method lives in settings and should survive; it was ours after 7d, so check it
			if imeDefault {
				m.recheckHostIme(i, m.guestIO(ctx, i))
			}
		},
	}
	for _, step := range steps {
		if err := ctxErr(ctx); err != nil {
			return r, err
		}
		step()
	}
	r.HostIME = imeDefault
	return r, ctxErr(ctx)
}

// affinityLockWait is how long tuneHostScheduling waits for the machine-wide lock around read-choose-pin.
const affinityLockWait = 10 * time.Second

// affinityLockPath is that lock: instances booting at the same time (one modavd process each, or several roots) must not
// all read the same load and pick the same group.
func affinityLockPath() string {
	return filepath.Join(os.TempDir(), "modavd-affinity.lock")
}

// tuneHostScheduling raises qemu to above normal priority and, on X3D parts, pins it to one of the L3 groups (09-28 on
// a 7950X3D, Honkai: qemu CPU -20%, the game's main thread CPU per frame -25%; the high-clock CCD was no better than
// default). Only pinned when the L3 sizes differ; otherwise Windows decides. Which group: the one with the most free
// logical processors (vCPUs of the qemu already pinned there subtracted), the bigger L3 on a tie; when neither has
// room for cores vCPUs the instance is not pinned and Windows spreads it (ModAVD#34: pinning every instance to the big
// group left the other CCD idle). Returns the note for the log.
func tuneHostScheduling(p win.Process, cores int) string {
	var notes []string
	if err := win.SetAboveNormalPriority(p); err != nil {
		notes = append(notes, fmt.Sprintf("设优先级失败（%v）", err))
	} else {
		notes = append(notes, "优先级高于正常")
	}
	groups := win.L3Groups()
	if !hasBigL3(groups) {
		return strings.Join(notes, "，")
	}
	lock, ok, err := win.Lock(affinityLockPath(), affinityLockWait)
	if err != nil || !ok {
		return strings.Join(append(notes, "拿不到绑核锁，不绑核"), "，")
	}
	defer lock.Unlock()
	c := chooseAffinity(groups, otherQemuAffinities(p), cores)
	switch {
	case c.Group < 0:
		notes = append(notes, fmt.Sprintf("各组 L3 剩余核数 %v 都不够 %d 个 vCPU，不绑核", c.Free, cores))
	default:
		if err := win.SetAffinity(p, c.Mask); err != nil {
			notes = append(notes, fmt.Sprintf("绑核失败（%v）", err))
		} else {
			notes = append(notes, fmt.Sprintf("绑定 L3 %d MB 的核心 %s（各组剩余核数 %v，需要 %d）", groups[c.Group].Size>>20,
				cpuRanges(c.Mask), c.Free, cores))
		}
	}
	return strings.Join(notes, "，")
}

// otherQemuAffinities lists the affinity of every other qemu on the host (this root's or another's, production
// included: they share the cores). Their vCPU count is not known (it is in the AVD config, not on the command line),
// so each counts as the cores of the instance being tuned.
func otherQemuAffinities(self win.Process) []uint64 {
	procs, err := win.ListProcesses()
	if err != nil {
		return nil
	}
	var masks []uint64
	for _, q := range procs {
		if q.PID == self.PID || !isQemuName(q.Name) {
			continue
		}
		mask, err := win.GetAffinity(q)
		if err != nil {
			mask = 0 // not readable (e.g. started elevated): counts as unpinned
		}
		masks = append(masks, mask)
	}
	return masks
}

// hasBigL3 is true when the L3 groups differ in size (AMD X3D).
func hasBigL3(groups []win.L3Group) bool {
	if len(groups) < 2 {
		return false
	}
	lo, hi := groups[0].Size, groups[0].Size
	for _, g := range groups {
		lo, hi = min(lo, g.Size), max(hi, g.Size)
	}
	return hi > lo
}

// affinityChoice: Group is the index in the group list (-1: none has room), Mask its processors, Free the free
// logical processors per group before this instance.
type affinityChoice struct {
	Group int
	Mask  uint64
	Free  []int
}

// chooseAffinity is the pure decision. others are the affinity masks of the other qemu, each counting as cores vCPUs
// spread over the groups in proportion to the logical processors its mask has in each (a mask inside one group loads
// that group only; 0, unreadable, or one covering everything spreads over all groups by their size). Max free wins,
// ties go to the bigger L3, then to the lower mask. A group with fewer free processors than cores is not an option.
func chooseAffinity(groups []win.L3Group, others []uint64, cores int) affinityChoice {
	total := make([]int, len(groups))
	var all uint64
	for k, g := range groups {
		total[k] = bits.OnesCount64(g.Mask)
		all |= g.Mask
	}
	load := make([]float64, len(groups))
	for _, m := range others {
		m &= all
		if m == 0 {
			m = all
		}
		n := bits.OnesCount64(m)
		for k, g := range groups {
			load[k] += float64(cores) * float64(bits.OnesCount64(m&g.Mask)) / float64(n)
		}
	}
	free := make([]int, len(groups))
	for k := range groups {
		free[k] = total[k] - int(math.Round(load[k]))
	}
	choice := affinityChoice{Group: -1, Free: free}
	for k, g := range groups {
		if free[k] < cores {
			continue
		}
		if choice.Group < 0 || free[k] > free[choice.Group] ||
			free[k] == free[choice.Group] && (g.Size > groups[choice.Group].Size ||
				g.Size == groups[choice.Group].Size && g.Mask < groups[choice.Group].Mask) {
			choice.Group, choice.Mask = k, g.Mask
		}
	}
	return choice
}

// cpuRanges renders a mask as "0–15" / "0–7,16–23".
func cpuRanges(mask uint64) string {
	var parts []string
	for c := 0; c < 64; c++ {
		if mask>>c&1 == 0 {
			continue
		}
		e := c
		for e+1 < 64 && mask>>(e+1)&1 == 1 {
			e++
		}
		if e == c {
			parts = append(parts, strconv.Itoa(c))
		} else {
			parts = append(parts, fmt.Sprintf("%d–%d", c, e))
		}
		c = e
	}
	return strings.Join(parts, ",")
}

// setupGuestWriteback sets the guest dirty_expire / dirty_writeback to 200 / 100 cs and reads them back (10-04 hard
// kill tests, deleting then renaming the temp file came back 0 bytes up to 34 s after the write at
// the default 3000 / 500; 200 / 100 shrinks that to about 5 s for ~14% more host writes).
func (m *Manager) setupGuestWriteback(ctx context.Context, i int) {
	res := m.su(ctx, i, fmt.Sprintf("echo %d > /proc/sys/vm/dirty_expire_centisecs; "+
		"echo %d > /proc/sys/vm/dirty_writeback_centisecs; "+
		"cat /proc/sys/vm/dirty_expire_centisecs /proc/sys/vm/dirty_writeback_centisecs",
		guestDirtyExpireCs, guestDirtyWritebackCs), 0)
	expire, writeback, ok := parseWriteback(res.Out)
	if ok && expire == guestDirtyExpireCs && writeback == guestDirtyWritebackCs {
		m.Log.Infof("实例 %d 客体回写: dirty_expire %d cs、dirty_writeback %d cs", i, expire, writeback)
	} else {
		m.Log.Warnf("实例 %d 客体回写没设上（要 %d/%d）: %s", i, guestDirtyExpireCs, guestDirtyWritebackCs, tail(res.Out, 200))
	}
}

// parseWriteback reads the first two fields as integers (AUTO-MAS _int per field); ok=false when either is not one.
func parseWriteback(out string) (expire, writeback int, ok bool) {
	f := strings.Fields(out)
	if len(f) < 2 {
		return 0, 0, false
	}
	e, err1 := strconv.Atoi(f[0])
	w, err2 := strconv.Atoi(f[1])
	return e, w, err1 == nil && err2 == nil
}

func (m *Manager) setupBalloon(ctx context.Context, i int) {
	const param = "/sys/module/page_reporting/parameters/page_reporting_order"
	res := m.su(ctx, i, fmt.Sprintf("ls /sys/bus/virtio/drivers/virtio_balloon | grep -c virtio; echo %d > %s; cat %s",
		balloonReportingOrder, param, param), 0)
	bound, current, ok := balloonState(res.Out, balloonReportingOrder)
	if !ok {
		m.Log.Warnf("实例 %d 空闲页上报没就位：气球设备绑定 %s，page_reporting_order %s（要 %d）；模拟器要 sdk-mas19 起的自编 qemu",
			i, bound, current, balloonReportingOrder)
	} else {
		m.Log.Infof("实例 %d 空闲页上报: 气球设备绑定 %s，page_reporting_order %s", i, bound, current)
	}
}

// balloonState: the bound virtio_balloon device count and the order read back ("?" when missing); ok when a device
// is bound and the order is the wanted one.
func balloonState(out string, order int) (bound, current string, ok bool) {
	parts := strings.Fields(out)
	bound, current = "?", "?"
	if len(parts) > 0 {
		bound = parts[0]
	}
	if len(parts) > 1 {
		current = parts[1]
	}
	ok = bound != "" && bound != "0" && bound != "?" && current == strconv.Itoa(order)
	return bound, current, ok
}

func (m *Manager) setupNetwork(ctx context.Context, i int) {
	m.shell(ctx, i, "su 0 svc wifi enable", 0)
	remote, ok := m.pushGuestScript(ctx, i, guest.WifiFastpath)
	if !ok {
		return
	}
	m.shell(ctx, i, "su 0 sh "+remote+" on; su 0 sh "+remote+" daemon", 0)
	status := m.shell(ctx, i, "su 0 sh "+remote+" status", 0).Out
	if warn, msg := fastpathReport(i, status); warn {
		m.Log.Warnf("%s", msg)
	} else {
		m.Log.Infof("%s", msg)
	}
}

// fastpathReport judges `mas-wifi-fastpath.sh status`: the IPv4 rule is in place, or at least the watcher runs
// (right after boot eth0 often has no routing table yet; the watcher adds the rule within 10 s once it has).
func fastpathReport(i int, status string) (warn bool, msg string) {
	lines := parseStatusLines(status)
	if strings.Contains(lines["rule4"], "lookup eth0") {
		return false, fmt.Sprintf("实例 %d 网络: WiFi 开，fastpath 生效（%s）", i, lines["rule4"])
	}
	if lines["watchers"] != "" {
		return false, fmt.Sprintf("实例 %d 网络: WiFi 开，fastpath 守护已起（pid %s），规则在 eth0 就绪后由守护补上", i, lines["watchers"])
	}
	return true, fmt.Sprintf("实例 %d fastpath 没起来: %s", i, tail(status, 300))
}

func (m *Manager) setupIdlepoll(ctx context.Context, i int) {
	remote, ok := m.pushGuestScript(ctx, i, guest.IdlePoll)
	if !ok {
		return
	}
	started := m.shell(ctx, i, "su 0 sh "+remote+" daemon", 0).Out
	status := m.shell(ctx, i, "su 0 sh "+remote+" status", 0).Out
	if warn, msg := idlepollReport(i, started, status); warn {
		m.Log.Warnf("%s", msg)
	} else {
		m.Log.Infof("%s", msg)
	}
}

// idlepollReport judges `mas-idlepoll.sh daemon` + `status`: running when a watcher pid is listed.
func idlepollReport(i int, started, status string) (warn bool, msg string) {
	lines := parseStatusLines(status)
	if lines["watchers"] != "" {
		return false, fmt.Sprintf("实例 %d 密集读写加速: 守护已起（pid %s，数据盘 %s）", i, lines["watchers"],
			orText(lines["disk"], "?"))
	}
	return true, fmt.Sprintf("实例 %d 密集读写加速没起来: %s", i, tail(strings.TrimSpace(started)+" / "+status, 300))
}

// parseStatusLines maps "key: value" lines (key as is, value trimmed; a line without ':' maps to ""), later lines
// winning, like AUTO-MAS's dict over str.partition(":").
func parseStatusLines(out string) map[string]string {
	lines := map[string]string{}
	for _, line := range outputLines(out) {
		key, value, _ := strings.Cut(line, ":")
		lines[key] = strings.TrimSpace(value)
	}
	return lines
}

// setupOrientation keeps the display at 0° (landscape): it follows neither the sensor nor apps' orientation
// requests, and landscape games are never flipped to 180°. A portrait page then sits letterboxed in the middle.
// The letterbox education dialog ("See and do more") is turned off first: that switch is not persistent, so every
// boot; `wm fixed-to-user-rotation` is stored in display_settings.xml and survives reboots.
func (m *Manager) setupOrientation(ctx context.Context, i int) {
	var failures []string
	if res := m.shell(ctx, i, "wm set-letterbox-style --isEducationEnabled false", 0); res.Code != 0 {
		failures = append(failures, "关信箱引导: "+tail(res.Out, 200))
	}
	if res := m.shell(ctx, i, "settings put system user_rotation 0 ; wm fixed-to-user-rotation enabled", 0); res.Code != 0 {
		failures = append(failures, "固定方向: "+tail(res.Out, 200))
	}
	rotation := m.shell(ctx, i, "dumpsys window displays | grep -m1 -oE mDisplayRotation=ROTATION_[0-9]+", 0)
	if len(failures) > 0 {
		m.Log.Warnf("实例 %d 方向没设好: %s", i, pyList(failures))
	} else {
		m.Log.Infof("实例 %d 方向: 固定 0°，当前 %s", i, parseRotation(rotation.Out))
	}
}

// parseRotation takes the mDisplayRotation= line only: when grep -m1 exits early dumpsys adds a "Broken pipe" line.
func parseRotation(out string) string {
	for _, line := range outputLines(out) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "mDisplayRotation=") {
			_, v, _ := strings.Cut(line, "=")
			return v
		}
	}
	return "读不到"
}

// initialized: mas-avd.json instances.<i>.initialized is truthy (Python `if not meta.get("initialized")`).
func (m *Manager) initialized(i int) bool {
	meta := m.Root.InstanceMeta(i)
	if meta == nil {
		return false
	}
	return jsonTruthy(meta.Get("initialized"))
}

// jsonTruthy is Python's truth value of a JSON value.
func jsonTruthy(v *jsonord.Value) bool {
	switch v.Kind() {
	case jsonord.Bool:
		b, _ := v.BoolVal()
		return b
	case jsonord.Number:
		f, ok := v.Float()
		return !ok || f != 0
	case jsonord.String:
		s, _ := v.Str()
		return s != ""
	case jsonord.Array:
		return len(v.Items()) > 0
	case jsonord.Object:
		return v.Obj() != nil && v.Obj().Len() > 0
	}
	return false
}

// joinNonEmptyLines joins the trimmed non-empty lines of out.
func joinNonEmptyLines(out, sep string) string {
	var lines []string
	for _, line := range outputLines(out) {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, sep)
}

// lastNonEmptyLine is the last trimmed non-empty line, "" when none.
func lastNonEmptyLine(out string) string {
	lines := outputLines(out)
	for k := len(lines) - 1; k >= 0; k-- {
		if line := strings.TrimSpace(lines[k]); line != "" {
			return line
		}
	}
	return ""
}

func orText(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// pyList renders a list of strings the way AUTO-MAS's messages show one (Python repr): ['a', 'b'], so multi-line
// adb output stays on one line.
func pyList(items []string) string {
	quoted := make([]string, len(items))
	for k, s := range items {
		quoted[k] = pyRepr(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// pyRepr is Python's repr of a str: single quotes unless the text has a single and no double quote; backslash, the
// quote, \n \r \t and other control characters escaped; printable non-ASCII kept.
func pyRepr(s string) string {
	quote := byte('\'')
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = '"'
	}
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == '\\' || r == rune(quote):
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}
