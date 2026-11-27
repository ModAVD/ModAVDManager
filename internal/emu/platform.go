package emu

import (
	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/root"
)

// requireWindows gates operations whose host implementation is still Windows-only.
// Keeping this check at the public operation boundary prevents a non-Windows stub
// from creating markers, metadata, locks, or other partial state before failing.
func requireWindows() error {
	if !root.CurrentPlatform().Windows() {
		return errs.New(errs.Unsupported, "当前平台暂不支持此操作")
	}
	return nil
}

// requireLifecycle only opens shared instance operations when the host can prove
// QEMU ownership, lock files and query ports. In particular a no-cgo Darwin build
// must fail before creating instance metadata or partial initialization state.
func requireLifecycle() error {
	p := root.CurrentPlatform()
	if p.Windows() || (p.MirrorOS == "macos" && adb.OwnershipSupported) {
		return nil
	}
	return errs.New(errs.Unsupported, "当前平台暂不支持实例管理")
}
