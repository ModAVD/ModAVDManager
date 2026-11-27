//go:build !windows

package host

func TerminateRefused(Process) bool { return false }
