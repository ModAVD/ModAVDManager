package emu

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/logx"
	"modavd/internal/root"
)

func TestParseClocksource(t *testing.T) {
	cases := []struct {
		out, current, available, arch string
		ok                            bool
	}{
		{"tsc\n---\ntsc hpet acpi_pm \n---\nx86_64\n", "tsc", "tsc hpet acpi_pm", "x86_64", true},
		{"hpet\r\n---\r\ntsc hpet acpi_pm\r\n---\r\nx86_64\r\n", "hpet", "tsc hpet acpi_pm", "x86_64", true},
		{"arch_sys_counter\n---\narch_sys_counter\n---\naarch64\n", "arch_sys_counter", "arch_sys_counter", "aarch64", true},
		// an error text instead of the file: unread, and uname still read
		{"cat: /sys/devices/system/clocksource/clocksource0/current_clocksource: No such file or directory\n---\n---\nx86_64\n",
			"", "", "x86_64", false},
		{"error: device offline", "", "", "", false},
		{"", "", "", "", false},
	}
	for _, c := range cases {
		current, available, arch, ok := parseClocksource(c.out)
		if current != c.current || available != c.available || arch != c.arch || ok != c.ok {
			t.Errorf("parseClocksource(%q) = %q, %q, %q, %v; want %q, %q, %q, %v", c.out, current, available, arch, ok,
				c.current, c.available, c.arch, c.ok)
		}
	}
}

func TestX86Guest(t *testing.T) {
	arm := &avd.Ini{}
	arm.Set("abi.type", "arm64-v8a")
	for _, c := range []struct {
		arch string
		cfg  *avd.Ini
		want bool
	}{
		{"x86_64", nil, true},
		{"i686", nil, true},
		{"aarch64", nil, false},
		{"armv8l", nil, false},
		{"", nil, true},  // unread, no config: the historical x86_64
		{"", arm, false}, // unread, an arm64 config
		{"x86_64", arm, true},
	} {
		if got := x86Guest(c.arch, c.cfg); got != c.want {
			t.Errorf("x86Guest(%q, %v) = %v", c.arch, c.cfg, got)
		}
	}
}

// fakeClockGuest answers the two guest commands of checkClocksource and records what was run.
type fakeClockGuest struct {
	status, kernel adb.Result
	ran            []string
}

func (g *fakeClockGuest) run(command string, _ time.Duration) adb.Result {
	g.ran = append(g.ran, command)
	switch command {
	case clocksourceScript:
		return g.status
	case clocksourceKernelCommand:
		return g.kernel
	}
	return adb.Result{Code: 127, Out: "unexpected " + command}
}

// clockLine is one recorded log line (ime_test.go's logLine is Windows-only).
type clockLine struct {
	level logx.Level
	msg   string
}

func clockManager(t *testing.T) (*Manager, *[]clockLine) {
	t.Helper()
	r, err := root.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var lines []clockLine
	m := &Manager{Root: r, Log: logx.New(func(level logx.Level, msg string) { lines = append(lines, clockLine{level, msg}) })}
	return m, &lines
}

func readClockRecord(t *testing.T, m *Manager, i int) clocksourceRecord {
	t.Helper()
	b, err := os.ReadFile(m.clocksourcePath(i))
	if err != nil {
		t.Fatal(err)
	}
	var rec clocksourceRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func clockLogs(t *testing.T, m *Manager) []string {
	t.Helper()
	entries, err := os.ReadDir(m.Root.LogsDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), clocksourceSuffix) {
			names = append(names, e.Name())
		}
	}
	return names
}

var clockNow = time.Date(2026, 11, 15, 10, 20, 30, 0, time.UTC)

