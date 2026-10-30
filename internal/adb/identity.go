//go:build windows

package adb

// Which root a running qemu belongs to.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"modavd/internal/root"
	"modavd/internal/win"
)

// QemuBelongsTo reports whether qemu process p, already matched to instance i by its "-avd mas_<i>" and "-ports
// <console>,..." arguments, is instance i of root r. Every root names its instances mas_0, mas_1 ... and uses the same
// port plan, so those arguments also match the same-numbered instance of any other root on the machine (a test root
// next to the production D:\ModAVD). Taking that one for ours would shut it down, register it with our private
// server or run our after-boot setup on it.
//
//  1. The emulator's runtime file pid_<qemu pid>.ini in its discovery directory (EmulatorDiscoveryDir; qemu writes it
//     a few seconds after it starts and removes it when it exits normally) names the AVD directory it runs: its
//     avd.dir= has to be r's <root>\avd\mas_<i>.avd (compared case-insensitively, and again with links resolved,
//     since the root may be reached through a directory junction on another drive). A file naming any other
//     directory means another root, whatever else matches. A file older than the process is a leftover of an earlier
//     process with the same PID (a killed qemu never removes it) and does not count.
//  2. Without such a file (the first seconds of a boot: measured qemu created 16:15:42, pid_<pid>.ini written
//     16:15:46) both have to hold:
//     a. the qemu executable, argv[0], lies under r's <RuntimeSDK>\emulator\;
//     b. p holds the emulator's run lock of r's <root>\avd\mas_<i>.avd (runLockHeldBy).
//     a alone does not tell apart roots that share one SDK (sdkRoot): in that window stop would register another
//     root's emulator with our server and shut it down, and start would adopt it and time out. The lock lives in
//     the instance directory, so it does. When it cannot be confirmed (not taken yet, its PID not written yet, a
//     leftover, p's start time unknown, unreadable) the qemu is not ours: taking another root's emulator for ours
//     is the harm to avoid, and ours is found a moment later, once its lock (taken early in qemu's main) or its
//     runtime file is there.
func QemuBelongsTo(r *root.Root, p win.Process, i int) bool {
	if r == nil || p.PID == 0 {
		return false
	}
	return qemuBelongsTo(r, i, runningAvdDir(EmulatorDiscoveryDir(), p), func() string {
		cmdline, err := p.CommandLine()
		if err != nil || len(cmdline) == 0 {
			return ""
		}
		return cmdline[0]
	}, func(avdDir string) bool { return runLockHeldBy(avdDir, p) })
}

// qemuBelongsTo is the rule of QemuBelongsTo: avdDir is the avd.dir of the qemu's current runtime file ("" when it has
// none); argv0 reads its executable path and runLocked tells whether the qemu holds the run lock of an AVD directory.
// Both are only needed without that file.
func qemuBelongsTo(r *root.Root, i int, avdDir string, argv0 func() string, runLocked func(avdDir string) bool) bool {
	if avdDir != "" {
		return samePath(avdDir, instanceDir(r, i))
	}
	return pathUnder(argv0(), filepath.Join(r.RuntimeSDK(), "emulator")) && runLocked(instanceDir(r, i))
}

// instanceDir is avd.Dir(r, i), <root>\avd\mas_<i>.avd (spelled out here: adb does not depend on package avd).
func instanceDir(r *root.Root, i int) string {
	return filepath.Join(r.AvdHome(), root.AvdName(i)+".avd")
}

// The emulator's run lock of an AVD (AOSP android-qemu2-glue/main.cpp:1993: filelock_create_timeout(
// <avd>\hardware-qemu.ini, 2000), taken early in qemu's main, long before the runtime file, and released only at exit).
// On Windows (android/emu/files/src/android/utils/filelock.cpp filelock_lock) it is the directory
// hardware-qemu.ini.lock holding a file pid, which qemu opens with GENERIC_WRITE, FILE_SHARE_READ and CREATE_ALWAYS
// (a leftover is overwritten), writes its own PID into right away ("%lu", no line end) and keeps open until it exits;
// a normal exit closes and deletes both (filelock_release). A killed qemu leaves both behind, closed. Only -read-only
// and -check-snapshot-loadable skip this lock; neither modavd nor AUTO-MAS passes them.
const (
	runLockDir  = "hardware-qemu.ini.lock"
	runLockFile = "pid"
)

