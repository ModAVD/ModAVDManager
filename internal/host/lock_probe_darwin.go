//go:build darwin && cgo

package host

import "modavd/internal/macos"

func ExclusiveLockHeld(path string) (bool, error) { return macos.ExclusiveLockHeld(path) }
