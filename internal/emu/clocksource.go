package emu

// The guest's clock source (ModAVD/ModAVD#110). On Windows + WHPX the guest kernel sometimes (about 1 in 18 cold boots)
// stops trusting tsc and falls back to hpet: every clock read then exits to qemu, and the whole boot is about half as
// fast (synchronous disk writes measured 8 instead of 16–18 MB/s). It stays so until the next cold boot. Why the
// kernel gave up on tsc is only in the guest kernel log, and end users do not look there, so every boot reads the
// clock source once at the end of afterBoot, writes it into this start's log (the boot watch's log for a guest-only
// reboot) and records it in <root>\avd\.mas_<i>.clocksource.json for collect-info. An x86_64 guest that is not on tsc
// also gets the clock lines of its kernel log saved to <root>\logs\mas_<i>-<time>-clocksource.txt right away, before
// they scroll out of the buffer. arm64 guests have no tsc (arch_sys_counter is normal there): read and logged, never
// warned about. A failed read is one log line; nothing is retried and the boot goes on.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/root"
)

const (
	// clocksourceScript prints the current clock source, the available ones and the guest's machine, separated by
	// "---" lines: one fixed command line without variables. As root: on the Android 14 images SELinux does not let
	// the adb shell user read this sysfs directory (every boot logged "Permission denied").
	clocksourceScript = "su 0 sh -c 'cat /sys/devices/system/clocksource/clocksource0/current_clocksource; echo ---; " +
		"cat /sys/devices/system/clocksource/clocksource0/available_clocksource; echo ---; uname -m'"
	clocksourceTimeout = 10 * time.Second
	// clocksourceKernelCommand is the guest kernel log as root (logcat -b kernel needs it on these images; dmesg may
	// have been overwritten by then). Filtered here, on the host, so no pipe runs in the guest.
	clocksourceKernelCommand = "su 0 logcat -b kernel -d"
	clocksourceKernelTimeout = 20 * time.Second
	// clocksourceKernelMax bounds a saved kernel log; the clock lines come first and get at most clocksourceMatchMax.
	clocksourceKernelMax = 4 << 20
	clocksourceMatchMax  = 256 << 10
	// clocksourceKernelKeep: saved kernel logs kept per instance (they are written only on a fallback).
	clocksourceKernelKeep = 5
	clocksourceSuffix     = "-clocksource.txt"
)

// clocksourceLine selects the kernel log lines saved first (case-insensitive, as grep -iE).
var clocksourceLine = regexp.MustCompile(`(?i)clocksource|tsc|watchdog|hpet`)

// clocksourceReason picks, among those, the lines that say why tsc was dropped (the three cases of ModAVD#110: TSC
// warp at boot, the watchdog's skew, an hpet read-back that took too long).
var clocksourceReason = regexp.MustCompile(`(?i)unstable|warp|skew|read-back delay`)

// clocksourceName: what a clock source or machine name looks like; anything else (an error text) counts as unread.
var clocksourceName = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// clocksourceRecord is the content of clocksourcePath: the clock source of the instance's latest boot.
type clocksourceRecord struct {
	At          string `json:"at"`                   // RFC 3339, when it was read
	Clocksource string `json:"clocksource"`          // "" when it could not be read
	Available   string `json:"available,omitempty"`  // available_clocksource, space separated
	Arch        string `json:"arch,omitempty"`       // uname -m; "" when unread
	X86         bool   `json:"x86"`                  // judged as an x86 guest (only these must be on tsc)
	KernelLog   string `json:"kernel_log,omitempty"` // file name in the logs dir when one was saved
	Error       string `json:"error,omitempty"`      // why the clock source or the kernel log was not read
}

// fallback: an x86 guest whose clock source was read and is not tsc.
func (r clocksourceRecord) fallback() bool {
	return r.X86 && r.Clocksource != "" && r.Clocksource != "tsc"
}

// clocksourcePath is <root>\avd\.mas_<i>.clocksource.json.
func (m *Manager) clocksourcePath(i int) string {
	return filepath.Join(m.Root.AvdHome(), "."+root.AvdName(i)+".clocksource.json")
}