// runLockHeldBy reports whether qemu process p holds the emulator's run lock of AVD directory avdDir: the lock file
// names p's PID and was last written at or after p's start. qemu writes its PID right after taking the lock and keeps
// the lock until it exits, so such a file is p's own. A file an earlier process with the same PID left behind was
// written before that process exited, so before p was created (this also tells our qemu apart from a second one of
// the same root and index, whose PID differs). Without p's start time (Created 0) a leftover cannot be told from p's
// own file, and the lock does not count; neither does an unreadable file or one without a PID yet (between qemu's
// CreateFileW and its WriteFile). On NTFS both the overwrite and the write move LastWriteTime at once, and another
// handle sees the new time while qemu keeps its own open (measured).
//
// The probe never gets in the way of the emulator: it opens the file for reading only, allowing every sharing.
// qemu's lock handle (GENERIC_WRITE, FILE_SHARE_READ) admits such a reader, and the reader admits qemu's CreateFileW,
// its PID read and, at exit, its DeleteFileW, so qemu takes and drops its lock whether a probe has the file open at
// that moment or not. A probe that opens the file denying writers, to see whether somebody holds it for writing, is
// different: a qemu creating the lock while that handle is open gets ERROR_SHARING_VIOLATION (filelock.cpp:181-192).
// qemu then reads the PID in the file and calls waitForProcessExit(pid, 200) (filelock.cpp:193-221). When OpenProcess
// fails, that returns at once (Exited for a PID that is gone, which is what a killed qemu leaves behind; Error when
// access is denied: System.cpp:1109-1117), and filelock_lock counts it as its 200 ms sleep (slept=true), so the 10
// retries of the 2000 ms timeout run back to back within milliseconds, not every 200 ms over 2 s
// (filelock.cpp:229-243). A retry waits 200 ms only when the file holds no PID, or its PID now names a live process
// qemu can open (WaitForSingleObject times out). Such a probe was unlikely to make a boot fail, but not unable to: all
// retries fail if its handle stays open across them (the probing thread preempted between CreateFile and CloseHandle
// on a busy host is enough), and qemu then panics with "Running multiple emulators with the same AVD" (main.cpp).
// That is one reason the probe reads with every share mode now and never stands in the emulator's way.
func runLockHeldBy(avdDir string, p win.Process) bool {
	started := p.StartedAt()
	if p.PID == 0 || started.IsZero() {
		return false
	}
	holder, written, err := readRunLock(filepath.Join(avdDir, runLockDir, runLockFile))
	return err == nil && holder == p.PID && !written.Before(started)
}

// readRunLock reads the run lock file at path through one handle (openRunLock): its LastWriteTime, then the PID in it
// (0 when it does not hold a number). The time is read first: a time at or after p's start means p had taken the
// lock, and overwritten any leftover, before the PID was read.
func readRunLock(path string) (pid uint32, written time.Time, err error) {
	h, err := openRunLock(path)
	if err != nil {
		return 0, time.Time{}, err
	}
	defer syscall.CloseHandle(h)
	var fi syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &fi); err != nil {
		return 0, time.Time{}, err
	}
	written = time.Unix(0, fi.LastWriteTime.Nanoseconds())
	var buf [16]byte // a PID has at most 10 digits
	var n uint32
	if err := syscall.ReadFile(h, buf[:], &n, nil); err != nil {
		return 0, written, err
	}
	v, perr := strconv.ParseUint(strings.TrimSpace(string(buf[:n])), 10, 32)
	if perr != nil {
		return 0, written, nil
	}
	return uint32(v), written, nil
}

// openRunLock opens the existing run lock file at path the only way the probe ever does: GENERIC_READ with every
// sharing allowed, so it never keeps qemu from taking, reading or deleting it (see runLockHeldBy).
func openRunLock(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return syscall.InvalidHandle, err
	}
	return syscall.CreateFile(p, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL, 0)
}

// EmulatorDiscoveryDir is where the emulator writes its runtime files pid_<pid>.ini (AOSP
// android/emulation/ConfigDirs.cpp getDiscoveryDirectory on Windows): %LOCALAPPDATA%\Temp\avd\running, or, when
// LOCALAPPDATA is not set, <emulator user directory>\avd\running. TEMP / TMP never matter. qemu gets our environment
// when modavd starts it, and AUTO-MAS runs as the same user, so our LOCALAPPDATA is the emulator's.
func EmulatorDiscoveryDir() string {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "Temp", "avd", "running")
	}
	return filepath.Join(emulatorUserDir(), "avd", "running")
}

