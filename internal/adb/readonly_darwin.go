//go:build darwin && cgo

package adb

import (
	"modavd/internal/host"
	"modavd/internal/root"
)

// ExistingServerOwned verifies a root's already listening private server. It
// never starts, registers with, replaces or contacts an ADB server, including
// the global server on port 5037. Unreadable ownership fails closed.
func ExistingServerOwned(r *root.Root) bool {
	if r == nil || r.AdbServerPort() == 5037 {
		return false
	}
	port := r.AdbServerPort()
	pid, up, err := host.ListenerPID(port)
	return err == nil && up && pid != 0 && serverBelongsTo(r, port, pid, host.ProcessName(pid))
}