// parseClocksource reads clocksourceScript's output. ok=false when the current clock source is not there.
func parseClocksource(out string) (current, available, arch string, ok bool) {
	var parts [3][]string
	k := 0
	for _, line := range outputLines(out) {
		line = strings.TrimSpace(line)
		if line == "---" {
			if k < 2 {
				k++
			}
			continue
		}
		if line != "" {
			parts[k] = append(parts[k], line)
		}
	}
	if len(parts[0]) == 1 && clocksourceName.MatchString(parts[0][0]) {
		current, ok = parts[0][0], true
	}
	if len(parts[1]) == 1 {
		available = strings.Join(strings.Fields(parts[1][0]), " ")
	}
	if len(parts[2]) == 1 && clocksourceName.MatchString(parts[2][0]) {
		arch = parts[2][0]
	}
	return current, available, arch, ok
}

// x86Guest: by the guest's machine (uname -m) when read, otherwise by the instance's config (bridgeGuestSupported:
// not arm64 there means x86_64).
func x86Guest(arch string, cfg *avd.Ini) bool {
	switch a := strings.ToLower(arch); {
	case a == "x86_64" || a == "amd64" || a == "x86" || a == "i386" || a == "i686":
		return true
	case a != "":
		return false
	}
	return bridgeGuestSupported(cfg)
}

// recordClocksource is the afterBoot step: once, through the instance's adb shell.
func (m *Manager) recordClocksource(ctx context.Context, inst *avd.Instance) {
	i := inst.Index
	m.checkClocksource(i, inst.Config, func(command string, timeout time.Duration) adb.Result {
		return m.shell(ctx, i, command, timeout)
	}, time.Now())
}

// checkClocksource reads the clock source with run (a guest shell command line), logs it, saves the kernel log's clock
// lines on a fallback and writes the record. Never fails: every problem is a log line.
func (m *Manager) checkClocksource(i int, cfg *avd.Ini, run func(command string, timeout time.Duration) adb.Result,
	now time.Time) clocksourceRecord {
	rec := clocksourceRecord{At: now.Format(time.RFC3339)}
	res := run(clocksourceScript, clocksourceTimeout)
	current, available, arch, ok := parseClocksource(res.Out)
	rec.Available, rec.Arch, rec.X86 = available, arch, x86Guest(arch, cfg)
	switch {
	case !ok:
		rec.Available = "" // whatever stood there is part of an error text
		rec.Error = orText(tail(strings.Join(strings.Fields(res.Out), " "), 200), fmt.Sprintf("退出码 %d", res.Code))
		m.Log.Warnf("实例 %d 读不到客体时钟源（%s），这次开机不判断", i, rec.Error)
	case !rec.X86 || current == "tsc":
		rec.Clocksource = current
		m.Log.Infof("实例 %d 客体时钟源: %s（可选 %s，客体 %s）", i, current, orText(available, "?"), orText(arch, "?"))
	default:
		rec.Clocksource = current
		path, reasons, err := m.saveClocksourceKernelLog(i, rec, run, now)
		msg := fmt.Sprintf("实例 %d 客体时钟源是 %s，不是 tsc（可选 %s，客体 %s）：这次开机读时钟都要退出到模拟器，"+
			"整体会慢，关机再开才恢复", i, current, orText(available, "?"), orText(arch, "?"))
		if err != nil {
			rec.Error = err.Error()
			msg += fmt.Sprintf("；客体内核日志没存下: %v", err)
		} else {
			rec.KernelLog = filepath.Base(path)
			msg += "；客体内核日志已存到 " + path
			if len(reasons) > 0 {
				msg += "；原因行: " + pyList(reasons)
			} else {
				msg += "；内核日志里没找到停用 tsc 的原因行"
			}
		}
		m.Log.Warnf("%s", msg)
	}
	m.writeClocksourceRecord(i, rec)
	return rec
}

