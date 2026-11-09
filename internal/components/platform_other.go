//go:build !darwin || !arm64 || !cgo

package components

import (
	"modavd/internal/root"
)

const nativeDarwinComponents = false

func (f *fetcher) platformPaths() error                    { return nil }
func (f *fetcher) platformIdle() error                     { return nil }
func (f *fetcher) platformSpace([]root.SDKComponent) error { return nil }

// moveComponent renames a component directory (setting an incomplete one aside, putting it back).
func moveComponent(src, dst string) error { return moveDir(src, dst, false) }

// installComponent moves an unpacked component from the staging directory into place; on Windows it may copy instead
// (moveDir), which is safe only because the source is ours to throw away.
func installComponent(src, dst string) error { return moveDir(src, dst, true) }
