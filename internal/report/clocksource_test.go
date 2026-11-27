package report

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/root"
	"modavd/internal/testutil"
)

// clockRoot is a root with instances mas_1 and mas_2 (config.ini only); records maps an instance to the content of
// its avd\.mas_<i>.clocksource.json.
func clockRoot(t *testing.T, records map[string]string) string {
	t.Helper()
	r := testutil.TempDir(t)
	avdDir := filepath.Join(r, root.AvdDir)
	for _, name := range []string{"mas_1", "mas_2"} {
		if err := os.MkdirAll(filepath.Join(avdDir, name+".avd"), 0o755); err != nil {
			t.Fatal(err)
		}
		for path, text := range map[string]string{
			filepath.Join(avdDir, name+".ini"):                  "path=x\n",
			filepath.Join(avdDir, name+".avd", "config.ini"):    "hw.ramSize=6144\n",
			filepath.Join(avdDir, "."+name+".clocksource.json"): records[name],
		} {
			if text == "" {
				continue
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	return r
}

func TestCollectInstanceClocksource(t *testing.T) {
	saved := time.Local
	t.Cleanup(func() { time.Local = saved })
	time.Local = time.UTC
	cases := []struct {
		name, record, want string
	}{
		{"tsc", `{"at":"2026-11-15T10:20:30Z","clocksource":"tsc","available":"tsc hpet acpi_pm","arch":"x86_64","x86":true}`,
			"  时钟源（最近一次开机，2026-11-15 10:20:30 读）：tsc"},
		{"fallback", `{"at":"2026-11-15T10:20:30Z","clocksource":"hpet","available":"tsc hpet acpi_pm","arch":"x86_64","x86":true,"kernel_log":"mas_1-20261115-102030-clocksource.txt"}`,
			`  时钟源（最近一次开机，2026-11-15 10:20:30 读）：hpet，不是 tsc（可选 tsc hpet acpi_pm），内核日志 logs\mas_1-20261115-102030-clocksource.txt`},
		{"fallback without log", `{"at":"2026-11-15T10:20:30Z","clocksource":"hpet","x86":true,"error":"su: not found"}`,
			"  时钟源（最近一次开机，2026-11-15 10:20:30 读）：hpet，不是 tsc（可选 ?），内核日志没存下"},
		{"arm64", `{"at":"2026-11-15T10:20:30Z","clocksource":"arch_sys_counter","arch":"aarch64","x86":false}`,
			"  时钟源（最近一次开机，2026-11-15 10:20:30 读）：arch_sys_counter"},
		{"unread", `{"at":"2026-11-15T10:20:30Z","clocksource":"","x86":true,"error":"offline"}`,
			"  时钟源（最近一次开机，2026-11-15 10:20:30 读）：读不到"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := clockRoot(t, map[string]string{"mas_1": c.record})
			col := &collector{root: r, stage: testutil.TempDir(t)}
			if err := col.instanceFiles(); err != nil {
				t.Fatal(err)
			}
			out := strings.Join(col.lines, "\n")
			_, after, ok := strings.Cut(out, "mas_1：hw.ramSize=6144")
			if !ok {
				t.Fatalf("no mas_1 line:\n%s", out)
			}
			if got := strings.Split(after, "\n")[1]; got != c.want {
				t.Errorf("line after mas_1 = %q\nwant %q", got, c.want)
			}
			// mas_2 has no record (not started since, older modavd): no line for it
			_, rest, _ := strings.Cut(out, "mas_2：hw.ramSize=6144")
			if strings.Contains(rest, "时钟源") {
				t.Errorf("a clock source line without a record:\n%s", out)
			}
		})
	}
}

// A broken record is no line, as no record.
func TestCollectInstanceClocksourceBroken(t *testing.T) {
	r := clockRoot(t, map[string]string{"mas_1": "{not json", "mas_2": `{"clocksource":"tsc"}`})
	col := &collector{root: r, stage: testutil.TempDir(t)}
	if err := col.instanceFiles(); err != nil {
		t.Fatal(err)
	}
	if out := strings.Join(col.lines, "\n"); strings.Contains(out, "时钟源") {
		t.Errorf("clock source line from a broken record:\n%s", out)
	}
}

// The saved kernel logs are collected, newest clocksourceLogsKeep per instance, and are no emulator log or logcat.
func TestCollectClocksourceLogs(t *testing.T) {
	r := testutil.TempDir(t)
	dir := filepath.Join(r, root.LogsDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{
		"mas_1-20261115-100000-clocksource.txt", "mas_1-20261115-110000-clocksource.txt",
		"mas_1-20261115-120000-clocksource.txt", "mas_1-20261115-130000-clocksource.txt",
		"mas_2-20261115-100000-clocksource.txt",
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := &collector{root: r, stage: testutil.TempDir(t), instances: []string{"mas_1", "mas_2"}}
	if err := c.emulatorLogs(); err != nil {
		t.Fatal(err)
	}
	out := strings.Join(c.lines, "\n")
	if !strings.Contains(out, "时钟源不是 tsc 时存的客体内核日志（*-clocksource.txt）：取了 4 份") ||
		!strings.Contains(out, "mas_1：模拟器日志 0 份，logcat 0 份") {
		t.Errorf("summary:\n%s", out)
	}
	for _, n := range names {
		_, err := os.Stat(filepath.Join(c.stage, root.LogsDir, n))
		if want := n != names[0]; (err == nil) != want {
			t.Errorf("%s copied = %v, want %v", n, err == nil, want)
		}
	}

	// none: nothing said
	c = &collector{root: r, stage: testutil.TempDir(t)}
	for _, n := range names {
		os.Remove(filepath.Join(dir, n))
	}
	if err := c.emulatorLogs(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(c.lines, "\n"), "clocksource") {
		t.Errorf("summary without files:\n%s", strings.Join(c.lines, "\n"))
	}
}
