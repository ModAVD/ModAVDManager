//go:build darwin && arm64

package host

import "modavd/internal/macos"

type ManagerWindow = macos.ManagerWindow

func ManagerDataDir() string                 { return macos.ManagerDataDir() }
func ManagerRootIdentity(path string) string { return macos.ManagerRootIdentity(path) }
func ManagerBundleRoot(exe string) string    { return macos.ManagerBundleRoot(exe) }
func LaunchManagerWindow(url string, chrome bool) (*ManagerWindow, error) {
	return macos.LaunchManagerWindow(url, chrome)
}
func OpenDesktop(target string) error { return macos.OpenDesktop(target) }
func MessageBox(message string) error { return macos.ShowManagerMessage(message, false) }
func InfoBox(message string) error    { return macos.ShowManagerMessage(message, true) }