// writeClocksourceRecord replaces the record; a failure is a debug line (it only feeds collect-info).
func (m *Manager) writeClocksourceRecord(i int, rec clocksourceRecord) {
	b, err := json.Marshal(rec)
	if err == nil {
		err = os.MkdirAll(m.Root.AvdHome(), 0o755)
	}
	if err == nil {
		err = writeFileReplace(m.clocksourcePath(i), b, strconv.Itoa(os.Getpid()))
	}
	if err != nil {
		m.Log.Debugf("实例 %d 记不下这次开机的时钟源: %v", i, err)
	}
}

// saveClocksourceKernelLog reads the guest kernel log once and writes <logs>\mas_<i>-<time>-clocksource.txt: the clock
// lines first, then the log itself from its start, together at most clocksourceKernelMax bytes. reasons are the clock
// lines that say why tsc was dropped (at most three, each cut to 300 bytes).
func (m *Manager) saveClocksourceKernelLog(i int, rec clocksourceRecord, run func(string, time.Duration) adb.Result,
	now time.Time) (path string, reasons []string, err error) {
	res := run(clocksourceKernelCommand, clocksourceKernelTimeout)
	if res.Code != 0 || strings.TrimSpace(res.Out) == "" {
		return "", nil, fmt.Errorf("logcat -b kernel -d: %s", orText(tail(res.Out, 200), fmt.Sprintf("退出码 %d", res.Code)))
	}
	var matched []string
	for _, line := range outputLines(res.Out) {
		if !clocksourceLine.MatchString(line) {
			continue
		}
		matched = append(matched, line)
		if len(reasons) < 3 && clocksourceReason.MatchString(line) {
			reasons = append(reasons, tail(line, 300))
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# 实例 %d 客体时钟源 %s（可选 %s，客体 %s），读于 %s\n", i, rec.Clocksource, orText(rec.Available, "?"),
		orText(rec.Arch, "?"), rec.At)
	fmt.Fprintf(&b, "# 来源: adb shell %s；见 ModAVD/ModAVD#110\n\n", clocksourceKernelCommand)
	fmt.Fprintf(&b, "=== 和时钟有关的行（clocksource|tsc|watchdog|hpet，不分大小写），共 %d 行 ===\n", len(matched))
	written := 0
	for k, line := range matched {
		if written+len(line)+1 > clocksourceMatchMax {
			fmt.Fprintf(&b, "……（后面还有 %d 行，超过 %d KB 没写）\n", len(matched)-k, clocksourceMatchMax>>10)
			break
		}
		b.WriteString(line)
		b.WriteByte('\n')
		written += len(line) + 1
	}
	full := strings.ReplaceAll(res.Out, "\r\n", "\n")
	head := fmt.Sprintf("\n=== %s 全文（%d 字节）===\n", clocksourceKernelCommand, len(full))
	room := clocksourceKernelMax - b.Len() - len(head) - 128
	b.WriteString(head)
	if len(full) > room {
		cut := max(room, 0)
		for cut > 0 && (full[cut]&0xC0) == 0x80 {
			cut--
		}
		b.WriteString(full[:cut])
		fmt.Fprintf(&b, "\n……（超过 %d MB，后面 %d 字节没写）\n", clocksourceKernelMax>>20, len(full)-cut)
	} else {
		b.WriteString(full)
	}
	dir := m.Root.LogsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", reasons, err
	}
	path = filepath.Join(dir, root.AvdName(i)+"-"+now.Format("20060102-150405")+clocksourceSuffix)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", reasons, err
	}
	pruneClocksourceLogs(dir, i, clocksourceKernelKeep)
	return path, reasons, nil
}

// pruneClocksourceLogs keeps the newest keep saved kernel logs of instance i (by the time in the name); best effort.
func pruneClocksourceLogs(dir string, i, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	re := regexp.MustCompile(`^` + regexp.QuoteMeta(root.AvdName(i)) + `-[0-9]{8}-[0-9]{6}` + regexp.QuoteMeta(clocksourceSuffix) + `$`)
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && re.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	for _, n := range names[min(keep, len(names)):] {
		os.Remove(filepath.Join(dir, n))
	}
}
