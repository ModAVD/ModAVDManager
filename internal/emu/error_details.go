package emu

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strconv"

	"modavd/internal/adb"
	"modavd/internal/avd"
	"modavd/internal/errs"
	"modavd/internal/root"
)

const unknownState = "unknown"

// Replaced only by error-observation tests, to model avd.Get's lossy NotFound after a file attribute read fails.
var errorInstanceForState = avd.Get

// errorRunning takes one read-only process snapshot. Lifecycle scans deliberately tolerate unreadable processes;
// error details must instead say unknown when that could hide this instance. No lock is acquired or removed.
// Replaced in tests that run creation without an emulator.
var errorRunning = func(m *Manager, i int) string {
	// Ports() falls back to the default plan on unreadable metadata. That is useful to best-effort lifecycle
	// queries, but cannot prove a custom-port qemu is absent from this root.
	meta, err := m.Root.ReadMetadata()
	if err != nil {
		return unknownState
	}
	plan, problem := root.PortPlanOf(meta.Obj())
	// Legacy risk warnings still preserve the actual plan for observation and shutdown. Only malformed values
	// that fell back to the default segment make a negative process observation unreliable.
	if problem != "" && plan.Base == root.DefaultPortBase {
		return unknownState
	}
	procs, err := listQemuProcesses()
	if err != nil {
		return unknownState
	}
	running := "false"
	console := plan.Console(i)
	for _, p := range procs {
		if !isQemuName(p.Name) {
			continue
		}
		args, err := p.CommandLine()
		if err != nil {
			running = unknownState
			continue
		}
		if !cmdlineMatches(args, root.AvdName(i), console) {
			continue
		}
		if adb.QemuBelongsTo(m.Root, p, i) {
			return "true"
		}
		// A matching process without confirmed ownership may be this root's early boot. Do not call it offline.
		running = unknownState
	}
	return running
}

// startFailure reuses this one process observation for both diagnostics and details. bootStatus is what the
// failed phase established, starting during the boot wait, unknown during later setup which can restart Android.
func (m *Manager) startFailure(ctx context.Context, i int, err error, bootStatus string) error {
	running := errorRunning(m, i)
	err = m.saveBootFailure(ctx, i, err, running == "true")
	status := unknownState
	switch running {
	case "false":
		status = string(StatusOffline)
	case "true":
		status = bootStatus
	}
	return errs.WithDetails(err, map[string]string{errs.InstanceStatus: status, errs.QemuRunning: running})
}

// withCreateErrorState observes files, metadata and qemu after cleanup, without calling Info (which probes our own
// locks and makes adb calls). ownCreation means its create lock is still held here but will be released on return:
// a recorded creating then reads create_failed afterwards, even if persisting failed did not succeed.
func (m *Manager) withCreateErrorState(i int, err error, ownCreation bool) error {
	details := map[string]string{errs.InstanceExists: unknownState, errs.InstanceStatus: unknownState, errs.QemuRunning: unknownState}
	if m.Root == nil || !root.ValidIndex(i) {
		return errs.WithDetails(err, details)
	}
	exists := "false"
	for _, path := range []string{avd.IniPath(m.Root, i), avd.Dir(m.Root, i)} {
		_, ferr := os.Lstat(path)
		if ferr == nil {
			exists = "true"
		} else if !errors.Is(ferr, fs.ErrNotExist) && exists != "true" {
			exists = unknownState
		}
	}
	creation := ""
	meta, merr := m.Root.ReadMetadata()
	if merr != nil {
		if exists != "true" {
			exists = unknownState
		}
	} else if instance := meta.Obj().Get("instances").Obj().Get(strconv.Itoa(i)); instance != nil {
		exists = "true"
		creation = metaText(instance.Obj().Get("creationState"))
	}
	details[errs.InstanceExists] = exists
	running := errorRunning(m, i)
	details[errs.QemuRunning] = running
	switch {
	case creation == creationFailed || ownCreation && creation == creationCreating:
		details[errs.InstanceStatus] = string(StatusCreateFailed)
	case creation == creationCreating || merr != nil:
		// Another creator may still be working, or metadata was unreadable. Do not probe or disturb its lock.
	case exists == "false":
		details[errs.InstanceStatus] = "not_found"
	case exists == "true":
		// Get's NotFound also covers a config.ini attribute read failure. With files/metadata present it cannot
		// distinguish incomplete files from an unreadable instance, so keep unknown rather than claim absence.
		if _, ierr := errorInstanceForState(m.Root, i); ierr == nil && running == "false" {
			details[errs.InstanceStatus] = string(StatusOffline)
		}
	}
	return errs.WithDetails(err, details)
}
