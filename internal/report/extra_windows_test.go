package report

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setNetsh makes netsh.exe answer stdout, or fail to start with startErr.
func setNetsh(f *fixture, stdout, startErr string) {
	if f.sc.Run == nil {
		f.sc.Run = map[string]struct {
			Stdout, Stderr, StartErr string
			Exit                     int
			Timeout                  bool
		}{}
	}
	f.sc.Run["netsh.exe"] = struct {
		Stdout, Stderr, StartErr string
		Exit                     int
		Timeout                  bool
	}{Stdout: stdout, StartErr: startErr}
}

// The sections the scripts never had: the manager's logs, every adb.exe with the adb server log, the reserved ports.
func TestCollectManagerLogsAdbAndPorts(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	guardDials(t, f.adbPort)
	logs := filepath.Join(f.root, "logs")
	if err := os.WriteFile(filepath.Join(logs, "modavd-ui.log.1"), []byte("older manager log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, names, files := runCollect(t, f)
	s := summaryOf(t, files)

	wantSection := "\r\n== 管理器日志 ==\r\n管理器日志（modavd-ui.log）：12 B\r\n管理器日志（上一份）（modavd-ui.log.1）：18 B\r\n" +
		"开机守护日志（modavd-bootwatch.log）：15 B\r\n"
	if !strings.Contains(s, wantSection) {
		t.Errorf("manager logs section:\n%s", s)
	}
	for name, want := range map[string]string{
		"logs/modavd-ui.log": "manager log\n", "logs/modavd-ui.log.1": "older manager log\n",
		"logs/modavd-bootwatch.log": "boot watch log\n", "adb/adb.log": "--- adb starting (pid 4242) ---\n",
	} {
		if got, ok := files[name]; !ok || string(got) != want {
			t.Errorf("%s: %q (in zip %v), want %q", name, got, ok, want)
		}
	}
	if hasEntryPrefix(names, "logs/modavd-bootwatch.log.1") {
		t.Error("a missing .1 backup in the zip")
	}

	adbLog := filepath.Join(f.env["TEMP"], "adb.log")
	if !strings.Contains(s, "\r\n== adb.exe 进程与 adb server 日志 ==\r\nadb.exe 进程：2 个\r\n") ||
		!strings.Contains(s, "  pid 4242  启动于 2026-01-15 11:57:00  （路径读不到）\r\n    adb -L tcp:fake fork-server server\r\n") ||
		!strings.Contains(s, `  X:\fake\other\adb.exe`+"\r\n") ||
		!strings.Contains(s, "adb server 日志（"+adbLog+"）：32 B\r\n") {
		t.Errorf("adb section:\n%s", s)
	}

	if !strings.Contains(s, "\r\n== Windows 保留的 TCP 端口（netsh） ==\r\n") ||
		!strings.Contains(s, "\r\n       50000       50059     *\r\n") || !strings.Contains(s, "\r\n本目录要用的端口都不在保留范围里\r\n") {
		t.Errorf("ports section:\n%s", s)
	}
	// none of them failed or has a timing line
	for _, name := range []string{sectionManagerLogs, sectionAdbProcesses, sectionPortExcluded} {
		if strings.Contains(s, "\r\n  "+name+"：") || strings.Contains(strings.Join(res.Problems, "\n"), name+"：") {
			t.Errorf("%s: timing line or problem %q", name, res.Problems)
		}
	}
}

// Only the end of a long log is taken, and the summary says how much of how much.
func TestCollectManagerLogTail(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	guardDials(t, f.adbPort)
	old := managerLogTailBytes
	managerLogTailBytes = 4
	t.Cleanup(func() { managerLogTailBytes = old })
	_, _, files := runCollect(t, f)
	if got := string(files["logs/modavd-ui.log"]); got != "log\n" {
		t.Errorf("tail %q", got)
	}
	if s := summaryOf(t, files); !strings.Contains(s, "管理器日志（modavd-ui.log）：12 B，只取了最后 4 B\r\n") {
		t.Errorf("summary:\n%s", s)
	}
}

// A port of this root inside a reserved range is named; netsh failing to start is a failed section, not a stop.
func TestCollectPortExclusions(t *testing.T) {
	useGoldenZone(t, "full")
	f := newFixture(t, "full", testTempDir(t), "")
	guardDials(t, f.adbPort)
	lo, hi := f.adbPort-3, f.adbPort+3
	setNetsh(f, fmt.Sprintf("\r\n协议 tcp 端口排除范围\r\n\r\n开始端口    结束端口\r\n----------    --------\r\n     %d       %d\r\n\r\n", lo, hi), "")
	_, _, files := runCollect(t, f)
	want := fmt.Sprintf("警告：本目录要用的端口被 Windows 保留了，adb server 或模拟器在这些端口上监听不了：%d（私有 adb server）在 %d–%d 里\r\n",
		f.adbPort, lo, hi)
	if s := summaryOf(t, files); !strings.Contains(s, want) {
		t.Errorf("want %q in:\n%s", want, s)
	}

	f = newFixture(t, "full", testTempDir(t), "")
	guardDials(t, f.adbPort)
	setNetsh(f, "", "netsh is gone")
	res, _, files := runCollect(t, f)
	s := summaryOf(t, files)
	if !strings.Contains(s, "\r\n== Windows 保留的 TCP 端口（netsh） ==\r\n[本节出错] Windows 保留的 TCP 端口（netsh）：netsh is gone\r\n") ||
		!strings.Contains(strings.Join(res.Problems, "\n"), "Windows 保留的 TCP 端口（netsh）：netsh is gone") ||
		!strings.Contains(s, "\r\n== 模拟器日志（logs\\） ==\r\n") {
		t.Errorf("problems %q, summary:\n%s", res.Problems, s)
	}
}