// emulatorUserDir is ConfigDirs::getUserDirectory on Windows: ANDROID_EMULATOR_HOME; else ANDROID_PREFS_ROOT, then the
// deprecated ANDROID_SDK_HOME, each with \.android appended when that directory exists; else <profile>\.android, the
// profile from SHGetFolderPath(CSIDL_PROFILE) (approximated here by the process token's profile directory, to stay
// off shell32) or HOMEDRIVE + HOMEPATH; without a profile, <GetTempPath>\AndroidEmulator\.android
// (System::getTempDir). Only used when LOCALAPPDATA is not set, which a normal Windows session never has.
func emulatorUserDir() string {
	if home := os.Getenv("ANDROID_EMULATOR_HOME"); home != "" {
		return home
	}
	for _, key := range []string{"ANDROID_PREFS_ROOT", "ANDROID_SDK_HOME"} {
		if home := os.Getenv(key); home != "" {
			if sub := filepath.Join(home, ".android"); isDir(sub) {
				return sub
			}
			return home
		}
	}
	home := profileDir()
	if home == "" {
		home = filepath.Join(os.TempDir(), "AndroidEmulator")
	}
	return filepath.Join(home, ".android")
}

// profileDir is the current user's profile directory, "" when it cannot be told.
func profileDir() string {
	if tok, err := syscall.OpenCurrentProcessToken(); err == nil {
		dir, err := tok.GetUserProfileDirectory()
		tok.Close()
		if err == nil && dir != "" {
			return dir
		}
	}
	if drive, path := os.Getenv("HOMEDRIVE"), os.Getenv("HOMEPATH"); drive != "" && path != "" {
		return drive + path
	}
	return ""
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

// runningAvdDir is the avd.dir= value of qemu p's runtime file pid_<pid>.ini in dir; "" when there is no current one
// (missing, unreadable, without that line, or written before p started).
func runningAvdDir(dir string, p win.Process) string {
	path := filepath.Join(dir, "pid_"+strconv.FormatUint(uint64(p.PID), 10)+".ini")
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return ""
	}
	if started := p.StartedAt(); !started.IsZero() && st.ModTime().Before(started) {
		return ""
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.EqualFold(strings.TrimSpace(key), "avd.dir") {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// samePath: a and b name the same file or directory. Compared case-insensitively (Windows), and when that fails once
// more with every link on both sides resolved.
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	return strings.EqualFold(resolvedPath(a), resolvedPath(b))
}

// pathUnder: path lies inside directory dir (at any depth), compared like samePath.
func pathUnder(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	if hasDirPrefix(filepath.Clean(path), filepath.Clean(dir)) {
		return true
	}
	return hasDirPrefix(resolvedPath(path), resolvedPath(dir))
}

// hasDirPrefix: cleaned path starts with cleaned directory dir followed by a separator, case-insensitively.
func hasDirPrefix(path, dir string) bool {
	dir = strings.TrimRight(dir, `\/`)
	if dir == "" || len(path) <= len(dir) || !strings.EqualFold(path[:len(dir)], dir) {
		return false
	}
	return os.IsPathSeparator(path[len(dir)])
}

// resolvedPath is path with its links resolved, cleaned; the cleaned path itself when it cannot be resolved (it does
// not exist, access denied). filepath.EvalSymlinks alone is not enough: since Go 1.23 it leaves mount points unresolved
// on Windows, and a directory junction (C:\emu-src) is one.
func resolvedPath(path string) string {
	if s, ok := finalPath(path); ok {
		return filepath.Clean(s)
	}
	if s, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Clean(s)
	}
	return filepath.Clean(path)
}

var procGetFinalPathNameByHandleW = kernel32.NewProc("GetFinalPathNameByHandleW") // kernel32: decode.go

const (
	fileReadAttributes      = 0x80
	fileFlagBackupSemantics = 0x02000000 // needed to open a directory
)

// finalPath is GetFinalPathNameByHandleW (normalized, DOS volume name) of an existing file or directory, without the
// \\?\ prefix it carries.
func finalPath(path string) (string, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	h, err := syscall.CreateFile(p, fileReadAttributes,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, fileFlagBackupSemantics, 0)
	if err != nil {
		return "", false
	}
	defer syscall.CloseHandle(h)
	if procGetFinalPathNameByHandleW.Find() != nil {
		return "", false
	}
	buf := make([]uint16, 512)
	for range 3 {
		n, _, _ := procGetFinalPathNameByHandleW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0)
		if n == 0 {
			return "", false
		}
		if int(n) >= len(buf) { // too small: n is the size needed, terminating NUL included
			buf = make([]uint16, n+1)
			continue
		}
		s := syscall.UTF16ToString(buf[:n])
		if rest, ok := strings.CutPrefix(s, `\\?\UNC\`); ok {
			return `\\` + rest, true
		}
		return strings.TrimPrefix(s, `\\?\`), true
	}
	return "", false
}
