package emu

// The guest's state when a boot fails (ModAVD/ModAVD#79). A start whose boot timed out, whose emulator exited while
// qemu stayed, or whose after-boot setup failed (zygote not coming up ...) saves logcat, dmesg, getprop, the process
// list, meminfo and the activity manager's processes into logs\mas_<i>-<time>-bootfail\ before it returns: the
// emulator's own log does not say why Android did not come up, and a new instance whose first boot failed is often
// deleted right after, its data disk with it. The error's details name the directory (errs.BootDiagDir).
//
// Not for a boot the caller gave up on (a cancel), nor one being stopped, nor once qemu is gone (there is no guest
// left to ask). Every command has its deadline and the whole is capped, so a broken guest does not keep a failed start
// much longer. The files stay on this computer (logcat may name accounts); collect-info packs the newest of them.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/root"
)

const (
	bootDiagSuffix = "-bootfail"
	bootDiagTotal  = 30 * time.Second // the whole collection
	bootDiagKeep   = 3                // per instance (pruneLogDir)
)

// bootDiagItem is one file of the directory: adb arguments after -s <serial>, given at most timeout.
type bootDiagItem struct {
	file    string
	args    []string
	timeout time.Duration
}

// Each shell command is one argument: adb joins its arguments with spaces and the guest's sh parses the line, so a
// pipe arrives as written. dmesg wants root on some images; these userdebug ones allow su 0 (see Manager.su).
var bootDiagItems = []bootDiagItem{
	{"logcat.txt", []string{"logcat", "-d", "-b", "all"}, 15 * time.Second},
	{"dmesg.txt", []string{"shell", "su 0 dmesg || dmesg"}, 5 * time.Second},
	{"getprop.txt", []string{"shell", "getprop"}, 5 * time.Second},
	{"ps.txt", []string{"shell", "ps -A"}, 5 * time.Second},
	{"meminfo.txt", []string{"shell", "cat /proc/meminfo"}, 5 * time.Second},
	{"dumpsys-activity-processes.txt", []string{"shell", "dumpsys activity processes | head -n 300"}, 5 * time.Second},
}

// bootDiagRun runs adb args for at most timeout, stdout into w (kept when adb times out or fails).
type bootDiagRun func(ctx context.Context, w io.Writer, timeout time.Duration, args ...string) adb.Result

// bootFailed: err ends a boot that went wrong, not one the caller gave up on. A cancel (Ctrl+C, MCP
// notifications/cancelled, the manager's job) is not one; create's own deadline running out is.
func bootFailed(ctx context.Context, err error) bool {
	return err != nil && !errors.Is(ctx.Err(), context.Canceled)
}

// saveBootFailure is err, with the directory in its details when the guest state of instance i was saved (see the
// file comment).
func (m *Manager) saveBootFailure(ctx context.Context, i int, err error, running bool) error {
	if !running || !bootFailed(ctx, err) || m.stopping(i) {
		return err
	}
	dir := m.collectBootFailure(i, err)
	if dir == "" {
		return err
	}
	m.Log.Warnf("实例 %d 开机失败，客体现场存在 %s", i, dir)
	return errs.WithDetail(err, errs.BootDiagDir, dir)
}

// collectBootFailure saves the guest state of instance i into a new logs\mas_<i>-<time>-bootfail; "" when nothing
// could be read (the directory is removed again).
func (m *Manager) collectBootFailure(i int, cause error) string {
	logs := m.Root.LogsDir()
	dir := filepath.Join(logs, root.AvdName(i)+"-"+time.Now().Format("20060102-150405")+bootDiagSuffix)
	if err := os.MkdirAll(logs, 0o755); err != nil {
		m.Log.Warnf("实例 %d 开机失败，建不了日志目录，不收客体现场: %v", i, err)
		return ""
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		m.Log.Warnf("实例 %d 开机失败，建不了 %s，不收客体现场: %v", i, filepath.Base(dir), err)
		return ""
	}
	serial := m.Root.Ports().Serial(i)
	run := func(ctx context.Context, w io.Writer, timeout time.Duration, args ...string) adb.Result {
		return m.ADB.Run(ctx, adb.RunOptions{Serial: serial, Timeout: timeout, Stdout: w}, args...)
	}
	e := errs.As(cause)
	header := fmt.Sprintf("实例 %d（%s）开机失败时的客体现场，%s\n错误（%s）：%s", i, serial,
		time.Now().Format("2006-01-02 15:04:05"), e.Code, e.Message)
	// a fresh context: create's own may be the one that ran out
	if !collectBootDiag(context.Background(), dir, header, bootDiagTotal, run) {
		os.RemoveAll(dir)
		m.Log.Warnf("实例 %d 开机失败，客体现场一项也没读到", i)
		return ""
	}
	m.pruneLogs(i)
	return dir
}

// collectBootDiag writes each of bootDiagItems into dir (whatever came, also from a command that timed out or failed;
// an empty file is removed) and summary.txt, one line per item; all of it within total. False when no item gave
// anything.
func collectBootDiag(ctx context.Context, dir, header string, total time.Duration, run bootDiagRun) bool {
	deadline := time.Now().Add(total)
	lines := []string{header, "这些文件可能带账号信息，只存在本机；要发给别人时按问题包的规矩来。", ""}
	got := false
	for _, it := range bootDiagItems {
		left := time.Until(deadline)
		if left <= 0 || ctx.Err() != nil {
			lines = append(lines, it.file+"：没收（总时限到了）")
			continue
		}
		path := filepath.Join(dir, it.file)
		f, err := os.Create(path)
		if err != nil {
			lines = append(lines, it.file+"：建不了文件："+err.Error())
			continue
		}
		res := run(ctx, f, min(it.timeout, left), it.args...)
		f.Close()
		var n int64
		if st, err := os.Stat(path); err == nil {
			n = st.Size()
		}
		if n == 0 {
			os.Remove(path)
		} else {
			got = true
		}
		lines = append(lines, it.file+"："+bootDiagStatus(res, n))
	}
	os.WriteFile(filepath.Join(dir, "summary.txt"), []byte(strings.Join(lines, "\r\n")+"\r\n"), 0o644)
	return got
}

// bootDiagStatus is an item's line of summary.txt; res.Out is adb's stderr (or why adb did not run): a guest early in
// its boot answers dumpsys with "Can't find service" there and exit code 0.
func bootDiagStatus(res adb.Result, n int64) string {
	why := strings.Join(strings.Fields(res.Out), " ")
	if res.Code == 0 {
		if why != "" {
			return fmt.Sprintf("成功，%d 字节（%s）", n, why)
		}
		return fmt.Sprintf("成功，%d 字节", n)
	}
	if why == "" {
		why = "没有说明"
	}
	return fmt.Sprintf("退出码 %d（%s），收到 %d 字节", res.Code, why, n)
}
