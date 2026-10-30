//go:build windows

package emu

import (
	"context"
	"time"
)

// Waits for the servers to listen, as in AUTO-MAS ensure_adb_server / ensure_script_adb_server.
const (
	privateServerWait = 20 * time.Second
	scriptServerWait  = 10 * time.Second
)

// EnsureServers starts the private adb server and the scripts' adb server (detached) when not running.
//
// Both are started by us, detached: a server that a script's adb client spawns inherits that client's output pipe,
// and whoever reads the pipe then waits until the server exits. A port held by something that is not adb is
// errs.PortConflict (change adbServerPort / scriptAdbServerPort in mas-avd.json).
func (m *Manager) EnsureServers(ctx context.Context) (*ServersResult, error) {
	res := &ServersResult{AdbServerPort: m.Root.AdbServerPort(), ScriptAdbServerPort: m.Root.ScriptAdbServerPort()}
	up, err := m.ADB.EnsureServer(ctx, privateServerWait)
	if err != nil {
		return nil, err
	}
	res.AdbServerUp = up
	if err := ctxErr(ctx); err != nil {
		return nil, err
	}
	up, err = m.ADB.EnsureScriptServer(ctx, scriptServerWait)
	if err != nil {
		return nil, err
	}
	res.ScriptAdbServerUp = up
	if !res.AdbServerUp {
		m.Log.Warnf("私有 adb server（端口 %d）%.0f 秒内没起来", res.AdbServerPort, privateServerWait.Seconds())
	}
	if !res.ScriptAdbServerUp {
		m.Log.Warnf("脚本专用 adb server（端口 %d）%.0f 秒内没起来", res.ScriptAdbServerPort, scriptServerWait.Seconds())
	}
	return res, nil
}
