//go:build windows

package adb

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modavd/internal/root"
	"modavd/internal/win"
)

func TestHasDirPrefix(t *testing.T) {
	cases := []struct {
		path, dir string
		want      bool
	}{
		{`D:\ModAVD\sdk\emulator\qemu\windows-x86_64\qemu-system-x86_64.exe`, `D:\ModAVD\sdk\emulator`, true},
		{`d:\modavd\SDK\Emulator\qemu.exe`, `D:\ModAVD\sdk\emulator`, true},
		{`D:\ModAVD\sdk\emulator\qemu.exe`, `D:\ModAVD\sdk\emulator\`, true},
		{`D:\ModAVD\sdk\emulator2\qemu.exe`, `D:\ModAVD\sdk\emulator`, false}, // a sibling with the same prefix
		{`D:\ModAVD\sdk\emulator`, `D:\ModAVD\sdk\emulator`, false},           // the directory itself
		{`D:\other\sdk\emulator\qemu.exe`, `D:\ModAVD\sdk\emulator`, false},
		{`D:\x.exe`, `D:\`, true},
		{`qemu.exe`, `D:\ModAVD\sdk\emulator`, false},
		{`D:\x.exe`, ``, false},
	}
	for _, tc := range cases {
		if got := hasDirPrefix(filepath.Clean(tc.path), filepath.Clean(tc.dir)); got != tc.want {
			t.Errorf("hasDirPrefix(%q, %q) = %v, want %v", tc.path, tc.dir, got, tc.want)
		}
	}
}

func TestSamePathAndPathUnder(t *testing.T) {
	dir := tempDir(t)
	target := filepath.Join(dir, "target")
	avdDir := filepath.Join(target, "avd", "mas_3.avd")
	exe := filepath.Join(target, "sdk", "emulator", "qemu", "qemu-system-x86_64.exe")
	for _, d := range []string{avdDir, filepath.Dir(exe)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(exe, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !samePath(avdDir, strings.ToUpper(avdDir)+`\`) || !samePath(avdDir, filepath.Join(target, "avd", ".", "mas_3.avd")) {
		t.Errorf("samePath: case and spelling must not matter")
	}
	if samePath(avdDir, filepath.Join(target, "avd", "mas_4.avd")) || samePath("", "") || samePath(avdDir, "") {
		t.Errorf("samePath: different paths are not the same")
	}
	if !pathUnder(exe, filepath.Join(target, "sdk", "emulator")) || pathUnder(exe, filepath.Join(target, "sdk", "emu")) {
		t.Errorf("pathUnder")
	}

	// Through a directory junction (say a source directory on C: that points to one on another drive):
	// filepath.EvalSymlinks leaves those alone since Go 1.23, the comparison has to resolve them anyway.
	link := filepath.Join(dir, "link")
	cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", link, target)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v %s", err, out)
	}
	t.Cleanup(func() { os.Remove(link) }) // the junction only, before the temp dir goes
	if !samePath(filepath.Join(link, "avd", "mas_3.avd"), avdDir) {
		t.Errorf("samePath through a junction = false")
	}
	if !pathUnder(exe, filepath.Join(link, "sdk", "emulator")) {
		t.Errorf("pathUnder with the directory reached through a junction = false")
	}
	if !pathUnder(filepath.Join(link, "sdk", "emulator", "qemu", "qemu-system-x86_64.exe"), filepath.Join(target, "sdk", "emulator")) {
		t.Errorf("pathUnder with the path reached through a junction = false")
	}
	if samePath(filepath.Join(link, "avd", "mas_4.avd"), avdDir) {
		t.Errorf("samePath through a junction matched another directory")
	}
}

func filetimeOf(t time.Time) uint64 {
	ft := syscall.NsecToFiletime(t.UnixNano())
	return uint64(ft.HighDateTime)<<32 | uint64(ft.LowDateTime)
}

func TestRunningAvdDir(t *testing.T) {
	dir := tempDir(t)
	write := func(pid uint32, text string) string {
		path := filepath.Join(dir, "pid_"+strconv.FormatUint(uint64(pid), 10)+".ini")
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	// the layout the emulator writes (CRLF, other keys around it)
	write(4242, "launcher.dir=D:\\ModAVD\\sdk\\emulator\r\navd.name=mas_0\r\navd.dir=D:\\ModAVD\\avd\\mas_0.avd\r\nport.serial=20000\r\n")
	if got := runningAvdDir(dir, win.Process{PID: 4242}); got != `D:\ModAVD\avd\mas_0.avd` {
		t.Errorf("runningAvdDir = %q", got)
	}
	// the process started before the file was written: the file is its own
	if got := runningAvdDir(dir, win.Process{PID: 4242, Created: filetimeOf(time.Now().Add(-time.Hour))}); got == "" {
		t.Errorf("a file written after the process started was ignored")
	}
	// the process started after the file was written: a leftover of an earlier process with the same PID
	if got := runningAvdDir(dir, win.Process{PID: 4242, Created: filetimeOf(time.Now().Add(time.Hour))}); got != "" {
		t.Errorf("a stale file counted: %q", got)
	}
	write(4243, "avd.name=mas_0\nport.serial=20000\n")
	if got := runningAvdDir(dir, win.Process{PID: 4243}); got != "" {
		t.Errorf("no avd.dir line = %q", got)
	}
	if got := runningAvdDir(dir, win.Process{PID: 4244}); got != "" {
		t.Errorf("no file = %q", got)
	}
	if got := runningAvdDir(filepath.Join(dir, "missing"), win.Process{PID: 4242}); got != "" {
		t.Errorf("no directory = %q", got)
	}
}

func TestQemuBelongsToRule(t *testing.T) {
	dir := tempDir(t)
	r := &root.Root{Dir: filepath.Join(dir, "root")}
	ours := filepath.Join(r.Dir, "avd", "mas_3.avd")
	ourQemu := filepath.Join(r.RuntimeSDK(), "emulator", "qemu", "windows-x86_64", "qemu-system-x86_64-headless.exe")
	other := filepath.Join(dir, "other") // another root (nothing there; the paths are only compared)
	otherQemu := filepath.Join(other, "sdk", "emulator", "qemu", "windows-x86_64", "qemu-system-x86_64-headless.exe")
	argv0 := func(s string) func() string { return func() string { return s } }
	mustNotRead := func() string { t.Errorf("argv[0] read although the runtime file decides"); return ourQemu }
	mustNotProbe := func(string) bool { t.Errorf("run lock probed although the runtime file decides"); return true }
	var probed []string
	lockedIn := func(dirs ...string) func(string) bool {
		return func(d string) bool {
			probed = append(probed, d)
			for _, want := range dirs {
				if samePath(d, want) {
					return true
				}
			}
			return false
		}
	}

	if want := ours; instanceDir(r, 3) != want {
		t.Fatalf("instanceDir = %q, want %q", instanceDir(r, 3), want)
	}
	// 1. the runtime file decides, whatever argv[0] and the lock say
	if !qemuBelongsTo(r, 3, strings.ToUpper(ours), mustNotRead, mustNotProbe) {
		t.Errorf("our avd.dir (other case) = not ours")
	}
	if qemuBelongsTo(r, 3, filepath.Join(other, "avd", "mas_3.avd"), mustNotRead, mustNotProbe) {
		t.Errorf("another root's avd.dir = ours")
	}
	if qemuBelongsTo(r, 4, ours, mustNotRead, mustNotProbe) {
		t.Errorf("instance 3's avd.dir counted for instance 4")
	}
	// 2. no runtime file: the qemu executable under our SDK's emulator directory and our instance's run lock held
	if !qemuBelongsTo(r, 3, "", argv0(ourQemu), lockedIn(ours)) {
		t.Errorf("our SDK's qemu holding our lock = not ours")
	}
	if len(probed) != 1 || probed[0] != ours {
		t.Errorf("probed %q, want our instance directory %q", probed, ours)
	}
	if qemuBelongsTo(r, 3, "", argv0(ourQemu), lockedIn()) {
		t.Errorf("our SDK's qemu without the lock held = ours")
	}
	if qemuBelongsTo(r, 4, "", argv0(ourQemu), lockedIn(ours)) {
		t.Errorf("instance 3's lock counted for instance 4")
	}
	if qemuBelongsTo(r, 3, "", argv0(otherQemu), lockedIn(ours)) {
		t.Errorf("another SDK's qemu = ours")
	}
	if qemuBelongsTo(r, 3, "", argv0(filepath.Join(r.RuntimeSDK(), "qemu-system-x86_64.exe")), lockedIn(ours)) {
		t.Errorf("a qemu outside <sdk>\\emulator = ours")
	}
	if qemuBelongsTo(r, 3, "", argv0(""), lockedIn(ours)) {
		t.Errorf("unreadable command line = ours")
	}

	// Two roots sharing one SDK (sdkRoot): argv[0] matches both, the lock tells them apart.
	shared := filepath.Join(dir, "sdk-shared")
	a, b := sharedSDKRoot(t, filepath.Join(dir, "a"), shared), sharedSDKRoot(t, filepath.Join(dir, "b"), shared)
	sharedQemu := filepath.Join(shared, "emulator", "qemu", "windows-x86_64", "qemu-system-x86_64-headless.exe")
	if !qemuBelongsTo(a, 3, "", argv0(sharedQemu), lockedIn(instanceDir(a, 3))) {
		t.Errorf("shared SDK: the root holding the lock = not ours")
	}
	if qemuBelongsTo(b, 3, "", argv0(sharedQemu), lockedIn(instanceDir(a, 3))) {
		t.Errorf("shared SDK: the root without the lock = ours")
	}
}

// sharedSDKRoot is a root in dir whose mas-avd.json points sdkRoot at sdk.
func sharedSDKRoot(t *testing.T, dir, sdk string) *root.Root {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta, err := json.Marshal(map[string]string{"sdkRoot": sdk})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, root.MetadataFile), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	r := &root.Root{Dir: dir}
	if !samePath(r.RuntimeSDK(), sdk) {
		t.Fatalf("RuntimeSDK = %q, want %q", r.RuntimeSDK(), sdk)
	}
	return r
}

// holdFile creates (or truncates) the file at path, and its directory, with the given access (write access included)
// and sharing, writes content and keeps it open until the returned release (also run at cleanup). GENERIC_WRITE with
// FILE_SHARE_READ is how qemu holds its run lock; sharing 0 holds it exclusively.
func holdFile(t *testing.T, path string, access, share uint32, content string) (release func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(p, access, share, nil, syscall.CREATE_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("CreateFile %s: %v", path, err)
	}
	if content != "" {
		var n uint32
		if err := syscall.WriteFile(h, []byte(content), &n, nil); err != nil || int(n) != len(content) {
			syscall.CloseHandle(h)
			t.Fatalf("WriteFile %s: %v (%d bytes)", path, err, n)
		}
	}
	var once sync.Once
	release = func() { once.Do(func() { syscall.CloseHandle(h) }) }
	t.Cleanup(release)
	return release
}

const (
	qemuLockAccess = syscall.GENERIC_WRITE
	qemuLockShare  = syscall.FILE_SHARE_READ
	// errSharingViolation is ERROR_SHARING_VIOLATION (the syscall package does not name it).
	errSharingViolation = syscall.Errno(32)
)

// qemuOpenFile is CreateFileW of the run lock file as filelock_lock issues it: for taking the lock (qemuLockAccess,
// qemuLockShare, CREATE_ALWAYS: filelock.cpp:181-188) or for reading the holder's PID after that failed (GENERIC_READ,
// FILE_SHARE_READ|FILE_SHARE_WRITE, OPEN_EXISTING: filelock.cpp:193-205).
func qemuOpenFile(t *testing.T, path string, access, share, disposition uint32) (syscall.Handle, error) {
	t.Helper()
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	return syscall.CreateFile(p, access, share, nil, disposition, syscall.FILE_ATTRIBUTE_NORMAL, 0)
}

func TestRunLockHeldBy(t *testing.T) {
	avdDir := filepath.Join(tempDir(t), "mas_3.avd")
	lock := filepath.Join(avdDir, runLockDir, runLockFile)
	pid := uint32(os.Getpid())
	pidText := strconv.FormatUint(uint64(pid), 10)
	started := time.Now().Add(-time.Hour)
	p := win.Process{PID: pid, Created: filetimeOf(started)} // started before anything below is written

	if runLockHeldBy(avdDir, p) {
		t.Errorf("no lock directory = held")
	}
	// held the way qemu holds it: write access, readers allowed, its PID inside
	release := holdFile(t, lock, qemuLockAccess, qemuLockShare, pidText)
	if got, written, err := readRunLock(lock); err != nil || got != pid || written.Before(started) {
		t.Errorf("readRunLock = %d, %v, %v; want %d written after %v", got, written, err, pid, started)
	}
	if !runLockHeldBy(avdDir, p) {
		t.Errorf("held like qemu with our PID = not held")
	}
	if runLockHeldBy(avdDir, win.Process{PID: pid + 1, Created: p.Created}) {
		t.Errorf("held with another PID counted for pid %d", pid+1)
	}
	// the same PID, but the process started after the file was written: not its lock
	if runLockHeldBy(avdDir, win.Process{PID: pid, Created: filetimeOf(time.Now().Add(time.Hour))}) {
		t.Errorf("a lock written before the process started = held by it")
	}
	if runLockHeldBy(avdDir, win.Process{PID: pid}) {
		t.Errorf("held although the start time is unknown")
	}
	if runLockHeldBy(avdDir, win.Process{Created: p.Created}) {
		t.Errorf("held by PID 0")
	}
	release()

	// what a killed qemu leaves behind (directory and file with its PID, nobody holding them), from before p started:
	// p having been given that PID again does not make the lock p's
	leftover := started.Add(-time.Hour)
	if err := os.Chtimes(lock, leftover, leftover); err != nil {
		t.Fatal(err)
	}
	if runLockHeldBy(avdDir, p) {
		t.Errorf("a leftover written before the process started = held")
	}
	// p taking that leftover over (CREATE_ALWAYS overwrites it, then the PID is written): written after p's start,
	// readable while p keeps it open
	release = holdFile(t, lock, qemuLockAccess, qemuLockShare, pidText)
	if !runLockHeldBy(avdDir, p) {
		_, written, err := readRunLock(lock)
		t.Errorf("an overwritten leftover = not held (LastWriteTime %v, %v; started %v)", written, err, started)
	}
	release()

	// taken, but the PID not written yet
	release = holdFile(t, lock, qemuLockAccess, qemuLockShare, "")
	if got, _, err := readRunLock(lock); err != nil || got != 0 {
		t.Errorf("readRunLock without a PID = %d, %v", got, err)
	}
	if runLockHeldBy(avdDir, p) {
		t.Errorf("held without a PID in it = held by us")
	}
	release()
	// not a number
	release = holdFile(t, lock, qemuLockAccess, qemuLockShare, "pid "+pidText)
	if runLockHeldBy(avdDir, p) {
		t.Errorf("held with garbage in it = held by us")
	}
	release()
	// held denying readers: qemu never does that, and while such a handle is open qemu cannot take the lock either
	release = holdFile(t, lock, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, pidText)
	if _, _, err := readRunLock(lock); !errors.Is(err, errSharingViolation) {
		t.Errorf("readRunLock of an exclusively held file: %v, want ERROR_SHARING_VIOLATION", err)
	}
	if runLockHeldBy(avdDir, p) {
		t.Errorf("held exclusively by somebody = held by us")
	}
	release()
}

// The probe's handle never keeps qemu from taking, reading or deleting its lock file, whether the file is a leftover
// or held by qemu. A probe denying writers does keep qemu from taking it (why runLockHeldBy does not use one).
func TestRunLockProbeNeverBlocksQemu(t *testing.T) {
	lock := filepath.Join(tempDir(t), "mas_3.avd", runLockDir, runLockFile)
	holdFile(t, lock, qemuLockAccess, qemuLockShare, "4242")() // a leftover of a killed qemu with PID 4242

	probe, err := openRunLock(lock)
	if err != nil {
		t.Fatal(err)
	}
	var closeProbe sync.Once
	t.Cleanup(func() { closeProbe.Do(func() { syscall.CloseHandle(probe) }) })
	// qemu takes the lock while the probe has the file open
	h, err := qemuOpenFile(t, lock, qemuLockAccess, qemuLockShare, syscall.CREATE_ALWAYS)
	if err != nil {
		t.Fatalf("qemu's CreateFileW while the probe is open: %v", err)
	}
	var closeQemu sync.Once
	t.Cleanup(func() { closeQemu.Do(func() { syscall.CloseHandle(h) }) })
	var n uint32
	if err := syscall.WriteFile(h, []byte("4343"), &n, nil); err != nil {
		t.Fatal(err)
	}
	// qemu's PID read (another emulator of the same AVD) gets in next to both handles
	g, err := qemuOpenFile(t, lock, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
		syscall.OPEN_EXISTING)
	if err != nil {
		t.Errorf("qemu's PID read while held and probed: %v", err)
	} else {
		syscall.CloseHandle(g)
	}
	// a second probe while qemu holds it
	if got, _, err := readRunLock(lock); err != nil || got != 4343 {
		t.Errorf("readRunLock while held and probed = %d, %v; want 4343", got, err)
	}
	// qemu's exit (filelock_release: CloseHandle, then DeleteFileW) while the probe is still open
	closeQemu.Do(func() { syscall.CloseHandle(h) })
	if err := os.Remove(lock); err != nil {
		t.Errorf("qemu's DeleteFileW while the probe is open: %v", err)
	}
	closeProbe.Do(func() { syscall.CloseHandle(probe) })

	// contrast: a probe denying writers makes qemu's CreateFileW fail
	holdFile(t, lock, qemuLockAccess, qemuLockShare, "4242")()
	denying, err := qemuOpenFile(t, lock, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, syscall.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(denying)
	if h, err := qemuOpenFile(t, lock, qemuLockAccess, qemuLockShare, syscall.CREATE_ALWAYS); err == nil {
		syscall.CloseHandle(h)
		t.Errorf("qemu's CreateFileW succeeded next to a handle denying writers")
	} else if !errors.Is(err, errSharingViolation) {
		t.Errorf("qemu's CreateFileW next to a handle denying writers: %v, want ERROR_SHARING_VIOLATION", err)
	}
}

// Every mas_<i> qemu running on this machine (its runtime file names its instance directory) holds that directory's
// run lock under its own PID, written after it started, as runLockHeldBy expects. Skipped when none runs. Read-only:
// the lock file is only opened for reading with every sharing allowed (see runLockHeldBy for why that cannot disturb
// the emulator).
func TestRunLockOfRealInstances(t *testing.T) {
	// Looks at the emulators running on this machine (their lock / runtime files, a real adb server): opt in.
	if os.Getenv("MODAVD_TEST_LIVE") == "" {
		t.Skip("set MODAVD_TEST_LIVE=1 to run tests that look at live emulators or start a real adb server")
	}
	procs, err := win.ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	discovery := EmulatorDiscoveryDir()
	checked := 0
	for _, p := range procs {
		if !strings.HasPrefix(strings.ToLower(p.Name), "qemu-system") {
			continue
		}
		avdDir := runningAvdDir(discovery, p)
		if base := strings.ToLower(filepath.Base(avdDir)); !strings.HasPrefix(base, root.AvdNamePrefix) || !strings.HasSuffix(base, ".avd") {
			continue // no runtime file, or not a ModAVD instance
		}
		if !runLockHeldBy(avdDir, p) {
			holder, written, err := readRunLock(filepath.Join(avdDir, runLockDir, runLockFile))
			t.Errorf("qemu %d (started %v) does not hold the run lock of %s (holder %d, written %v, %v)", p.PID,
				p.StartedAt(), avdDir, holder, written, err)
		}
		checked++
	}
	if checked == 0 {
		t.Skip("no mas_<i> qemu with a runtime file runs on this machine")
	}
	t.Logf("checked %d running instance(s)", checked)
}

func TestEmulatorDiscoveryDir(t *testing.T) {
	dir := tempDir(t)
	tmp := filepath.Join(dir, "tmp")
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "local"))
	emuHome, prefs, sdkHome := filepath.Join(dir, "emu-home"), filepath.Join(dir, "prefs"), filepath.Join(dir, "sdk-home")
	t.Setenv("ANDROID_EMULATOR_HOME", emuHome)
	t.Setenv("ANDROID_PREFS_ROOT", prefs)
	t.Setenv("ANDROID_SDK_HOME", sdkHome)
	check := func(what, want string) {
		t.Helper()
		if got := EmulatorDiscoveryDir(); got != want {
			t.Errorf("%s: EmulatorDiscoveryDir = %q, want %q", what, got, want)
		}
	}
	// TEMP / TMP and the user directory variables never matter while LOCALAPPDATA is set
	check("LOCALAPPDATA set", filepath.Join(dir, "local", "Temp", "avd", "running"))

	// without LOCALAPPDATA: ConfigDirs::getUserDirectory()\avd\running
	t.Setenv("LOCALAPPDATA", "")
	check("ANDROID_EMULATOR_HOME", filepath.Join(emuHome, "avd", "running"))
	t.Setenv("ANDROID_EMULATOR_HOME", "")
	check("ANDROID_PREFS_ROOT without .android", filepath.Join(prefs, "avd", "running"))
	if err := os.MkdirAll(filepath.Join(prefs, ".android"), 0o755); err != nil {
		t.Fatal(err)
	}
	check("ANDROID_PREFS_ROOT with .android", filepath.Join(prefs, ".android", "avd", "running"))
	t.Setenv("ANDROID_PREFS_ROOT", "")
	check("ANDROID_SDK_HOME without .android", filepath.Join(sdkHome, "avd", "running"))
	if err := os.MkdirAll(filepath.Join(sdkHome, ".android"), 0o755); err != nil {
		t.Fatal(err)
	}
	check("ANDROID_SDK_HOME with .android", filepath.Join(sdkHome, ".android", "avd", "running"))
	t.Setenv("ANDROID_SDK_HOME", "")
	profile := profileDir()
	if profile == "" {
		t.Fatal("profileDir is empty")
	}
	check("profile", filepath.Join(profile, ".android", "avd", "running"))
}

// QemuBelongsTo on a real process (this test binary standing in for qemu), with runtime files in a LOCALAPPDATA of
// our own.
func TestQemuBelongsToProcess(t *testing.T) {
	dir := tempDir(t)
	tmp := filepath.Join(dir, "tmp")
	local := filepath.Join(dir, "local")
	for _, d := range []string{tmp, local} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	t.Setenv("LOCALAPPDATA", local)
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok || self.Created == 0 {
		t.Fatal("cannot read this process")
	}
	r := &root.Root{Dir: filepath.Join(dir, "root")}
	if QemuBelongsTo(nil, self, 3) || QemuBelongsTo(r, win.Process{}, 3) {
		t.Errorf("nil root / no process = ours")
	}
	// no runtime file: argv[0] (this test binary) is not under the root's SDK, holding the root's run lock or not
	if QemuBelongsTo(r, self, 3) {
		t.Errorf("a process outside the root's SDK without runtime file = ours")
	}
	release := holdFile(t, filepath.Join(instanceDir(r, 3), runLockDir, runLockFile), qemuLockAccess, qemuLockShare,
		strconv.Itoa(os.Getpid()))
	if QemuBelongsTo(r, self, 3) {
		t.Errorf("a process outside the root's SDK holding the run lock = ours")
	}
	release()
	writeRuntime := func(base, avdDir string) string {
		t.Helper()
		running := filepath.Join(base, "avd", "running")
		if err := os.MkdirAll(running, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(running, "pid_"+strconv.Itoa(os.Getpid())+".ini")
		if err := os.WriteFile(file, []byte("avd.name=mas_3\r\navd.dir="+avdDir+"\r\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return file
	}
	other := &root.Root{Dir: filepath.Join(dir, "other")}

	// in TEMP: the emulator never writes there (its directory comes from LOCALAPPDATA), so it does not count
	inTemp := writeRuntime(tmp, instanceDir(r, 3))
	if QemuBelongsTo(r, self, 3) {
		t.Errorf("a runtime file in TEMP counted")
	}
	if err := os.Remove(inTemp); err != nil {
		t.Fatal(err)
	}

	file := writeRuntime(filepath.Join(local, "Temp"), instanceDir(r, 3))
	if !QemuBelongsTo(r, self, 3) {
		t.Errorf("runtime file naming our instance dir = not ours")
	}
	if QemuBelongsTo(r, self, 4) {
		t.Errorf("runtime file of instance 3 counted for 4")
	}
	if QemuBelongsTo(other, self, 3) {
		t.Errorf("runtime file of root %s counted for root %s", r.Dir, other.Dir)
	}
	// older than the process: a leftover of a previous process with this PID, so argv[0] decides (not ours)
	old := self.StartedAt().Add(-time.Hour)
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	if QemuBelongsTo(r, self, 3) {
		t.Errorf("a stale runtime file counted")
	}
}

// Two roots sharing one SDK, before the runtime file exists: QemuBelongsTo on a real process (this test binary) whose
// argv[0] lies under the shared <sdk>\emulator (a junction to the test binary's directory), with the run lock held
// only in one root's instance directory.
func TestQemuBelongsToSharedSDK(t *testing.T) {
	dir := tempDir(t)
	t.Setenv("LOCALAPPDATA", filepath.Join(dir, "local")) // no runtime file there
	self, ok := win.FindProcess(uint32(os.Getpid()))
	if !ok || self.Created == 0 {
		t.Fatal("cannot read this process")
	}
	cmdline, err := self.CommandLine()
	if err != nil || len(cmdline) == 0 || !filepath.IsAbs(cmdline[0]) {
		t.Skipf("argv[0] of this process is not an absolute path: %q %v", cmdline, err)
	}
	shared := filepath.Join(dir, "sdk-shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	emulatorDir := filepath.Join(shared, "emulator")
	cmd := exec.Command("cmd.exe", "/c", "mklink", "/J", emulatorDir, filepath.Dir(cmdline[0]))
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: win.HideConsole}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("mklink /J failed: %v %s", err, out)
	}
	t.Cleanup(func() { os.Remove(emulatorDir) }) // the junction only, before the temp dir goes
	a, b := sharedSDKRoot(t, filepath.Join(dir, "a"), shared), sharedSDKRoot(t, filepath.Join(dir, "b"), shared)
	if !pathUnder(cmdline[0], filepath.Join(a.RuntimeSDK(), "emulator")) {
		t.Fatalf("argv[0] %s is not under %s", cmdline[0], emulatorDir)
	}
	for _, r := range []*root.Root{a, b} {
		if err := os.MkdirAll(instanceDir(r, 3), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	check := func(what string, wantA bool) {
		t.Helper()
		if got := QemuBelongsTo(a, self, 3); got != wantA {
			t.Errorf("%s: root a = %v, want %v", what, got, wantA)
		}
		if QemuBelongsTo(b, self, 3) {
			t.Errorf("%s: root b (no lock) = ours", what)
		}
		if QemuBelongsTo(a, self, 4) {
			t.Errorf("%s: instance 4 of root a (no lock) = ours", what)
		}
	}
	check("no lock anywhere", false)

	lock := filepath.Join(instanceDir(a, 3), runLockDir, runLockFile)
	pidText := strconv.Itoa(os.Getpid())
	// held exclusively: the PID cannot be read, and qemu (which never holds it so) could not have taken it either
	release := holdFile(t, lock, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, pidText)
	check("lock held exclusively in root a", false)
	release()
	// left behind with this PID by an earlier process (written before this one started)
	old := self.StartedAt().Add(-time.Hour)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	check("lock left behind before this process started", false)

	release = holdFile(t, lock, qemuLockAccess, qemuLockShare, pidText)
	check("lock held like qemu with this PID in root a", true)
	release()

	// another qemu of root a's instance 3 holds it: this process is not that one
	release = holdFile(t, lock, qemuLockAccess, qemuLockShare, strconv.Itoa(os.Getpid()+1))
	check("lock of another PID in root a", false)
	release()
}
