//go:build darwin && arm64

package macos

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ManagerWindow describes only the browser process started for the manager.
// A default-browser launch has no owned process and is tracked by page activity.
type ManagerWindow struct {
	App     bool
	Started time.Time
	Done    <-chan struct{}
}

// ManagerDataDir is this user's manager state and optional Chrome profile.
func ManagerDataDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		// Keep a failed home-directory lookup separate from other local users.
		return filepath.Join(os.TempDir(), fmt.Sprintf("ModAVD-%d", os.Getuid()))
	}
	return filepath.Join(base, "ModAVD")
}

// ManagerRootIdentity identifies aliases of one root on both case-sensitive
// and case-insensitive volumes, without conflating distinct case-sensitive roots.
func ManagerRootIdentity(path string) string {
	if info, err := os.Stat(path); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino)
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

// ManagerBundleRoot recognizes <root>/*.app/Contents/MacOS/<executable>.
// It deliberately does not search arbitrary ancestors above an executable.
func ManagerBundleRoot(exe string) string {
	dir := filepath.Dir(filepath.Clean(exe))
	contents := filepath.Dir(dir)
	bundle := filepath.Dir(contents)
	if filepath.Base(dir) != "MacOS" || filepath.Base(contents) != "Contents" ||
		!strings.EqualFold(filepath.Ext(bundle), ".app") {
		return ""
	}
	return filepath.Dir(bundle)
}

// LaunchManagerWindow uses the default browser unless Chrome was explicitly
// requested. Chrome always gets a separate profile with sync and extensions off.
func LaunchManagerWindow(url string, chrome bool) (*ManagerWindow, error) {
	if chrome {
		if exe := findChrome(); exe != "" {
			profile := filepath.Join(ManagerDataDir(), "ui-chrome")
			if err := os.MkdirAll(profile, 0o700); err == nil {
				cmd := exec.Command(exe, chromeArgs(url, profile)...)
				if err := cmd.Start(); err == nil {
					done := make(chan struct{})
					go func() {
						_ = cmd.Wait()
						close(done)
					}()
					return &ManagerWindow{App: true, Started: time.Now(), Done: done}, nil
				}
			}
		}
	}
	if err := OpenDesktop(url); err != nil {
		return nil, fmt.Errorf("打不开默认浏览器: %w", err)
	}
	return &ManagerWindow{Started: time.Now()}, nil
}

func chromeArgs(url, profile string) []string {
	return []string{"--app=" + url, "--user-data-dir=" + profile, "--no-first-run", "--no-default-browser-check",
		"--disable-sync", "--disable-extensions", "--window-size=1180,760"}
}

func findChrome() string {
	paths := []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, "Applications/Google Chrome.app/Contents/MacOS/Google Chrome"))
	}
	for _, path := range paths {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0 {
			return path
		}
	}
	return ""
}

// OpenDesktop asks Launch Services to open a URL or a folder, with no shell.
func OpenDesktop(target string) error { return exec.Command("/usr/bin/open", "--", target).Run() }

// ShowManagerMessage keeps the message in argv instead of interpolating it into
// AppleScript, so paths and error text cannot become executable script.
func ShowManagerMessage(message string, info bool) error {
	icon := "stop"
	if info {
		icon = "note"
	}
	script := "on run argv\ndisplay dialog (item 1 of argv) with title \"ModAVD\" buttons {\"好\"} default button 1 with icon " + icon + "\nend run"
	return exec.Command("/usr/bin/osascript", "-e", script, "--", message).Run()
}
