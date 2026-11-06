package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// RunWithTempUserDirs runs a package's tests with the per-user directories pointed at a new temporary directory:
// LOCALAPPDATA on Windows, and the directories os.UserConfigDir reads elsewhere. No test of the package then reads or
// writes the real %LOCALAPPDATA%\ModAVD (the component download source, update settings, the affinity lock) or
// sees the emulators running on this computer through %LOCALAPPDATA%\Temp\avd\running, even one that forgets to
// set its own. Use it as: func TestMain(m *testing.M) { os.Exit(testutil.RunWithTempUserDirs(m)) }.
func RunWithTempUserDirs(m *testing.M) int {
	dir, err := os.MkdirTemp("", "modavd-userdirs-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "testutil: no temporary user directory:", err)
		return 1
	}
	defer removeAll(dir, 2*time.Second, os.RemoveAll)
	set := map[string]string{"LOCALAPPDATA": filepath.Join(dir, "LocalAppData")}
	if runtime.GOOS != "windows" {
		set["XDG_CONFIG_HOME"] = filepath.Join(dir, "config")
		set["HOME"] = dir
	}
	for k, v := range set {
		if err := os.MkdirAll(v, 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "testutil:", err)
			return 1
		}
		os.Setenv(k, v)
	}
	return m.Run()
}