// tsc on x86_64: one info line, no kernel log read, the record written.
func TestCheckClocksourceTSC(t *testing.T) {
	m, lines := clockManager(t)
	g := &fakeClockGuest{status: adb.Result{Out: "tsc\n---\ntsc hpet acpi_pm\n---\nx86_64\n"}}
	rec := m.checkClocksource(1, nil, g.run, clockNow)
	if len(g.ran) != 1 {
		t.Fatalf("ran %q", g.ran)
	}
	want := "实例 1 客体时钟源: tsc（可选 tsc hpet acpi_pm，客体 x86_64）"
	if len(*lines) != 1 || (*lines)[0].level != logx.Info || (*lines)[0].msg != want {
		t.Fatalf("log = %+v", *lines)
	}
	if got := readClockRecord(t, m, 1); got != rec || got.Clocksource != "tsc" || !got.X86 || got.KernelLog != "" ||
		got.At != "2026-11-15T10:20:30Z" {
		t.Errorf("record = %+v (returned %+v)", got, rec)
	}
	if n := clockLogs(t, m); len(n) != 0 {
		t.Errorf("kernel logs saved: %q", n)
	}
}

// arm64 has no tsc: arch_sys_counter is logged, never warned about, and its kernel log is not read.
func TestCheckClocksourceArm64(t *testing.T) {
	m, lines := clockManager(t)
	g := &fakeClockGuest{status: adb.Result{Out: "arch_sys_counter\n---\narch_sys_counter\n---\naarch64\n"}}
	rec := m.checkClocksource(2, nil, g.run, clockNow)
	if len(g.ran) != 1 || len(*lines) != 1 || (*lines)[0].level != logx.Info || rec.X86 || rec.fallback() {
		t.Fatalf("ran %q, log %+v, record %+v", g.ran, *lines, rec)
	}
}

