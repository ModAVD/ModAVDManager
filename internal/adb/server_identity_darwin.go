//go:build darwin && cgo

package adb

import (
	"os"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

const serverConflictReason = "无法确认它是这个根目录的 adb"

// Ownership compares the AVD home by filesystem identity. Prepare that empty
// directory before launching a server for a root without any instances yet.
// Existing listeners are checked before this function, so a foreign server
// cannot cause a directory to be created or another server to be started.
func prepareServerRoot(r *root.Root) error {
	if err := os.MkdirAll(r.AvdHome(), 0o755); err != nil {
		return errs.Wrap(errs.Internal, err, "创建私有 adb 的 AVD 目录失败: "+r.AvdHome())
	}
	return nil
}

// An inherited socket/address can redirect an adb client away from its private
// port despite the emulator's ANDROID_ADB_SERVER_PORT. Keep the native command
// line's -P authoritative for both the private and script servers.
func serverBaseEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		switch envKey(entry) {
		case "ADB_SERVER_SOCKET", "ANDROID_ADB_SERVER_ADDRESS":
			continue
		}
		out = append(out, entry)
	}
	return out
}

func serverBelongsTo(r *root.Root, port int, pid uint32, name string) bool {
	if r == nil || !isAdbName(name) {
		return false
	}
	p, ok := host.FindProcess(pid)
	if !ok || !isAdbName(p.Name) {
		return false
	}
	exe, err := p.ImagePath()
	if err != nil || !samePath(exe, r.AdbExe()) {
		return false
	}
	env, ok := processEnvironment(p)
	if !ok || !samePath(env["ANDROID_AVD_HOME"], r.AvdHome()) ||
		!samePath(env["ANDROID_SDK_ROOT"], r.RuntimeSDK()) {
		return false
	}
	for key, value := range root.NoLocalScanEnv {
		if env[key] != value {
			return false
		}
	}
	return qemuListenerUnchanged(p, port)
}

func (c *Client) startedServerListening(k serverKind, port int) (bool, error) {
	return c.serverListening(k, port)
}

func qemuListenerUnchanged(p host.Process, port int) bool {
	pid, up, err := host.ListenerPID(port)
	return err == nil && up && pid == p.PID && processUnchanged(p)
}

func registrationStillOwned(c *Client, console int, previous host.Process) bool {
	p, ok := QemuOnAdbPort(c.Root, console)
	if !ok || p.PID != previous.PID || p.Created != previous.Created {
		return false
	}
	up, err := c.serverListening(privateServer, c.Root.AdbServerPort())
	return err == nil && up
}
