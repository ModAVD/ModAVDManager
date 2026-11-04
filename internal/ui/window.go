//go:build windows

package ui

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"modavd/internal/logx"
)

// Window is the manager window that LaunchWindow opened.
type Window struct {
	// Edge: an Edge app window; false = the default browser (nothing to wait for).
	Edge    bool
	Started time.Time
	// Done is closed when the Edge process exits; nil for the default browser. An Edge that finds one already
	// running on the same profile hands the window to it and exits at once.
	Done <-chan struct{}
}

// LaunchWindow opens url (the login address "http://127.0.0.1:<port>/?t=<token>") in an Edge app window on the
// manager's own profile (%LOCALAPPDATA%\ModAVD\ui-edge2), or in the default browser when Edge is missing or does not
// start.
func LaunchWindow(url string) (*Window, error) {
	if exe := FindEdge(); exe != "" {
		cmd := exec.Command(exe, edgeArgs(url, EdgeProfileDir())...)
		if err := cmd.Start(); err == nil {
			done := make(chan struct{})
			go func() {
				cmd.Wait()
				close(done)
			}()
			return &Window{Edge: true, Started: time.Now(), Done: done}, nil
		}
	}
	if err := shellOpen(url); err != nil {
		return nil, fmt.Errorf("Edge 和默认浏览器都打不开: %w", err)
	}
	return &Window{Started: time.Now()}, nil
}

// AppDataDir is %LOCALAPPDATA%\ModAVD (the manager's state files and Edge profile).
func AppDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base, _ = os.UserCacheDir()
	}
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "ModAVD")
}

// edgeArgs is the msedge.exe command line of the manager window on profile. A profile of its own is not enough: on a
// new profile Edge signs in with the Windows account by itself (the msImplicitSignin feature), sync then installs the
// user's extensions there, and their welcome pages open in a full browser window next to the manager. So the window
// also runs without sign-in, sync and extensions; the last two hold even if a later Edge renames the feature.
//
// And when the window closes, startup boost (msEdgeStartupBoost) starts "msedge --no-startup-window", which knows
// nothing of --user-data-dir: an Edge on the user's own profile, with their extensions, even if they had not opened
// Edge. Edge reads only the last --disable-features, so both features go in one.
func edgeArgs(url, profile string) []string {
	return []string{"--app=" + url, "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check",
		"--disable-features=" + edgeDisabledFeatures, "--disable-sync", "--disable-extensions", "--window-size=1180,760"}
}

const edgeDisabledFeatures = "msImplicitSignin,msEdgeStartupBoost"

// EdgeProfileDir is the manager's own Edge profile: the window does not mix with the user's browsing, and the
// cookie lives there only. Not ui-edge: managers up to 0.3.0 used that one without the switches of edgeArgs, so it
// may already be signed in with the user's account (switches do not sign a profile out); removeOldEdgeProfile
// deletes it.
func EdgeProfileDir() string { return filepath.Join(AppDataDir(), "ui-edge2") }

// oldEdgeProfile is the Edge profile of managers up to 0.3.0 under AppDataDir, and oldEdgeProfile+".deleting" the
// same folder while it is being deleted.
const oldEdgeProfile = "ui-edge"

// removeOldEdgeProfile deletes appData\ui-edge, which may hold copies of the user's favourites, history and passwords
// that sync brought in, and nothing else. It leaves the folder alone while an Edge runs on it (the window of an older
// manager still open): Edge then holds the profile's lockfile, and Windows does not rename a folder with a file open
// in it. The folder is renamed to ui-edge.deleting first, so an Edge that an older manager starts meanwhile gets a
// new profile rather than a half-deleted one, and a deletion cut short is finished on the next start. Failures are
// only logged; the next start tries again.
func removeOldEdgeProfile(appData string, log *logx.Logger) {
	dir := filepath.Join(appData, oldEdgeProfile)
	trash := dir + ".deleting"
	if !removePlainDir(trash, log) {
		return
	}
	if _, err := os.Lstat(filepath.Join(dir, "lockfile")); err == nil {
		log.Infof("旧版管理器的 Edge 配置 %s 还在用（还开着旧版管理器？），下次打开时再删", dir)
		return
	}
	if st, err := os.Lstat(dir); err != nil || !plainDir(st) {
		if err == nil {
			log.Warnf("%s 不是普通文件夹，不删", dir)
		} else if !errors.Is(err, fs.ErrNotExist) {
			log.Warnf("看不了旧版管理器的 Edge 配置 %s: %v", dir, err)
		}
		return
	}
	if err := os.Rename(dir, trash); err != nil {
		log.Infof("旧版管理器的 Edge 配置 %s 还在用，下次打开时再删: %v", dir, err)
		return
	}
	if removePlainDir(trash, log) {
		log.Infof("删掉了旧版管理器的 Edge 配置 %s（里面是以前同步进去的收藏夹、历史、密码的副本）", dir)
	}
}

