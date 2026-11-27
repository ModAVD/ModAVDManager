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

func (m *Manager) gameSource(ctx context.Context, source gamesource.Source) (importDevice, error) {
	p, err := gamesource.Ready(ctx, source)
	if err != nil {
		return importDevice{}, err
	}
	if m.Root.Ports().InstancePorts()[p.Port] || p.Port == 5037 || p.Port == m.Root.ScriptAdbServerPort() || p.Port == m.Root.AdbServerPort() {
		return importDevice{}, errs.Newf(errs.PortConflict, "源实例端口 %d 与 ModAVD 冲突", p.Port)
	}
	// An instance can be in the vendor's running list before Android has started
	// its adb and package services. Wait for those before presenting its apps.
	deadline, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var last error
	for {
		d, err := m.connectImportDevice(deadline, p.Port)
		if err == nil {
			var state string
			state, err = d.shell(deadline, "getprop sys.boot_completed; pm path --user 0 android")
			if err == nil && strings.HasPrefix(strings.ReplaceAll(strings.TrimSpace(state), "\r", ""), "1\npackage:") {
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
