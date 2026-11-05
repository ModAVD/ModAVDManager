//go:build !windows

package win

func ExclusiveLockHeld(string) (bool, error) { return false, errUnsupported }
