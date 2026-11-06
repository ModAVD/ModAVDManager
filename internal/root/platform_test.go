package root

import (
	"runtime"
	"testing"
)

func TestCurrentPlatformMatchesHost(t *testing.T) {
	p := CurrentPlatform()
	switch {
	case runtime.GOOS == "windows" && runtime.GOARCH == "amd64":
		want := Platform{Emulator: "emulator.exe", ADB: "adb.exe", QEMU: "qemu-system-x86_64.exe", QEMUHeadless: "qemu-system-x86_64-headless.exe", QemuDir: "windows-x86_64", PackageSuffix: "win-x64", MirrorOS: "win", MirrorArch: "x64", GuestABI: "x86_64", GuestArch: "x86_64", TargetAPI: "android-34", SystemImageDir: SystemImageDir, SystemImageName: SystemImageName}
		if p != want {
			t.Fatalf("Windows platform: %+v", p)
		}
	case runtime.GOOS == "darwin" && runtime.GOARCH == "arm64":
		want := Platform{Emulator: "emulator", ADB: "adb", QEMU: "qemu-system-aarch64", QEMUHeadless: "qemu-system-aarch64-headless", QemuDir: "darwin-aarch64", PackageSuffix: "macos-arm64", MirrorOS: "macos", MirrorArch: "arm64", GuestABI: "arm64-v8a", GuestArch: "arm64", TargetAPI: "android-35", SystemImageDir: "system-images/android-35/google_apis/arm64-v8a", SystemImageName: "系统镜像 Android 15 Google APIs arm64-v8a"}
		if p != want {
			t.Fatalf("darwin platform: %+v", p)
		}
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		want := Platform{Emulator: "emulator", ADB: "adb", QEMU: "qemu-system-x86_64", QEMUHeadless: "qemu-system-x86_64", PackageSuffix: "linux-x64", MirrorOS: "linux", MirrorArch: "x64", GuestABI: "x86_64", GuestArch: "x86_64", TargetAPI: "android-34", SystemImageDir: "system-images/android-34/google_apis/x86_64", SystemImageName: SystemImageName}
		if p != want {
			t.Fatalf("linux platform: %+v", p)
		}
	default:
		if p != (Platform{}) {
			t.Fatalf("unsupported platform inherited values: %+v", p)
		}
	}
}
