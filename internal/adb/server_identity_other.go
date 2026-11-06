//go:build !darwin || !cgo

package adb

import (
	"modavd/internal/host"
	"modavd/internal/root"
)

const serverConflictReason = "它不是 adb"

func prepareServerRoot(*root.Root) error { return nil }

func serverBaseEnv(env []string) []string { return env }

// Preserve the native Windows server check, including an unreadable name.
func serverBelongsTo(_ *root.Root, _ int, _ uint32, name string) bool {
	return name == "" || isAdbName(name)
}

// Keep Windows' original startup wait: it observes the listening state after
// spawning adb. Darwin additionally verifies who acquired the port.
func (c *Client) startedServerListening(_ serverKind, port int) (bool, error) {
	return listening(port)
}

func registrationStillOwned(*Client, int, host.Process) bool { return true }
func qemuListenerUnchanged(host.Process, int) bool           { return true }
