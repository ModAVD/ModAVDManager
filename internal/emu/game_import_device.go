// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"modavd/internal/errs"
	"modavd/internal/gamesource"
	"modavd/internal/host"
)

type importDevice struct {
	m        *Manager
	serial   string
	bootID   string
	identity string
	exec     func(ctx context.Context, args ...string) (string, error) // replaces adb in tests
	// fresh: this import made the connection to serial. One that was already on the script adb server belongs to
	// whoever made it and is not ours to drop.
	fresh bool
}

func (d importDevice) command(ctx context.Context, args ...string) *exec.Cmd {
	route := []string{"-P", strconv.Itoa(d.m.Root.ScriptAdbServerPort())}
	if d.serial != "" {
		route = append(route, "-s", d.serial)
	}
	cmd := exec.CommandContext(ctx, d.m.Root.AdbExe(), append(route, args...)...)
	cmd.Env = d.m.ADB.Env()
	host.HideConsoleCmd(cmd)
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

func (d importDevice) output(ctx context.Context, args ...string) (string, error) {
	if d.exec != nil {
		return d.exec(ctx, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	cmd := d.command(ctx, args...)
	var msg bytes.Buffer
	cmd.Stderr = &msg
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", errs.Newf(errs.Timeout, "adb %s 超时", args[0])
		}
		if e := ctxErr(ctx); e != nil {
			return "", e
		}
		return "", fmt.Errorf("adb %s: %w (%s)", args[0], err, tail(msg.String()+" "+string(out), 400))
	}
	return string(out), nil
}

func (d importDevice) shell(ctx context.Context, s string) (string, error) {
	return d.output(ctx, "shell", "-T", "-n", d.script(s))
}

// Public resources are written by their destination app UID.
func (d importDevice) script(s string) string {
	if d.identity != "" {
		s = "exec su " + d.identity + " sh -c " + importQuote(s)
	}
	// A server restart can reuse a transport ID. Bind each operation to both
	// the requested TCP endpoint and the same Android boot, including cleanup.
	if d.bootID != "" {
		s = "test \"$(cat /proc/sys/kernel/random/boot_id)\" = " + importQuote(d.bootID) + " || exit 75; " + s
	}
	return s
}

func (m *Manager) connectImportDevice(ctx context.Context, port int) (importDevice, error) {
	r, err := m.Root.SnapshotRuntime()
	if err != nil {
		return importDevice{}, err
	}
	m = m.withRuntime(r)
	d := importDevice{m: m}
	up, err := m.ADB.EnsureScriptServer(ctx, 20*time.Second)
	if err != nil {
		return d, err
	}
	if !up {
		return d, errs.New(errs.NotReady, "脚本 adb server 未就绪")
	}
	serial := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	// LDPlayer can leave its previous TCP transport offline across a reboot.
	// Remove only a stale record for this endpoint. Other devices share the server.
	devices, err := d.output(ctx, "devices")
	if err != nil {
		return d, err
	}
	for line := range strings.SplitSeq(devices, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == serial && f[1] == "offline" {
			if _, err := d.output(ctx, "disconnect", serial); err != nil {
				return d, err
			}
			break
		}
	}
	msg, err := d.output(ctx, "connect", serial)
	if err != nil {
		return d, err
	}
	d.serial = serial
	d.fresh = connectIsNew(msg)
	id, err := d.shell(ctx, "cat /proc/sys/kernel/random/boot_id")
	if err != nil {
		return d, errs.Newf(errs.NotReady, "连不上 %s：%s", serial, strings.TrimSpace(msg)+" "+err.Error())
	}
	id = strings.TrimSpace(id)
	decoded, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	if err != nil || len(decoded) != 16 {
		return d, errs.Newf(errs.NotReady, "无法确认设备 %s 的开机标识", serial)
	}
	d.bootID = id
	return d, nil
}

// importPortConflict reports whether a source's adb port belongs to ModAVD: one of the instances' ports, the default
// adb server's 5037, or one of the reserved adb server ports.
func importPortConflict(port int, instancePorts map[int]bool, reserved ...int) bool {
	return instancePorts[port] || port == 5037 || slices.Contains(reserved, port)
}

// connectIsNew reads the reply of `adb connect`: "connected to <serial>" made the connection,
// "already connected to <serial>" found one.
func connectIsNew(reply string) bool {
	return strings.HasPrefix(strings.TrimSpace(reply), "connected to ")
}

// disconnect drops the transport this import created from the script adb server, which every script on the machine
// shares; a connection that was there already is left alone. Failing only leaves the connection where it was, so it
// is logged.
func (d importDevice) disconnect() {
	if d.serial == "" || !d.fresh {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	server := d
	server.serial = ""
	if _, err := server.output(ctx, "disconnect", d.serial); err != nil {
		d.m.Log.Warnf("断开与 %s 的 adb 连接失败：%v", d.serial, err)
	}
}

// gameSource connects to the source emulator on the script adb server. A failure drops the connection it made.
func (m *Manager) gameSource(ctx context.Context, source gamesource.Source) (_ importDevice, failure error) {
	// created: one of the connects of the retry loop below made the connection (the later ones find it).
	var connected importDevice
	created := false
	defer func() {
		if failure != nil {
			connected.fresh = created
			connected.disconnect()
		}
	}()
	p, err := gamesource.Ready(ctx, source)
	if err != nil {
		return importDevice{}, err
	}
	if importPortConflict(p.Port, m.Root.Ports().InstancePorts(), m.Root.ScriptAdbServerPort(), m.Root.AdbServerPort()) {
		return importDevice{}, errs.Newf(errs.PortConflict, "源实例端口 %d 与 ModAVD 冲突", p.Port)
	}
	// An instance can be in the vendor's running list before Android has started
	// its adb and package services. Wait for those before presenting its apps.
	deadline, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var last error
	for {
		d, err := m.connectImportDevice(deadline, p.Port)
		if d.serial != "" {
			connected = d
			created = created || d.fresh
		}
		if err == nil {
			var state string
			state, err = d.shell(deadline, "getprop sys.boot_completed; pm path --user 0 android")
			if err == nil && strings.HasPrefix(strings.ReplaceAll(strings.TrimSpace(state), "\r", ""), "1\npackage:") {
				d.fresh = created
				return d, nil
			}
		} else if errs.CodeOf(err) != errs.NotReady && deadline.Err() == nil {
			return d, err
		}
		if err != nil {
			last = err
		}
		select {
		case <-deadline.Done():
			if ctx.Err() != nil {
				return importDevice{}, ctxErr(ctx)
			}
			msg := errs.Newf(errs.Timeout, "等待 %s 实例 %d 的 Android 服务超时", source.Kind, source.Index)
			if last != nil {
				msg.WithAdvice(last.Error())
			}
			return importDevice{}, msg
		case <-time.After(2 * time.Second):
		}
	}
}

func packageList(ctx context.Context, d importDevice, flags string) ([]string, error) {
	out, err := d.shell(ctx, "pm list packages "+flags+" --user 0")
	if err != nil {
		return nil, err
	}
	pkgs := []string{}
	for _, line := range strings.Fields(out) {
		p, ok := strings.CutPrefix(line, "package:")
		if !ok || !validPackageName(p) {
			return nil, fmt.Errorf("pm list packages: %q", line)
		}
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	return pkgs, nil
}

// appRunning asks the guest, read-only, for the names of its processes (Toybox ps; NAME is argv[0], so a service of
// the app is "<package>:<service>"). The app runs when one of them is the package or starts with "<package>:". An
// answer that is not a process list (no ps -A, adb trouble) is an error rather than a guess.
func appRunning(ctx context.Context, d importDevice, pkg string) (bool, error) {
	out, err := d.shell(ctx, "ps -A -o NAME; echo exit:$?")
	if err != nil {
		return false, err
	}
	var names []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" && line != "NAME" {
			names = append(names, line)
		}
	}
	if n := len(names); n < 2 || names[n-1] != "exit:0" {
		return false, fmt.Errorf("ps: %q", tail(out, 200))
	}
	for _, name := range names {
		if name == pkg || strings.HasPrefix(name, pkg+":") {
			return true, nil
		}
	}
	return false, nil
}

// checkGamesStopped refuses to start while any of the games is running on the target (it would be writing the
// resources being merged) or on the source (its files would change during the copy). It never stops a game.
func checkGamesStopped(ctx context.Context, src, dst importDevice, games []ImportGame) error {
	for _, g := range games {
		running, err := appRunning(ctx, dst, g.Package)
		if err != nil {
			return err
		}
		if running {
			return errs.Newf(errs.PrecheckFailed, "目标实例上的 %s 正在运行，请先退出游戏再导入", g.Package)
		}
	}
	for _, g := range games {
		running, err := appRunning(ctx, src, g.Package)
		if err != nil {
			return err
		}
		if running {
			return errs.Newf(errs.PrecheckFailed, "源模拟器上的 %s 正在运行，请先退出游戏再导入", g.Package)
		}
	}
	return nil
}
func (m *Manager) SourcePackages(ctx context.Context, source gamesource.Source) ([]string, error) {
	d, err := m.gameSource(ctx, source)
	if err != nil {
		return nil, err
	}
	return packageList(ctx, d, "-3")
}

func importQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// Pin the physical directory before reading it. A link in a selected ancestor
// must not turn a public-storage selection into a read from elsewhere.
func importPublicCD(dir string) string {
	rel := strings.TrimPrefix(dir, "/sdcard/")
	return "cd -P /sdcard && base=$PWD && cd -P " + importQuote("./"+rel) + " && [ \"$PWD\" = \"$base\"/" + importQuote(rel) + " ]"
}

func resourcePath(s string) bool {
	return fs.ValidPath(s) && s != "." && !strings.Contains(s, "\\") && strings.IndexFunc(s, unicode.IsControl) < 0
}

type ResourceEntry struct {
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
}

func listResourceDir(ctx context.Context, d importDevice, base, rel string) ([]ResourceEntry, error) {
	dir := base
	if rel != "" {
		if !resourcePath(rel) {
			return nil, errs.New(errs.InvalidArgument, "请选择包内的资源路径")
		}
		dir = path.Join(base, rel)
	}
	// Only offer ordinary files and directories under the resolved public path.
	out, err := d.shell(ctx, "cd /sdcard/Android && if [ -d "+importQuote(dir)+" ]; then "+importPublicCD("/sdcard/Android/"+dir)+" && find . -mindepth 1 -maxdepth 1 \\( -type d -printf 'd%f\\0' -o -type f -printf 'f%f\\0' \\); elif [ -e "+importQuote(dir)+" ]; then exit 1; fi")
	if err != nil {
		return nil, err
	}
	entries := []ResourceEntry{}
	for _, s := range strings.Split(out, "\x00") {
		if len(s) > 1 && (s[0] == 'd' || s[0] == 'f') && resourcePath(s[1:]) {
			entries = append(entries, ResourceEntry{s[1:], s[0] == 'd'})
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

func (m *Manager) SourceFiles(ctx context.Context, source gamesource.Source, pkg, rel string) ([]ResourceEntry, error) {
	if !validPackageName(pkg) {
		return nil, errs.New(errs.InvalidArgument, "请选择第三方应用")
	}
	d, err := m.gameSource(ctx, source)
	if err != nil {
		return nil, err
	}
	pkgs, err := packageList(ctx, d, "-3")
	if err != nil {
		return nil, err
	}
	if !slices.Contains(pkgs, pkg) {
		return nil, errs.New(errs.InvalidArgument, "请选择第三方应用")
	}
	return listResourceDir(ctx, d, "data/"+pkg, rel)
}
