//go:build windows

package win

import "syscall"

// TerminateRefused reports whether a process cannot be opened for termination
// and is still the process identified by the snapshot. ERROR_INVALID_PARAMETER
// means the process is already gone; a reused PID is rejected by its creation
// time when that value is available.
func TerminateRefused(p Process) bool {
	if p.PID == 0 {
		return false
	}
	h, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, p.PID)
	if err == nil {
		syscall.CloseHandle(h)
		return false
	}
	if err == errorInvalidParameter {
		return false
	}
	if p.Created != 0 {
		if h, err := syscall.OpenProcess(processQueryLimitedInformation, false, p.PID); err == nil {
			defer syscall.CloseHandle(h)
			var c, e, k, u syscall.Filetime
			if syscall.GetProcessTimes(h, &c, &e, &k, &u) == nil && uint64(c.HighDateTime)<<32|uint64(c.LowDateTime) != p.Created {
				return false
			}
		}
	}
	return true
}
