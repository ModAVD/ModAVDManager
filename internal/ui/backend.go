//go:build windows

package ui

import (
	"context"
	"time"

	"modavd/internal/adb"
	"modavd/internal/emu"
)

// backend is the part of emu.Manager the server uses; tests inject a fake. One backend is built per job (its
// logger carries the job's messages), plus one for plain requests and one for the instance poller.
type backend interface {
	List(ctx context.Context) (*emu.ListResult, error)
	Info(ctx context.Context, i int) (*emu.InstanceInfo, error)
	Create(ctx context.Context, req emu.CreateRequest) (*emu.InstanceInfo, error)
	Update(ctx context.Context, i int, req emu.UpdateRequest) (*emu.UpdateResult, error)
	Delete(ctx context.Context, i int) (*emu.DeleteResult, error)
	Start(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error)
	Stop(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error)
	Check(ctx context.Context, opt emu.CheckOptions) (*emu.CheckResult, error)
	SetWindowVisible(ctx context.Context, i int, visible bool) (*emu.WindowResult, error)
	Shell(ctx context.Context, i int, command string, timeout time.Duration) (*emu.ShellResult, error)
	InstallAPK(ctx context.Context, i int, apkPath string) (*emu.InstallResult, error)
	// AdbRun is Manager.ADB.Run (the screenshot: exec-out screencap -p with Raw).
	AdbRun(ctx context.Context, opt adb.RunOptions, args ...string) adb.Result
	// RootInfo describes the root without asking any instance (GET /api/info).
	RootInfo() emu.RootInfo
}

// managerBackend adapts *emu.Manager.
type managerBackend struct{ *emu.Manager }

var _ backend = managerBackend{}

func (b managerBackend) AdbRun(ctx context.Context, opt adb.RunOptions, args ...string) adb.Result {
	return b.ADB.Run(ctx, opt, args...)
}

// RootInfo is what emu fills into ListResult / CheckResult (emu's own helper is unexported); List would cost an adb
// call per running instance.
func (b managerBackend) RootInfo() emu.RootInfo {
	r := b.Root
	info := emu.RootInfo{
		Root:                r.Dir,
		SDK:                 r.RuntimeSDK(),
		LocalSDK:            r.LocalSDKRoot() != "",
		AdbExe:              r.AdbExe(),
		AdbServerPort:       r.AdbServerPort(),
		ScriptAdbServerPort: r.ScriptAdbServerPort(),
		Version:             r.VersionText(),
	}
	if n, ok := r.ModBuild(); ok {
		info.Build = n
	}
	return info
}
