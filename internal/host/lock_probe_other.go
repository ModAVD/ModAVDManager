//go:build !windows && (!darwin || !cgo)

package host

func ExclusiveLockHeld(string) (bool, error) { return false, errUnsupported }