// hpet on x86_64: a warning naming the saved file and the reason line; the file has the clock lines first, then
// the whole log.
func TestCheckClocksourceFallbackSavesKernelLog(t *testing.T) {
	m, lines := clockManager(t)
	kernel := strings.Join([]string{
		"11-15 10:19:01.000     0     0 I         : Linux version 6.1.23",
		"11-15 10:19:01.100     0     0 I         : tsc: Detected 4200.000 MHz processor",
		"11-15 10:19:02.000     0     0 I         : random: crng init done",
		"11-15 10:19:21.000     0     0 W         : clocksource: timekeeping watchdog on CPU3: hpet read-back delay of 412000ns, attempt 4, marking unstable",
		"11-15 10:19:21.001     0     0 W         : tsc: Marking TSC unstable due to clocksource watchdog",
		"11-15 10:19:21.002     0     0 I         : clocksource: Switched to clocksource hpet",
	}, "\r\n") + "\r\n"
	g := &fakeClockGuest{status: adb.Result{Out: "hpet\n---\ntsc hpet acpi_pm\n---\nx86_64\n"}, kernel: adb.Result{Out: kernel}}
	rec := m.checkClocksource(3, nil, g.run, clockNow)
	if len(g.ran) != 2 || g.ran[1] != "su 0 logcat -b kernel -d" {
		t.Fatalf("ran %q", g.ran)
	}
	name := "mas_3-" + clockNow.Format("20060102-150405") + "-clocksource.txt"
	path := filepath.Join(m.Root.LogsDir(), name)
	if len(*lines) != 1 || (*lines)[0].level != logx.Warn {
		t.Fatalf("log = %+v", *lines)
	}
	msg := (*lines)[0].msg
	for _, part := range []string{"实例 3 客体时钟源是 hpet，不是 tsc（可选 tsc hpet acpi_pm，客体 x86_64）", "已存到 " + path,
		"原因行: ['11-15 10:19:21.000", "marking unstable'", "Marking TSC unstable due to clocksource watchdog'"} {
		if !strings.Contains(msg, part) {
			t.Errorf("warning lacks %q:\n%s", part, msg)
		}
	}
	if rec.KernelLog != name || !rec.fallback() || readClockRecord(t, m, 3) != rec {
		t.Errorf("record = %+v", rec)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	filtered, full, ok := strings.Cut(text, "=== su 0 logcat -b kernel -d 全文")
	if !ok || !strings.Contains(filtered, "共 4 行") || strings.Contains(filtered, "crng init done") ||
		!strings.Contains(filtered, "Switched to clocksource hpet") || !strings.Contains(full, "crng init done") ||
		strings.Contains(text, "\r") {
		t.Errorf("saved file:\n%s", text)
	}
}

// The saved file stays within clocksourceKernelMax however long the kernel log is, and only the newest
// clocksourceKernelKeep files of the instance stay.
func TestClocksourceKernelLogBoundedAndPruned(t *testing.T) {
	m, _ := clockManager(t)
	big := strings.Repeat("11-15 10:19:01.000 0 0 I : tsc: something about the clock 测试\n", clocksourceKernelMax/40)
	g := &fakeClockGuest{status: adb.Result{Out: "hpet\n---\ntsc hpet\n---\nx86_64\n"}, kernel: adb.Result{Out: big}}
	other := filepath.Join(m.Root.LogsDir(), "mas_4-20261101-000000-clocksource.txt")
	if err := os.MkdirAll(m.Root.LogsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for k := 0; k < clocksourceKernelKeep+2; k++ {
		m.checkClocksource(3, nil, g.run, clockNow.Add(time.Duration(k)*time.Second))
	}
	names := clockLogs(t, m)
	if len(names) != clocksourceKernelKeep+1 {
		t.Fatalf("kept %q", names)
	}
	for _, n := range names {
		if n == "mas_3-20261115-102030-clocksource.txt" || n == "mas_3-20261115-102031-clocksource.txt" {
			t.Errorf("oldest not pruned: %q", names)
		}
		st, err := os.Stat(filepath.Join(m.Root.LogsDir(), n))
		if err != nil || st.Size() > clocksourceKernelMax {
			t.Errorf("%s: %v, %v bytes", n, err, st)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another instance's file pruned: %v", err)
	}
}

// Failures are one log line each and never stop anything: an unreadable clock source writes a record without one, a
// kernel log that cannot be read is named in the warning.
func TestCheckClocksourceFailures(t *testing.T) {
	m, lines := clockManager(t)
	g := &fakeClockGuest{status: adb.Result{Code: 1, Out: "error: device 'emulator-5554' not found"}}
	rec := m.checkClocksource(1, nil, g.run, clockNow)
	if len(g.ran) != 1 || len(*lines) != 1 || (*lines)[0].level != logx.Warn ||
		!strings.HasPrefix((*lines)[0].msg, "实例 1 读不到客体时钟源（error: device") || rec.Clocksource != "" ||
		readClockRecord(t, m, 1).Error == "" {
		t.Fatalf("ran %q, log %+v, record %+v", g.ran, *lines, rec)
	}

	m, lines = clockManager(t)
	g = &fakeClockGuest{status: adb.Result{Out: "hpet\n---\ntsc hpet\n---\nx86_64\n"},
		kernel: adb.Result{Code: 1, Out: "su: not found"}}
	rec = m.checkClocksource(1, nil, g.run, clockNow)
	if len(*lines) != 1 || (*lines)[0].level != logx.Warn ||
		!strings.Contains((*lines)[0].msg, "客体内核日志没存下: logcat -b kernel -d: su: not found") ||
		rec.KernelLog != "" || rec.Clocksource != "hpet" || len(clockLogs(t, m)) != 0 {
		t.Fatalf("log %+v, record %+v", *lines, rec)
	}
}

// An unwritable record (the avd path is a file) is only a debug line.
func TestCheckClocksourceRecordUnwritable(t *testing.T) {
	m, lines := clockManager(t)
	if err := os.WriteFile(m.Root.AvdHome(), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &fakeClockGuest{status: adb.Result{Out: "tsc\n---\ntsc\n---\nx86_64\n"}}
	m.checkClocksource(1, nil, g.run, clockNow)
	var warned int
	for _, l := range *lines {
		if l.level >= logx.Warn {
			warned++
		}
	}
	if warned != 0 || len(*lines) != 2 {
		t.Errorf("log = %s", fmt.Sprint(*lines))
	}
}