// removePlainDir removes dir if it is a plain folder (not a link or junction); true when it is gone or was never
// there.
func removePlainDir(dir string, log *logx.Logger) bool {
	st, err := os.Lstat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil:
		log.Warnf("看不了 %s: %v", dir, err)
		return false
	case !plainDir(st):
		log.Warnf("%s 不是普通文件夹，不删", dir)
		return false
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Warnf("没删完 %s，下次打开时再删: %v", dir, err)
		return false
	}
	return true
}

func plainDir(st fs.FileInfo) bool {
	return st.IsDir() && st.Mode()&(fs.ModeSymlink|fs.ModeIrregular) == 0
}

// FindEdge is msedge.exe from the App Paths registration (per user, then per machine, both registry views), else
// the usual install folders; "" when there is none.
func FindEdge() string {
	const appPath = `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\msedge.exe`
	const wow64_64, wow64_32 = 0x0100, 0x0200
	for _, key := range []syscall.Handle{syscall.HKEY_CURRENT_USER, syscall.HKEY_LOCAL_MACHINE} {
		for _, view := range []uint32{wow64_64, wow64_32} {
			if p := regDefault(key, appPath, view); p != "" && isFile(p) {
				return p
			}
		}
	}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LOCALAPPDATA"} {
		if dir := os.Getenv(env); dir != "" {
			if p := filepath.Join(dir, `Microsoft\Edge\Application\msedge.exe`); isFile(p) {
				return p
			}
		}
	}
	return ""
}

// regDefault reads the default value (REG_SZ / REG_EXPAND_SZ) of a registry key, "" when absent.
func regDefault(root syscall.Handle, path string, view uint32) string {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	var k syscall.Handle
	if syscall.RegOpenKeyEx(root, p, 0, syscall.KEY_QUERY_VALUE|view, &k) != nil {
		return ""
	}
	defer syscall.RegCloseKey(k)
	var typ, size uint32
	if syscall.RegQueryValueEx(k, nil, nil, &typ, nil, &size) != nil || size < 2 || size > 64<<10 {
		return ""
	}
	if typ != syscall.REG_SZ && typ != syscall.REG_EXPAND_SZ {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if syscall.RegQueryValueEx(k, nil, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil {
		return ""
	}
	s := strings.Trim(strings.TrimSpace(syscall.UTF16ToString(buf)), `"`)
	if typ == syscall.REG_EXPAND_SZ {
		s = expandEnv(s)
	}
	return s
}

// expandEnv expands %NAME% like ExpandEnvironmentStrings.
func expandEnv(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			break
		}
		name := s[i+1 : i+1+j]
		b.WriteString(s[:i])
		if v, ok := os.LookupEnv(name); ok && name != "" {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+2+j])
		}
		s = s[i+2+j:]
	}
	b.WriteString(s)
	return b.String()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

var (
	modshell32          = syscall.NewLazyDLL("shell32.dll")
	modole32            = syscall.NewLazyDLL("ole32.dll") // a KnownDLL: always mapped from System32
	procShellExecuteW   = modshell32.NewProc("ShellExecuteW")
	procCoInitializeEx  = modole32.NewProc("CoInitializeEx")
	procCoUninitialize  = modole32.NewProc("CoUninitialize")
	coinitApartmentOle1 = uintptr(0x2 | 0x4) // COINIT_APARTMENTTHREADED | COINIT_DISABLE_OLE1DDE
)

// shellOpen is ShellExecuteW "open": a folder opens in Explorer, a URL in the default browser. It runs on a locked
// thread with COM initialized, since ShellExecute may hand the verb to shell extensions over COM.
func shellOpen(target string) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if procCoInitializeEx.Find() == nil {
			if hr, _, _ := procCoInitializeEx.Call(0, coinitApartmentOle1); int32(hr) >= 0 {
				defer procCoUninitialize.Call()
			}
		}
		done <- shellExecute(target)
	}()
	return <-done
}

func shellExecute(target string) error {
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := syscall.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if err := procShellExecuteW.Find(); err != nil {
		return err
	}
	const swShowNormal = 1
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0, 0,
		swShowNormal)
	if r <= 32 { // ShellExecute's "success" is any value above 32
		return fmt.Errorf("ShellExecute 返回 %d", r)
	}
	return nil
}
