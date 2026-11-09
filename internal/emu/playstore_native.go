package emu

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/playstore"
	"modavd/internal/root"
)

// Native ARM64 uses its own writable boot request, independent of the x86
// execution bridge and supported by Google's unmodified -writable-system flag.
const playStoreMarkerName = "modavd-playstore-writable-system"
const playStoreMarkerText = "ModAVD Google Play Store: keep -writable-system until playstore uninstall confirms absent.\n"

func nativePlayStore() bool {
	p := root.CurrentPlatform()
	return p.MirrorOS == "macos" && p.GuestABI == "arm64-v8a" && adb.OwnershipSupported
}

// An imported/external AVD must not receive files for a different API or ABI.
// The SDK component's own pins are checked separately before an install.
func playStoreGuestError(cfg *avd.Ini) error {
	p := playstore.CurrentProfile()
	abi, arch := strings.ToLower(cfg.Value("abi.type")), strings.ToLower(cfg.Value("hw.cpu.arch"))
	sysdir := strings.TrimRight(strings.ReplaceAll(cfg.Value("image.sysdir.1"), `\`, "/"), "/")
	want := strings.TrimRight(strings.ReplaceAll(root.CurrentPlatform().SystemImageDir, `\`, "/"), "/")
	if abi != p.ABI || (arch != "arm64" && arch != "aarch64" && arch != "arm64-v8a") || sysdir != want {
		return errs.WithDetail(errs.Newf(errs.Unsupported, "Google Play 商店只支持本平台的 Android %s Google APIs %s 实例", p.API, p.ABI),
			errs.Reason, PlayStoreReasonUnsupported)
	}
	return nil
}

func (m *Manager) playStoreMarker(i int) string {
	return filepath.Join(avd.Dir(m.Root, i), playStoreMarkerName)
}

func (m *Manager) writePlayStoreMarker(i int) error {
	path := m.playStoreMarker(i)
	st, err := os.Lstat(path)
	if err == nil {
		if !st.Mode().IsRegular() {
			return errs.Newf(errs.PrecheckFailed, "Google Play 商店的开机标记不是普通文件：%s", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errs.Wrap(errs.Internal, err, "读不了 Google Play 商店的开机标记")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return errs.Wrap(errs.Internal, err, "写不了 Google Play 商店的开机标记")
	}
	_, err = f.WriteString(playStoreMarkerText)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return errs.Wrap(errs.Internal, err, "保存 Google Play 商店的开机标记失败")
	}
	return nil
}

func (m *Manager) removePlayStoreMarker(i int) error {
	path := m.playStoreMarker(i)
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !st.Mode().IsRegular() {
		return errs.Newf(errs.PrecheckFailed, "无法移除 Google Play 商店的开机标记：%s", path)
	}
	if err := os.Remove(path); err != nil {
		return errs.Wrap(errs.Internal, err, "移除 Google Play 商店的开机标记失败")
	}
	return nil
}

// Called for every native launch, including a boot after a process interruption.
// Keep the marker through partial installs and uninstalls; clear it only after
// the guest confirms all files and the placeholder have returned to stock.
func (m *Manager) playStoreLaunchArgs(in *avd.Instance, args []string) ([]string, error) {
	if !nativePlayStore() {
		return args, nil
	}
	_, err := os.Lstat(m.playStoreMarker(in.Index))
	if errors.Is(err, os.ErrNotExist) && !m.playStoreWanted(in.Index) {
		return args, nil
	}
	if err := playStoreGuestError(in.Config); err != nil {
		return nil, err
	}
	if err := m.writePlayStoreMarker(in.Index); err != nil {
		return nil, err
	}
	qemu := slices.Index(args, "-qemu")
	if qemu < 0 {
		qemu = len(args)
	}
	return slices.Insert(args, qemu, "-writable-system"), nil
}
