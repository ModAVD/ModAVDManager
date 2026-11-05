//go:build darwin && cgo

package adb

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"modavd/internal/host"
	"modavd/internal/root"
)

const OwnershipSupported = true

// EmulatorDiscoveryDir follows the pinned core's ConfigDirs.cpp: HOME's cache
// directory, or the emulator user directory when HOME is empty. TMPDIR and
// ANDROID_USER_HOME do not select the discovery directory on Darwin.
func EmulatorDiscoveryDir() string { return emulatorDiscoveryDir(os.Getenv) }

func emulatorDiscoveryDir(getenv func(string) string) string {
	if home := getenv("HOME"); home != "" {
		return filepath.Join(home, "Library", "Caches", "TemporaryItems", "avd", "running")
	}
	return filepath.Join(emulatorUserDir(getenv), "avd", "running")
}

func emulatorUserDir(getenv func(string) string) string {
	if home := getenv("ANDROID_EMULATOR_HOME"); home != "" {
		return home
	}
	for _, key := range []string{"ANDROID_PREFS_ROOT", "ANDROID_SDK_HOME"} {
		if home := getenv(key); home != "" {
			if sub := filepath.Join(home, ".android"); isDir(sub) {
				return sub
			}
			return home
		}
	}
	home, _ := os.UserHomeDir()
	if home == "" {
		home = os.TempDir()
		if home == "" {
			home = "/tmp"
		}
	}
	return filepath.Join(home, ".android")
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// samePath uses file identity, honoring both symlinks and the volume's actual
// case rules. A missing or unreadable path cannot establish ownership.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	as, err := os.Stat(a)
	if err != nil {
		return false
	}
	bs, err := os.Stat(b)
	return err == nil && os.SameFile(as, bs)
}

func instanceDir(r *root.Root, i int) string {
	return filepath.Join(r.AvdHome(), root.AvdName(i)+".avd")
}

func processUnchanged(p host.Process) bool {
	if p.PID == 0 || p.Created == 0 {
		return false
	}
	current, ok := host.FindProcess(p.PID)
	return ok && current.Created == p.Created
}

// readIdentityFile never creates, locks or modifies the emulator's files. Read
// through one handle and reject symlinks, special files, replacement and writes
// during the read, rather than accepting mixed evidence from different files.
func readIdentityFile(path string, limit int64) ([]byte, os.FileInfo, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > limit {
		return nil, nil, errors.New("无效的模拟器归属文件")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if int64(len(b)) != before.Size() || !os.SameFile(before, after) || !os.SameFile(before, current) ||
		before.Size() != after.Size() || before.Size() != current.Size() ||
		!before.ModTime().Equal(after.ModTime()) || !before.ModTime().Equal(current.ModTime()) {
		return nil, nil, errors.New("模拟器归属文件在读取时变化")
	}
	return b, before, nil
}

// runningAvdDir permits the startup lock fallback only when the runtime file is
// absent or belongs to an older incarnation of this PID. Current but malformed,
// conflicting or unreadable metadata is never a reason to adopt the process.
func runningAvdDir(dir string, p host.Process) (avdDir string, fallback bool) {
	path := filepath.Join(dir, "pid_"+strconv.FormatUint(uint64(p.PID), 10)+".ini")
	b, st, err := readIdentityFile(path, 1<<20)
	if err != nil {
		return "", os.IsNotExist(err)
	}
	if st.ModTime().Before(p.StartedAt()) {
		return "", true
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "avd.dir" {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" || (avdDir != "" && !samePath(avdDir, value)) {
			return "", false
		}
		avdDir = value
	}
	return avdDir, false
}

// The pinned POSIX filelock.cpp writes PID + NUL to a mkstemp file, closes it,
// then link()s it to hardware-qemu.ini.lock and unlinks the temporary name. It
// retains no FD or flock. A fresh lock naming this live process identifies its
// instance even when two roots use the same SDK and instance/port arguments.
func runLockHeldBy(avdDir string, p host.Process) bool {
	b, st, err := readIdentityFile(filepath.Join(avdDir, "hardware-qemu.ini.lock"), 32)
	if err != nil || p.StartedAt().IsZero() || st.ModTime().Before(p.StartedAt()) {
		return false
	}
	pid, err := strconv.ParseUint(strings.TrimRight(string(b), "\x00\r\n \t"), 10, 32)
	return err == nil && uint32(pid) == p.PID
}

// QemuBelongsTo verifies both the native executable and root-specific AVD
// evidence, then checks the process creation identity again. A current runtime
// record naming another root overrides any startup lock or argument match.
func QemuBelongsTo(r *root.Root, p host.Process, i int) bool {
	if r == nil || !root.ValidIndex(i) || !processUnchanged(p) {
		return false
	}
	exe, err := p.ImagePath()
	if err != nil {
		return false
	}
	platform := root.CurrentPlatform()
	qemuDir := filepath.Join(r.RuntimeSDK(), "emulator", "qemu", platform.QemuDir)
	if !platform.Supported() || (!samePath(exe, filepath.Join(qemuDir, platform.QEMU)) &&
		!samePath(exe, filepath.Join(qemuDir, platform.QEMUHeadless))) {
		return false
	}
	env, ok := processEnvironment(p)
	if !ok {
		return false
	}
	if home := env["ANDROID_AVD_HOME"]; home != "" && !samePath(home, r.AvdHome()) {
		return false
	}
	dir := emulatorDiscoveryDir(func(key string) string { return env[key] })
	avdDir, fallback := runningAvdDir(dir, p)
	target := instanceDir(r, i)
	owned := avdDir != "" && samePath(avdDir, target)
	if avdDir == "" && fallback {
		owned = runLockHeldBy(target, p)
	}
	return owned && processUnchanged(p)
}

// processEnvironment uses the host's checked native query. An unreadable or
// changing process cannot establish root ownership.
func processEnvironment(p host.Process) (map[string]string, bool) {
	env, err := p.Environment()
	return env, err == nil && processUnchanged(p)
}
