package ui

import (
	"context"
	"time"

	"modavd/internal/adb"
	"modavd/internal/appproxy"
	"modavd/internal/emu"
)

// backend is the part of emu.Manager the server uses; tests inject a fake. One backend is built per job (its
// logger carries the job's messages), plus one for plain requests and one for the instance poller.
type backend interface {
	List(ctx context.Context) (*emu.ListResult, error)
	Info(ctx context.Context, i int) (*emu.InstanceInfo, error)
	Create(ctx context.Context, req emu.CreateRequest) (*emu.InstanceInfo, error)
	// FreeIndex is the index Create picks when none is given.
	FreeIndex() (int, error)
	Update(ctx context.Context, i int, req emu.UpdateRequest) (*emu.UpdateResult, error)
	PreviewDiagnostics(ctx context.Context, i int) (*emu.DiagnosticsPreview, error)
	Delete(ctx context.Context, i int) (*emu.DeleteResult, error)
	Start(ctx context.Context, i int, opt emu.StartOptions) (*emu.StartResult, error)
	Stop(ctx context.Context, i int, opt emu.StopOptions) (*emu.StopResult, error)
	Check(ctx context.Context, opt emu.CheckOptions) (*emu.CheckResult, error)
	Shell(ctx context.Context, i int, command string, timeout time.Duration) (*emu.ShellResult, error)
	InstallAPK(ctx context.Context, i int, apkPath string) (*emu.InstallResult, error)
	// DiskTrim / DiskInfo / DiskCompact are the data-disk reclaim (modavd disk): trim needs the instance booted,
	// info works either way, compact needs it shut down.
	DiskTrim(ctx context.Context, i int) (*emu.DiskTrimResult, error)
	DiskInfo(ctx context.Context, i int) (*emu.DiskInfoResult, error)
	DiskCompact(ctx context.Context, i int) (*emu.DiskCompactResult, error)
	// PlayStore is `modavd playstore <i> <action>`; PlayStoreCheck what it refuses before touching the instance (no
	// bridge, the root has not got the files ...), quick enough for a request.
	PlayStore(ctx context.Context, i int, action string) (*emu.PlayStoreResult, error)
	PlayStoreCheck(i int, action string) error
	// PlayStoreGSFID is `modavd playstore <i> gsfid`: a few seconds on a running instance.
	PlayStoreGSFID(ctx context.Context, i int) (*emu.PlayStoreGSFIDResult, error)
	// AdbRun is Manager.ADB.Run (the screenshot: exec-out screencap -p with Raw).
	AdbRun(ctx context.Context, opt adb.RunOptions, args ...string) adb.Result
	// Serial is instance i's adb serial in the root's port plan.
	Serial(i int) string
	// RootInfo describes the root without asking any instance (GET /api/info).
	RootInfo() emu.RootInfo
	// The per-app proxy (emu/appproxy.go). SuggestAppProxy and ProbeAppProxy are emu's functions of those names: they
	// ask the host, not an instance.
	AppProxy(ctx context.Context, i int) (*emu.AppProxyResult, error)
	SetAppProxy(ctx context.Context, i int, c appproxy.Change) (*emu.AppProxyResult, error)
	SuggestAppProxy(ctx context.Context) (*emu.AppProxySuggestion, error)
	ProbeAppProxy(ctx context.Context, server string, withUDP bool) (*emu.AppProxyProbe, error)
}

// managerBackend adapts *emu.Manager.
type managerBackend struct{ *emu.Manager }

var _ backend = managerBackend{}

func (b managerBackend) AdbRun(ctx context.Context, opt adb.RunOptions, args ...string) adb.Result {
	return b.ADB.Run(ctx, opt, args...)
}

func (b managerBackend) Serial(i int) string { return b.Root.Ports().Serial(i) }

func (managerBackend) SuggestAppProxy(ctx context.Context) (*emu.AppProxySuggestion, error) {
	return emu.SuggestAppProxy(ctx)
}

func (managerBackend) ProbeAppProxy(ctx context.Context, server string, withUDP bool) (*emu.AppProxyProbe, error) {
	return emu.ProbeAppProxy(ctx, server, withUDP)
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
		PortBase:            r.Ports().Base,
		Version:             r.VersionText(),
	}
	if n, ok := r.ModBuild(); ok {
		info.Build = n
	}
	return info
}
