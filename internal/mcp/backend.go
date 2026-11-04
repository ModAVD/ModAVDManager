package mcp

import (
	"context"
	"time"

	"modavd/internal/appproxy"
	"modavd/internal/emu"
)

// backend is the part of emu.Manager the tools call. *emu.Manager implements it as is; tests inject a fake.
type backend interface {
	List(ctx context.Context) (*emu.ListResult, error)
	Info(ctx context.Context, i int) (*emu.InstanceInfo, error)
	Check(ctx context.Context, opt emu.CheckOptions) (*emu.CheckResult, error)
	Create(ctx context.Context, req emu.CreateRequest) (*emu.InstanceInfo, error)
	Update(ctx context.Context, i int, req emu.UpdateRequest) (*emu.UpdateResult, error)
	Delete(ctx context.Context, i int) (*emu.DeleteResult, error)
	Start(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error)
	Stop(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error)
	SetWindowVisible(ctx context.Context, i int, visible bool) (*emu.WindowResult, error)
	Shell(ctx context.Context, i int, command string, timeout time.Duration) (*emu.ShellResult, error)
	InstallAPK(ctx context.Context, i int, apkPath string) (*emu.InstallResult, error)
	DiskInfo(ctx context.Context, i int) (*emu.DiskInfoResult, error)
	DiskTrim(ctx context.Context, i int) (*emu.DiskTrimResult, error)
	DiskCompact(ctx context.Context, i int) (*emu.DiskCompactResult, error)
	AppProxy(ctx context.Context, i int) (*emu.AppProxyResult, error)
	SetAppProxy(ctx context.Context, i int, c appproxy.Change) (*emu.AppProxyResult, error)
	// Wait waits up to timeout for the background work the manager started (the logcat restart after a
	// re-registration). Called after each call's result is sent; Serve waits for those waits before it returns.
	Wait(timeout time.Duration)
}

var _ backend = (*emu.Manager)(nil)
