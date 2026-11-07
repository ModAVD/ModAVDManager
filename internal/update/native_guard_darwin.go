//go:build darwin && cgo

package update

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

func managerPaths() []string {
	return []string{"ModAVD.app/Contents/MacOS/ModAVD", "tools/modavd", "tools/modavd-manager"}
}

// Reject redirected workspace paths before taking the lock or writing state.
// Symlinks declared inside a signed payload are checked separately as a graph.
func (u *Updater) validateNativeWorkspace() error {
	paths := []string{u.Root.Dir, u.Root.AvdHome(), u.l.at("sdk"), u.l.dir(), u.l.download(), u.l.stage(), u.l.previous(), u.l.trash(),
		filepath.Join(u.l.stage(), "payload"), filepath.Join(u.l.stage(), "payload", "sdk"),
		filepath.Join(u.l.previous(), "native"), filepath.Join(u.l.previous(), "native", "sdk")}
	for _, p := range paths {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return packageError("更新目录不是独立的普通目录: %s", p)
		}
	}
	for _, p := range []string{u.l.state(), u.l.state() + ".tmp", u.l.lock()} {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !st.Mode().IsRegular() {
			return packageError("更新状态或锁文件不安全: %s", p)
		}
	}
	return nil
}

// Unit roots are rename boundaries. Internal payload links are permitted only
// within that unit, never in a parent used to reach the unit itself.
func (u *Updater) validateNativeUnitPaths(unit string, wantDirectory bool) error {
	if err := u.validateNativeWorkspace(); err != nil {
		return err
	}
	for _, p := range []string{u.l.at(unit), u.nativeBackup(unit), filepath.Join(u.nativePayload(), filepath.FromSlash(unit))} {
		st, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || st.Mode()&os.ModeSymlink != 0 || wantDirectory && !st.IsDir() || !wantDirectory && !st.Mode().IsRegular() {
			return packageError("更新单元不是预期类型: %s", p)
		}
	}
	return nil
}

func (u *Updater) nativeProcessesUsing(programs bool) ([]processUse, error) {
	procs, err := host.ListProcesses()
	if err != nil {
		return nil, err
	}
	key := func(p string) string { return strings.ToLower(realOrSame(p)) }
	var prefixes []string
	for _, p := range []string{u.l.emulator(), u.nativeBackup("sdk/emulator")} {
		prefixes = append(prefixes, key(p)+string(filepath.Separator))
	}
	if programs {
		for _, p := range []string{"ModAVD.app", "tools"} {
			prefixes = append(prefixes, key(u.l.at(p))+string(filepath.Separator))
		}
	}
	var uses []processUse
	for _, p := range procs {
		if p.PID == uint32(os.Getpid()) || p.PID == 0 {
			continue
		}
		image, err := p.ImagePath()
		if err != nil {
			name := strings.ToLower(p.Name)
			if p.Alive() && (strings.HasPrefix(name, "qemu-system") || strings.HasPrefix(name, "emulator") || strings.HasPrefix(name, "netsimd") || programs && strings.HasPrefix(name, "modavd")) {
				uses = append(uses, processUse{PID: p.PID, Name: p.Name, Unknown: true})
			}
			continue
		}
		image = key(image)
		for _, prefix := range prefixes {
			if strings.HasPrefix(image, prefix) && p.Alive() {
				uses = append(uses, processUse{PID: p.PID, Name: p.Name})
				break
			}
		}
	}
	return uses, nil
}

// Exclude managed starts through every rename and recovery. macOS allows an
// executing inode to be renamed; this is advisory exclusion plus a final full
// process check, not the mandatory Windows PE image-lock mechanism.
func (u *Updater) lockNativeSwap(verb string) (func(), error) {
	if err := u.ensureNotInUse(verb); err != nil {
		return nil, err
	}
	var locks []*host.FileLock
	release := func() {
		for _, lock := range locks {
			lock.Unlock()
		}
	}
	for i := 0; i <= root.MaxIndex; i++ {
		if !root.ValidIndex(i) {
			continue
		}
		path := filepath.Join(u.Root.AvdHome(), "."+root.AvdName(i)+".start.lock")
		if st, err := os.Lstat(path); err == nil && !st.Mode().IsRegular() {
			release()
			return nil, packageError("实例启动锁不安全: %s", path)
		}
		lock, free, err := host.TryLock(path)
		if err != nil || !free {
			release()
			if err != nil {
				return nil, fmt.Errorf("打开实例启动锁: %w", err)
			}
			return nil, errs.Newf(errs.Busy, "实例 %d 正在启动，暂不能%s", i, verb)
		}
		locks = append(locks, lock)
	}
	if err := u.ensureNotInUse(verb); err != nil {
		release()
		return nil, err
	}
	return release, nil
}
