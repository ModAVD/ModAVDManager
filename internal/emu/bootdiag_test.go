package emu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/testutil"
)

// TestCollectBootDiag: every item gets its file (also what a command that timed out or failed wrote), an item without
// output gets none, summary.txt says what came of each; each shell command reaches adb as one argument, logcat with
// its own longer limit.
func TestCollectBootDiag(t *testing.T) {
	dir := testutil.TempDir(t)
	type call struct {
		args    []string
		timeout time.Duration
	}
	var calls []call
	const logcat, dmesg = "11-04 10:00:00.000 E zygote: CANNOT LINK EXECUTABLE\n", "[    0.000000] Linux version\n"
	run := func(ctx context.Context, w io.Writer, timeout time.Duration, args ...string) adb.Result {
		calls = append(calls, call{args, timeout})
		switch {
		case args[0] == "logcat":
			io.WriteString(w, logcat)
			return adb.Result{Code: -1, Out: "adb logcat -d -b all 超时（15 秒）"}
		case strings.Contains(args[1], "dmesg"):
			io.WriteString(w, dmesg)
			return adb.Result{Code: 1, Out: "su: not found"}
		case args[1] == "ps -A":
			return adb.Result{Code: 1, Out: "error: closed"}
		case strings.HasPrefix(args[1], "dumpsys"):
			return adb.Result{Out: "Can't find service: activity"}
		}
		io.WriteString(w, "ok\n")
		return adb.Result{}
	}
	if !collectBootDiag(context.Background(), dir, "实例 6 开机失败", time.Minute, run) {
		t.Fatal("collectBootDiag = false")
	}
	var got [][]string
	for _, c := range calls {
		got = append(got, c.args)
	}
	want := [][]string{
		{"logcat", "-d", "-b", "all"},
		{"shell", "su 0 dmesg || dmesg"},
		{"shell", "getprop"},
		{"shell", "ps -A"},
		{"shell", "cat /proc/meminfo"},
		{"shell", "dumpsys activity processes | head -n 300"},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("adb calls\n%q\nwant\n%q", got, want)
	}
	if calls[0].timeout != 15*time.Second || calls[1].timeout != 5*time.Second {
		t.Errorf("limits %v, %v", calls[0].timeout, calls[1].timeout)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		return string(b)
	}
	if s := read("logcat.txt"); !strings.Contains(s, "CANNOT LINK") {
		t.Errorf("logcat.txt (timed out) = %q", s)
	}
	if s := read("dmesg.txt"); !strings.Contains(s, "Linux version") {
		t.Errorf("dmesg.txt (exit code 1) = %q", s)
	}
	if _, err := os.Stat(filepath.Join(dir, "ps.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ps.txt without output: %v", err)
	}
	summary := read("summary.txt")
	for _, line := range []string{
		"实例 6 开机失败",
		"可能带账号信息",
		fmt.Sprintf("logcat.txt：退出码 -1（adb logcat -d -b all 超时（15 秒）），收到 %d 字节", len(logcat)),
		fmt.Sprintf("dmesg.txt：退出码 1（su: not found），收到 %d 字节", len(dmesg)),
		"ps.txt：退出码 1（error: closed），收到 0 字节",
		"getprop.txt：成功，3 字节",
		"dumpsys-activity-processes.txt：成功，0 字节（Can't find service: activity）",
	} {
		if !strings.Contains(summary, line) {
			t.Errorf("summary.txt has no %q:\n%s", line, summary)
		}
	}
}

// TestCollectBootDiagTotal: once the whole limit is used up, the items left are not run; each item's limit is what is
// left of the whole when that is shorter. The clock is a fake one that only each run moves (150 ms), so a busy CI
// runner cannot change how many items run (ModAVD/ModAVD#142).
func TestCollectBootDiagTotal(t *testing.T) {
	dir := testutil.TempDir(t)
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	saved := bootDiagNow
	bootDiagNow = func() time.Time { return now }
	t.Cleanup(func() { bootDiagNow = saved })
	var limits []time.Duration
	run := func(ctx context.Context, w io.Writer, timeout time.Duration, args ...string) adb.Result {
		limits = append(limits, timeout)
		io.WriteString(w, "x")
		now = now.Add(150 * time.Millisecond)
		return adb.Result{}
	}
	if !collectBootDiag(context.Background(), dir, "h", 200*time.Millisecond, run) {
		t.Fatal("collectBootDiag = false")
	}
	if want := []time.Duration{200 * time.Millisecond, 50 * time.Millisecond}; !slices.Equal(limits, want) {
		t.Fatalf("limits %v, want %v", limits, want)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "summary.txt"))
	if n := strings.Count(string(b), "没收（总时限到了）"); n != len(bootDiagItems)-2 {
		t.Errorf("%d items not run, want %d:\n%s", n, len(bootDiagItems)-2, b)
	}
}

// TestCollectBootDiagNothing: no item gave anything.
func TestCollectBootDiagNothing(t *testing.T) {
	run := func(ctx context.Context, w io.Writer, timeout time.Duration, args ...string) adb.Result {
		return adb.Result{Code: -1, Out: "error: device offline"}
	}
	if collectBootDiag(context.Background(), testutil.TempDir(t), "h", time.Minute, run) {
		t.Fatal("collectBootDiag = true without any output")
	}
}

// TestBootFailed: a cancel is not a failed boot; a deadline that ran out (create's own) is.
func TestBootFailed(t *testing.T) {
	failure := errs.New(errs.Timeout, "启动超时")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithTimeout(context.Background(), -time.Second)
	defer cancel2()
	for _, c := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"failed", context.Background(), failure, true},
		{"no error", context.Background(), nil, false},
		{"canceled", canceled, errs.Wrap(errs.Canceled, context.Canceled, "已取消"), false},
		{"canceled, other error", canceled, failure, false},
		{"deadline", expired, errs.Wrap(errs.Canceled, context.DeadlineExceeded, "已取消"), true},
	} {
		if got := bootFailed(c.ctx, c.err); got != c.want {
			t.Errorf("%s: bootFailed = %v", c.name, got)
		}
	}
}
