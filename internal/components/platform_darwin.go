//go:build darwin && arm64 && cgo

package components

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

const nativeDarwinComponents = true

// A component install owns these real directories. Following a mutable SDK,
// staging, log or download link could write outside the selected package.
func (f *fetcher) platformPaths() error {
	paths := []string{f.sdk, f.stagingRoot(), filepath.Join(f.r.Dir, "downloads"), f.r.LogsDir()}
	files := []string{
		filepath.Join(f.r.LogsDir(), "fetch-components.log"),
		filepath.Join(f.r.LogsDir(), "fetch-components.lock"),
		filepath.Join(f.r.Dir, "downloads", licenseFile),
		filepath.Join(f.r.Dir, "downloads", "android-sdk-arm-dbt-license.txt"),
	}
	specs := componentCatalogue()
	if f.o.PlayStore {
		specs = append(append([]root.SDKComponent(nil), specs...), playStoreCatalogue())
	}
	for _, c := range specs {
		paths = append(paths, c.Dir(f.sdk))
		files = append(files, f.zipPath(c), f.zipPath(c)+".part")
		if c.ID == playStoreCatalogue().ID {
			staging := filepath.Join(f.stagingRoot(), c.ID)
			paths = append(paths, staging, filepath.Join(staging, root.PlayStoreDir))
			files = append(files, filepath.Join(staging, "source.zip"), filepath.Join(staging, "system.img"))
		}
	}
	for _, p := range paths {
		rel, err := filepath.Rel(f.r.Dir, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return rejected("组件目录不在包根目录内：%s", p)
		}
		current := f.r.Dir
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			st, err := os.Lstat(current)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return errs.Wrap(errs.Internal, err, "读不了组件目录："+current)
			}
			if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
				return rejected("组件目录必须是真实目录，不能是链接或文件：%s", current)
			}
		}
	}
	for _, p := range files {
		st, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return errs.Wrap(errs.Internal, err, "读不了组件文件："+p)
		}
		if !st.Mode().IsRegular() {
			return rejected("组件文件必须是普通文件，不能是链接或目录：%s", p)
		}
	}
	return nil
}

// Check before the download and again immediately before publishing. Never stop
// the user's emulator or private adb server to make a repair possible.
func (f *fetcher) platformIdle() error {
	processes, err := host.ListProcesses()
	if err != nil {
		return errs.Wrap(errs.Internal, err, "无法确认 SDK 是否正在使用")
	}
	sdk, err := os.Stat(f.sdk)
	if err != nil {
		return errs.Wrap(errs.Internal, err, "无法读取 SDK 目录")
	}
	if !sdk.IsDir() {
		return errs.New(errs.Internal, "SDK 不是目录")
	}
	platform := root.CurrentPlatform()
	for _, p := range processes {
		if p.PID == uint32(os.Getpid()) || p.PID == 0 {
			continue
		}
		path, err := p.ImagePath()
		if err != nil {
			if !p.Alive() {
				continue
			}
			name := filepath.Base(p.Name)
			if name == platform.ADB || name == platform.Emulator || name == platform.QEMU || name == platform.QEMUHeadless || name == "netsimd" {
				return errs.Newf(errs.Busy, "无法确认 %s（PID %d）是否正在使用本包 SDK，暂不补全组件", name, p.PID).
					WithAdvice("请先自行关闭模拟器和私有 adb server，再重试")
			}
			continue
		}
		inside, err := executableWithinSDK(path, sdk)
		if err != nil {
			if !p.Alive() {
				continue
			}
			return errs.Wrap(errs.Busy, err, "无法确认 SDK 是否正在被 "+p.Name+" 使用，暂不补全组件").
				WithAdvice("请先自行关闭模拟器和私有 adb server，再重试")
		}
		if inside && p.Alive() {
			return errs.Newf(errs.Busy, "SDK 正在被 %s（PID %d）使用，暂不补全组件", p.Name, p.PID).
				WithAdvice("请先自行关闭模拟器和私有 adb server，再重试")
		}
	}
	return nil
}

// Compare real ancestor identities: path spelling alone cannot distinguish a
// case alias on one volume from separate directories on a case-sensitive one.
// An unreadable path is unknown, never evidence that the SDK is idle.
func executableWithinSDK(path string, sdk os.FileInfo) (bool, error) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, err
	}
	for dir := filepath.Dir(real); ; {
		info, err := os.Stat(dir)
		if err != nil {
			return false, err
		}
		if os.SameFile(info, sdk) {
			return true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}

// Count downloads and expansion on their actual volumes; SDK may be a mount
// point. Existing incomplete components stay on disk throughout staging.
func (f *fetcher) platformSpace(todo []root.SDKComponent) error {
	var download, unpack int64
	for _, c := range todo {
		have := int64(0)
		if st, err := os.Stat(f.zipPath(c)); err == nil {
			have = st.Size()
		} else if st, err := os.Stat(f.zipPath(c) + ".part"); err == nil {
			have = st.Size()
		}
		download += max(0, c.Size-have)
		unpack += c.ExtractedBytes
	}
	var rootStat, sdkStat syscall.Stat_t
	if err := syscall.Stat(f.r.Dir, &rootStat); err != nil {
		return errs.Wrap(errs.Internal, err, "查不了包根目录所在磁盘")
	}
	if err := syscall.Stat(f.sdk, &sdkStat); err != nil {
		return errs.Wrap(errs.Internal, err, "查不了 SDK 所在磁盘")
	}
	needs := []struct {
		path  string
		bytes int64
	}{{f.r.Dir, download + unpack}}
	if rootStat.Dev != sdkStat.Dev {
		needs[0].bytes = download
		needs = append(needs, struct {
			path  string
			bytes int64
		}{f.sdk, unpack})
	}
	for _, need := range needs {
		freeBytes, err := diskFree(need.path)
		if err != nil {
			return errs.Wrap(errs.Internal, err, "查不了剩余空间："+need.path)
		}
		free := int64(min(freeBytes, 1<<62))
		if mb := f.o.TestFreeSpaceMB; mb != nil {
			free = clampMB(*mb)
		}
		required := need.bytes + (500 << 20)
		if free < required {
			return errs.Newf(errs.PrecheckFailed, "磁盘空间不够：%s 需要约 %s，只剩 %s", need.path,
				formatSize(float64(required)), formatSize(float64(free))).WithAdvice("请先腾出空间，再重新补全组件")
		}
	}
	return nil
}

// Unlike POSIX rename, publishing must not replace a destination that appeared
// after the preflight. The old incomplete component remains in its aside path.
func moveComponent(src, dst string) error {
	return host.RenameNoReplace(src, dst)
}

// installComponent moves an unpacked component from the staging directory into place.
func installComponent(src, dst string) error { return moveComponent(src, dst) }
