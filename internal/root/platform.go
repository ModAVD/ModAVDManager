package root

import (
	"runtime"
	"strings"
)

func platformKeyFiles() map[string][]string {
	p := CurrentPlatform()
	qemu := p.QEMU
	if p.QemuDir != "" {
		qemu = "qemu/" + p.QemuDir + "/" + p.QEMU
	}
	if p.QemuDir == "" {
		qemu = "qemu/" + p.QEMU
	}
	return map[string][]string{
		"emulator": {p.Emulator, qemu},
	}
}

// Platform describes host executable names, guest images and update identifiers.
// Callers use this table instead of embedding Windows or x86_64 values in shared logic.
type Platform struct {
	Emulator        string
	ADB             string
	QEMU            string
	QEMUHeadless    string
	QemuDir         string
	PackageSuffix   string
	MirrorOS        string
	MirrorArch      string
	GuestABI        string
	GuestArch       string
	TargetAPI       string
	SystemImageDir  string
	SystemImageName string
}

// Supported reports whether the host matches one of the package's platform rows.
// An empty platform is returned for targets that are not listed (for example
// windows/arm64), and must not inherit the Windows implementation.
func (p Platform) Supported() bool { return p.Emulator != "" }

// Windows reports whether the host is the Windows/amd64 row used by the
// Windows-only implementation.
func (p Platform) Windows() bool { return p.QemuDir == "windows-x86_64" && p.MirrorOS == "win" }

// CLI is the root-local console executable used by background workers.
func (p Platform) CLI() string {
	if !p.Supported() {
		return ""
	}
	if p.Windows() {
		return "modavd.exe"
	}
	return "modavd"
}

func CurrentPlatform() Platform {
	switch {
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		return Platform{
			Emulator: "emulator", ADB: "adb", QEMU: "qemu-system-aarch64", QEMUHeadless: "qemu-system-aarch64-headless", QemuDir: "darwin-aarch64",
			PackageSuffix: "macos-arm64", MirrorOS: "macos", MirrorArch: "arm64",
			GuestABI: "arm64-v8a", GuestArch: "arm64", TargetAPI: "android-35",
			SystemImageDir: MacOSSystemImageDir, SystemImageName: MacOSSystemImageName,
		}
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		return Platform{
			Emulator: "emulator", ADB: "adb", QEMU: "qemu-system-x86_64", QEMUHeadless: "qemu-system-x86_64",
			PackageSuffix: "linux-x64", MirrorOS: "linux", MirrorArch: "x64",
			GuestABI: "x86_64", GuestArch: "x86_64", TargetAPI: "android-34",
			SystemImageDir: strings.ReplaceAll(SystemImageDir, `\`, "/"), SystemImageName: SystemImageName,
		}
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		return Platform{
			Emulator: "emulator.exe", ADB: "adb.exe", QEMU: "qemu-system-x86_64.exe", QEMUHeadless: "qemu-system-x86_64-headless.exe", QemuDir: "windows-x86_64",
			PackageSuffix: "win-x64", MirrorOS: "win", MirrorArch: "x64",
			GuestABI: "x86_64", GuestArch: "x86_64", TargetAPI: "android-34", SystemImageDir: SystemImageDir, SystemImageName: SystemImageName,
		}
	default:
		// An unlisted host must not inherit Windows names or MirrorChyan ids.
		return Platform{}
	}
}
