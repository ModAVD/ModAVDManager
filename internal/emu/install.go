//go:build windows

package emu

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/apk"
	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/win"
)

// apkInstallTimeout: game packages are 1-3 GB; push + install on a slow disk takes minutes (AUTO-MAS
// APK_INSTALL_TIMEOUT_SECONDS).
const apkInstallTimeout = 900 * time.Second

// installGuestLockWait / installGuestLockPoll: how long and how often an install waits for the guest lock while a
// start mounts the guest Vulkan fix (a zygote restart takes well under that). Variables for tests.
var (
	installGuestLockWait = 30 * time.Second
	installGuestLockPoll = 200 * time.Millisecond
)

// InstallAPK is `adb install -r <apk>` on a booted instance (timeout 900 s: game packages are 1-3 GB).
// errs.Unsupported when the instance is not online; errs.Internal with adb's output when the install fails; errs.Busy
// when a start keeps mounting the guest Vulkan fix for longer than installGuestLockWait.
//
// The whole install, from the online check until adb install returns, holds the guest lock (guestLockPath): a start
// that owes this boot's Vulkan fix mounts it only when it can take that lock, and the mount restarts zygote, which
// would kill the package manager under a running install (it fails half way, or the package is left half installed).
//
// Only a single .apk (same package: replaced, data kept); .xapk / split packages (install-multiple) are not
// supported, like AUTO-MAS install_avd_apk / install_apk. A relative path is taken relative to our working directory
// (the CLI's); AUTO-MAS refuses those because its backend has no meaningful one.
func (m *Manager) InstallAPK(ctx context.Context, i int, apkPath string) (*InstallResult, error) {
	path, err := checkAPKPath(apkPath)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	guestLock, err := m.lockGuestForInstall(ctx, i)
	if err != nil {
		return nil, err
	}
	// Released right after adb install below; the deferred Unlock covers the early returns (it is idempotent).
	defer guestLock.Unlock()
	_, running := m.qemuOf(i)
	if !running || !m.bootCompleted(ctx, i, statusAdbTimeout, true) {
		if err := ctxErr(ctx); err != nil {
			return nil, err
		}
		return nil, errs.Newf(errs.Unsupported, "魔改 AVD 实例 %d 没有开机，请先开机再安装 APK", i)
	}

	m.Log.Infof("魔改 AVD 实例 %d 安装 APK: %s", i, path)
	res := m.ADB.Run(ctx, adb.RunOptions{Serial: root.Serial(i), Timeout: apkInstallTimeout, Reregister: true},
		"install", "-r", path)
	// The guest is done with the install: a waiting start may restart zygote now (reading the manifest below is local).
	guestLock.Unlock()
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	last := installResultLine(res.Out)
	if res.Code != 0 || !strings.HasPrefix(last, "Success") {
		m.Log.Warnf("魔改 AVD 实例 %d 安装 %s 失败: %s", i, name, res.Out)
		why := last
		if why == "" {
			why = "adb 返回 " + strconv.Itoa(res.Code)
		}
		return nil, errs.Newf(errs.Internal, "安装 %s 失败：%s", name, why)
	}
	m.Log.Infof("魔改 AVD 实例 %d 已安装 %s", i, name)

	out := &InstallResult{Index: i, APK: path, Output: last}
	if man, err := apk.ReadManifest(path); err == nil && man != nil {
		out.Package = man.Package
	}
	return out, nil
}

// lockGuestForInstall takes the guest lock of instance i (TryLock on guestLockPath), waiting up to
// installGuestLockWait while a start holds it to mount the guest Vulkan fix. errs.Busy after that, errs.Canceled on a
// cancel, errs.Internal when the lock file cannot be opened.
func (m *Manager) lockGuestForInstall(ctx context.Context, i int) (*win.FileLock, error) {
	path := m.guestLockPath(i)
	deadline := time.Now().Add(installGuestLockWait)
	logged := false
	for {
		l, ok, err := win.TryLock(path)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, err, "实例锁文件打不开: "+path)
		}
		if ok {
			return l, nil
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, errs.Newf(errs.Busy, "实例 %d 正在挂驱动补丁，稍后再装", i).
				WithAdvice("挂补丁会重启安卓框架，装到一半会失败；等它挂完（start 返回）再装")
		}
		if !logged {
			m.Log.Infof("实例 %d 正在挂驱动补丁（会重启安卓框架），等它挂完再装（最多 %.0f 秒）", i,
				installGuestLockWait.Seconds())
			logged = true
		}
		select {
		case <-ctx.Done():
			return nil, ctxErr(ctx)
		case <-time.After(min(installGuestLockPoll, left)):
		}
	}
}

// checkAPKPath validates like AUTO-MAS install_avd_apk and returns the absolute path.
func checkAPKPath(apkPath string) (string, error) {
	text := strings.Trim(strings.TrimSpace(apkPath), `"`)
	if text == "" {
		return "", errs.New(errs.InvalidArgument, "请选择要安装的 APK 文件")
	}
	abs, err := filepath.Abs(text)
	if err != nil {
		return "", errs.Wrap(errs.InvalidArgument, err, "请给出安装包的完整路径（"+text+"）")
	}
	if !strings.EqualFold(filepath.Ext(abs), ".apk") {
		return "", errs.Newf(errs.InvalidArgument, "%s 不是 .apk 安装包（.xapk、拆分安装包暂不支持）", filepath.Base(abs))
	}
	if st, err := os.Stat(abs); err != nil || st.IsDir() {
		return "", errs.Newf(errs.InvalidArgument, "找不到安装包 %s", abs)
	}
	return abs, nil
}

// installResultLine is the last non-empty line, trimmed.
func installResultLine(s string) string {
	lines := strings.Split(s, "\n")
	for k := len(lines) - 1; k >= 0; k-- {
		if t := strings.TrimSpace(lines[k]); t != "" {
			return t
		}
	}
	return ""
}
