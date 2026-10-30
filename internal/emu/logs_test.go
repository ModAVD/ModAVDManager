//go:build windows

package emu

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"modavd/internal/logx"
	"modavd/internal/win"
)

// mkLog writes a file of size bytes with mtime base + age minutes.
func mkLog(t *testing.T, dir, name string, size int, minute int) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local).Add(time.Duration(minute) * time.Minute)
	if err := os.Chtimes(path, mt, mt); err != nil {
		t.Fatal(err)
	}
	return path
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestPruneLogDirPerInstance(t *testing.T) {
	dir := t.TempDir()
	for k := 0; k < 12; k++ {
		mkLog(t, dir, fmt.Sprintf("mas_0-20261028-1200%02d.log", k), 10, k)
	}
	for k := 0; k < 11; k++ {
		mkLog(t, dir, fmt.Sprintf("logcat-mas_0-20261028-1200%02d.txt", k), 20, k)
	}
	for k := 0; k < 12; k++ {
		mkLog(t, dir, fmt.Sprintf("mas_1-20261028-1200%02d.log", k), 10, k) // another instance: untouched
	}
	mkLog(t, dir, "mas_0.ini", 1, -100)           // not ours
	mkLog(t, dir, "notes-mas_0-old.txt", 1, -100) // not ours
	// the oldest emulator log of mas_0 is still open (an emulator writing it): skipped, not counted as removed
	held, err := os.OpenFile(filepath.Join(dir, "mas_0-20261028-120000.log"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	removed, freed, err := pruneLogDir(dir, 0, 10, 1<<30, logx.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 || freed != 10+20 {
		t.Errorf("removed %d freed %d, want 2 / 30", removed, freed)
	}
	got := names(t, dir)
	for _, gone := range []string{"mas_0-20261028-120001.log", "logcat-mas_0-20261028-120000.txt"} {
		for _, n := range got {
			if n == gone {
				t.Errorf("%s should be deleted", gone)
			}
		}
	}
	count := map[string]int{}
	for _, n := range got {
		switch {
		case len(n) > 6 && n[:6] == "mas_0-":
			count["mas_0"]++
		case len(n) > 13 && n[:13] == "logcat-mas_0-":
			count["logcat"]++
		case len(n) > 6 && n[:6] == "mas_1-":
			count["mas_1"]++
		default:
			count["other"]++
		}
	}
	// 10 kept + the open one
	want := map[string]int{"mas_0": 11, "logcat": 10, "mas_1": 12, "other": 2}
	if !reflect.DeepEqual(count, want) {
		t.Errorf("left %v, want %v (%q)", count, want, got)
	}
}

func TestPruneLogDirTotalSize(t *testing.T) {
	dir := t.TempDir()
	// oldest first: a(100) b(100) c(100) d(100, open) e(100); limit 250
	mkLog(t, dir, "mas_1-a.log", 100, 1)
	mkLog(t, dir, "logcat-mas_2-b.txt", 100, 2)
	mkLog(t, dir, "mas_3-c.log", 100, 3)
	open := mkLog(t, dir, "logcat-mas_4-d.txt", 100, 4)
	mkLog(t, dir, "mas_0-e.log", 100, 5)
	mkLog(t, dir, "big-unrelated.bin", 5000, 0) // never counted, never deleted
	held, err := os.OpenFile(open, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	// closed files: a b c e = 400 > 250 -> delete a, b (300 -> 200); the open d is not counted
	removed, freed, err := pruneLogDir(dir, 0, 10, 250, logx.Nop())
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 || freed != 200 {
		t.Errorf("removed %d freed %d, want 2 / 200", removed, freed)
	}
	want := []string{"big-unrelated.bin", "logcat-mas_4-d.txt", "mas_0-e.log", "mas_3-c.log"}
	if got := names(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("left %q, want %q", got, want)
	}

	// under the limit: nothing happens
	removed, _, err = pruneLogDir(dir, 0, 10, 1<<30, logx.Nop())
	if err != nil || removed != 0 {
		t.Errorf("second run removed %d, %v", removed, err)
	}
}

func TestPruneLogDirMissing(t *testing.T) {
	removed, freed, err := pruneLogDir(filepath.Join(t.TempDir(), "logs"), 0, 10, 1<<30, logx.Nop())
	if removed != 0 || freed != 0 || err != nil {
		t.Errorf("missing dir: %d %d %v", removed, freed, err)
	}
}

func TestPruneLogDirLogsRemoval(t *testing.T) {
	dir := t.TempDir()
	for k := 0; k < 11; k++ {
		mkLog(t, dir, fmt.Sprintf("mas_6-x%02d.log", k), 1<<20, k)
	}
	var msgs []string
	log := logx.New(func(level logx.Level, msg string) {
		if level >= logx.Info {
			msgs = append(msgs, msg)
		}
	})
	if removed, _, _ := pruneLogDir(dir, 6, 10, 1<<30, log); removed != 1 {
		t.Fatalf("removed %d", removed)
	}
	want := fmt.Sprintf("实例 6 建新日志前清理旧日志 1 份，释放 1.0 MB（%s）", dir)
	if len(msgs) != 1 || msgs[0] != want {
		t.Errorf("log %q, want %q", msgs, want)
	}
}

func TestFileInUse(t *testing.T) {
	dir := t.TempDir()
	path := mkLog(t, dir, "mas_0-x.log", 5, 0)
	if busy, gone := fileInUse(path); busy || gone {
		t.Errorf("closed file: busy %v gone %v", busy, gone)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if busy, gone := fileInUse(path); !busy || gone {
		t.Errorf("open file: busy %v gone %v", busy, gone)
	}
	f.Close()
	if b, err := os.ReadFile(path); err != nil || len(b) != 5 {
		t.Errorf("the check changed the file: %d bytes, %v", len(b), err)
	}
	if busy, gone := fileInUse(filepath.Join(dir, "missing.log")); busy || !gone {
		t.Errorf("missing file: busy %v gone %v", busy, gone)
	}
}

func TestManagedLogNames(t *testing.T) {
	for name, want := range map[string]bool{
		"mas_0-20261028-150405.log":        true,
		"mas_12-x.log":                     true,
		"logcat-mas_3-20261028-150405.txt": true,
		"mas_0.log":                        false,
		"mas_-x.log":                       false,
		"logcat-mas_3-.txt":                false,
		"emu-20000-x.log":                  false,
		"logcat-20000-x.txt":               false,
		"mas_0-x.log.bak":                  false,
	} {
		if got := isManagedLog(name); got != want {
			t.Errorf("isManagedLog(%q) = %v", name, got)
		}
	}
}

func TestOpenLogFile(t *testing.T) {
	dir := t.TempDir()
	mkLog(t, dir, "logcat-mas_2-a.txt", 1, 1)
	open := mkLog(t, dir, "logcat-mas_2-b.txt", 1, 2)
	mkLog(t, dir, "logcat-mas_2-c.txt", 1, 3) // newest, closed
	emu := mkLog(t, dir, "mas_2-20261028-120000.log", 1, 4)
	mkLog(t, dir, "mas_20-20261028-120000.log", 1, 5) // mas_20 is not mas_2
	if got := openLogFile(dir, "logcat-mas_2-", ".txt"); got != "" {
		t.Errorf("nothing open: %q", got)
	}
	for _, path := range []string{open, emu, filepath.Join(dir, "mas_20-20261028-120000.log")} {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
	}
	if got := openLogFile(dir, "logcat-mas_2-", ".txt"); got != open {
		t.Errorf("open capture = %q, want %q", got, open)
	}
	if got := openLogFile(dir, "mas_2-", ".log"); got != emu {
		t.Errorf("open emulator log = %q, want %q", got, emu)
	}
	if got := openLogFile(dir, "logcat-mas_3-", ".txt"); got != "" {
		t.Errorf("other instance: %q", got)
	}
}

// TestSplitLogcatClients: a capture client older than the private server now listening talks to a server that is
// gone, unless it is the client that started this server (the server is its child then).
func TestSplitLogcatClients(t *testing.T) {
	server := win.Process{PID: 500, PPID: 77, Name: "adb.exe", Created: 1000}
	old := win.Process{PID: 10, Name: "adb.exe", Created: 900}
	starter := win.Process{PID: 77, Name: "adb.exe", Created: 990}
	newer := win.Process{PID: 11, Name: "adb.exe", Created: 1100}
	unknown := win.Process{PID: 12, Name: "adb.exe"}
	live, stale := splitLogcatClients([]win.Process{old, starter, newer, unknown}, server)
	if !reflect.DeepEqual(live, []win.Process{starter, newer, unknown}) || !reflect.DeepEqual(stale, []win.Process{old}) {
		t.Errorf("live %+v stale %+v", live, stale)
	}
	// no server known (none listening, listener table unreadable, not adb): nothing counts as stale
	live, stale = splitLogcatClients([]win.Process{old, newer}, win.Process{})
	if len(live) != 2 || len(stale) != 0 {
		t.Errorf("no server: live %+v stale %+v", live, stale)
	}
	if live, stale = splitLogcatClients(nil, server); live != nil || stale != nil {
		t.Errorf("no clients: %+v %+v", live, stale)
	}
	if got := pidList([]win.Process{old, newer}); got != "10, 11" {
		t.Errorf("pidList = %q", got)
	}
}

// TestEndProcesses: processes already gone (or never there) neither fail nor make it wait.
func TestEndProcesses(t *testing.T) {
	start := time.Now()
	// PID 0 is never a process; a PID this high does not exist (Windows hands out small multiples of 4)
	endProcesses([]win.Process{{PID: 0}, {PID: 0xFFFFFFF0, Created: 1}}, 5*time.Second)
	if time.Since(start) > 3*time.Second {
		t.Errorf("waited %v for processes that are gone", time.Since(start))
	}
}

func TestLogcatCommandLine(t *testing.T) {
	args := logcatArgs(20050, "emulator-20030")
	want := []string{"-P", "20050", "-s", "emulator-20030", "logcat", "-v", "threadtime", "-b", "main,system,crash,events"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("logcatArgs = %q", args)
	}
	cmd := append([]string{`C:\ModAVD\sdk\platform-tools\adb.exe`}, args...)
	if !logcatCmdlineMatches(cmd, "20050", "emulator-20030") {
		t.Error("own capture not recognized")
	}
	for name, c := range map[string][]string{
		"other port":   {"adb.exe", "-P", "20089", "-s", "emulator-20030", "logcat"},
		"other serial": {"adb.exe", "-P", "20050", "-s", "emulator-20000", "logcat"},
		"not logcat":   {"adb.exe", "-P", "20050", "-s", "emulator-20030", "shell", "getprop"},
		"no -P":        {"adb.exe", "-s", "emulator-20030", "logcat"},
	} {
		if logcatCmdlineMatches(c, "20050", "emulator-20030") {
			t.Errorf("%s matched", name)
		}
	}
}
