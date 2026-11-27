//go:build darwin && cgo

// Package macos implements the macOS host APIs using libproc, sysctl and flock.
package macos

import "os"

// Process identifies a local process by PID and creation time in Unix microseconds.
type Process struct {
	PID     uint32
	PPID    uint32
	Name    string
	Created uint64
}

// Listener is a listening TCP socket; PID is zero when ownership is unreadable.
type Listener struct {
	Port int
	PID  uint32
}

// StartOptions controls the working directory, environment and child output.
type StartOptions struct {
	Dir        string
	Env        []string
	Output     *os.File
	OutputPath string
}

// FileLock holds an exclusive flock until Unlock or process exit.
type FileLock struct{ f *os.File }

const SpawnHelperArg = "__spawn"
