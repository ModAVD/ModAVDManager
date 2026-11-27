// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"context"
	"fmt"
	"io"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/root"
)

type MuMuGame struct {
	Package string   `json:"package"`
	Data    []string `json:"data,omitempty"`
}
type MuMuImportResult struct {
	Package       string `json:"package"`
	APKBytes      int64  `json:"apk_bytes"`
	ResourceBytes int64  `json:"resource_bytes"`
}

func apkPaths(ctx context.Context, d importDevice, pkg string) ([]string, error) {
	out, err := d.shell(ctx, "pm path --user 0 "+pkg)
	if err != nil {
		return nil, err
	}
	files := []string{}
	names := map[string]bool{}
	for _, line := range strings.Fields(out) {
		f, ok := strings.CutPrefix(line, "package:")
		name := path.Base(f)
		if !ok || !strings.HasPrefix(f, "/data/app/") || !resourcePath(strings.TrimPrefix(f, "/")) || !strings.HasSuffix(name, ".apk") || names[name] {
			return nil, fmt.Errorf("pm path: %q", line)
		}
		names[name] = true
		files = append(files, f)
	}
	if !names["base.apk"] {
		return nil, fmt.Errorf("pm path %s: base.apk", pkg)
	}
	sort.Slice(files, func(i, j int) bool { return path.Base(files[i]) < path.Base(files[j]) })
	return files, nil
}

func apkHashes(ctx context.Context, d importDevice, files []string) ([]string, error) {
	var args []string
	for _, f := range files {
		args = append(args, importQuote(f))
	}
	out, err := d.shell(ctx, "sha256sum "+strings.Join(args, " "))
	if err != nil {
		return nil, err
	}
	var sums []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || len(f[0]) != 64 {
			return nil, fmt.Errorf("sha256sum: %q", line)
		}
		sums = append(sums, f[0])
	}
	if len(sums) != len(files) {
		return nil, io.ErrUnexpectedEOF
	}
	sort.Strings(sums)
	return sums, nil
}

func copyAPKs(ctx context.Context, src, dst importDevice, pkg string, files []string, p *importProgress) error {
	dir, cleanup, err := dst.stage(ctx, "/data/local/tmp")
	if err != nil {
		return err
	}
	defer cleanup()
	args := []string{"pm install -R --user 0 --pkg", pkg}
	for _, f := range files {
		target := importQuote(path.Join(dir, path.Base(f)))
		err := importStream(ctx, src, dst, "cat "+importQuote(f), "cat > "+target, func(w io.Writer, r io.Reader) error { _, err := io.Copy(io.MultiWriter(w, p), r); return err })
		if err != nil {
			return err
		}
		args = append(args, target)
	}
	out, err := dst.shell(ctx, "chmod 755 "+importQuote(dir)+" && "+strings.Join(args, " "))
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "Success" {
		return fmt.Errorf("pm install: %s", tail(out, 400))
	}
	return nil
}

func resourcePaths(ctx context.Context, src importDevice, g MuMuGame) ([]string, error) {
	var files []string
	for i, s := range g.Data {
		if !resourcePath(s) {
			return nil, errs.New(errs.InvalidArgument, "请选择包内的资源路径")
		}
		for _, v := range g.Data[:i] {
			if v == s || strings.HasPrefix(v, s+"/") || strings.HasPrefix(s, v+"/") {
				return nil, errs.New(errs.InvalidArgument, "资源路径重复或相互包含")
			}
		}
		files = append(files, "/sdcard/Android/data/"+g.Package+"/"+s)
	}
	base := "obb/" + g.Package
	obbs, err := listResourceDir(ctx, src, base, "")
	if err != nil {
		return nil, err
	}
	for _, e := range obbs {
		if !e.Directory && strings.HasSuffix(strings.ToLower(e.Name), ".obb") {
			files = append(files, "/sdcard/Android/"+base+"/"+e.Name)
		}
	}
	return files, nil
}

func importSize(ctx context.Context, d importDevice, files []string) (int64, error) {
	if len(files) == 0 {
		return 0, nil
	}
	var args []string
	for _, f := range files {
		args = append(args, importQuote(f))
	}
	out, err := d.shell(ctx, "set -o pipefail; find "+strings.Join(args, " ")+" -type f -printf '%s\\n' | awk '{s+=$1} END {printf \"%.0f\",s}'")
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(out), 10, 64)
}

func checkImportSpace(ctx context.Context, dst importDevice, need int64) error {
	out, err := dst.shell(ctx, "df -Pk /data")
	if err != nil {
		return err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) < 6 {
		return fmt.Errorf("df: %q", out)
	}
	kb, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil {
		return err
	}
	free, err := host.DiskFree(dst.m.Root.AvdHome())
	if err != nil {
		return err
	}
	if kb < 0 || need > kb*1024 || uint64(need) > free {
		return errs.Newf(errs.PrecheckFailed, "导入需要 %d MiB，请检查目标数据盘和电脑磁盘的剩余空间", (need+(1<<20)-1)>>20)
	}
	return nil
}

func importGame(ctx context.Context, src, dst importDevice, g MuMuGame, progress func(Progress)) (MuMuImportResult, error) {
	result := MuMuImportResult{Package: g.Package}
	apks, err := apkPaths(ctx, src, g.Package)
	if err != nil {
		return result, err
	}
	resources, err := resourcePaths(ctx, src, g)
	if err != nil {
		return result, err
	}
	installed, err := packageList(ctx, dst, "-u")
	if err != nil {
		return result, err
	}
	reuse := slices.Contains(installed, g.Package)
	if reuse {
		existing, err := apkPaths(ctx, dst, g.Package)
		if err != nil {
			return result, err
		}
		a, err := apkHashes(ctx, src, apks)
		if err != nil {
			return result, err
		}
		b, err := apkHashes(ctx, dst, existing)
		if err != nil {
			return result, err
		}
		if !slices.Equal(a, b) {
			return result, errs.Newf(errs.AlreadyExists, "目标已有不同的 %s 安装包，请选另一台实例", g.Package)
		}
	} else {
		result.APKBytes, err = importSize(ctx, src, apks)
		if err != nil {
			return result, err
		}
	}
	result.ResourceBytes, err = importSize(ctx, src, resources)
	if err != nil {
		return result, err
	}
	// pm holds its own copy of the APKs while installing them. Allow space for small files and dex optimization.
	if err := checkImportSpace(ctx, dst, 2*result.APKBytes+result.ResourceBytes+(64<<20)); err != nil {
		return result, err
	}
	p := importProgress{pkg: g.Package, total: result.APKBytes + result.ResourceBytes, report: progress}
	p.update()
	if !reuse {
		if err := copyAPKs(ctx, src, dst, g.Package, apks, &p); err != nil {
			return result, err
		}
	}
	if len(resources) > 0 {
		out, err := dst.shell(ctx, "pm list packages -U --user 0 "+g.Package)
		if err != nil {
			return result, err
		}
		for line := range strings.SplitSeq(out, "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "package:"+g.Package {
				uid, e := strconv.Atoi(strings.TrimPrefix(f[1], "uid:"))
				if e == nil && uid >= 10000 && uid < 100000 {
					dst.identity = fmt.Sprintf("%d,%d,9997", uid, uid) // AID_EVERYBODY can traverse /storage.
				}
			}
		}
		if dst.identity == "" {
			return result, errs.Newf(errs.NotReady, "无法确认 %s 的应用 UID", g.Package)
		}
	}
	for _, f := range resources {
		if err := copyResource(ctx, src, dst, f, &p); err != nil {
			return result, errs.Newf(errs.CodeOf(err), "导入 %s 失败：%v", f, err)
		}
	}
	if p.done != p.total {
		return result, errs.New(errs.Internal, "源文件大小在导入期间发生变化，请退出游戏后重试")
	}
	p.update()
	return result, nil
}

func (m *Manager) ImportMuMu(ctx context.Context, target, source int, games []MuMuGame, progress func(Progress)) ([]MuMuImportResult, error) {
	runtime, err := m.Root.SnapshotRuntime()
	if err != nil {
		return nil, err
	}
	m = m.withRuntime(runtime)
	if _, err := m.instance(target); err != nil {
		return nil, err
	}
	if len(games) == 0 {
		return nil, errs.New(errs.InvalidArgument, "请选择第三方应用")
	}
	lock, err := m.lockGuestForInstall(ctx, target)
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	// A stop can interrupt an import. The pinned transport then fails, and
	// a retry compares installed APKs and already published resources.
	_, running, err := m.qemuOf(target)
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, errs.New(errs.NotReady, "请先启动目标 ModAVD 实例")
	}
	dst, err := m.connectImportDevice(ctx, m.Root.Ports().Adb(target))
	if err != nil {
		return nil, err
	}
	id, err := dst.shell(ctx, "getprop ro.boot.qemu.avd_name; getprop sys.boot_completed")
	if err != nil {
		return nil, err
	}
	if !slices.Equal(strings.Fields(id), []string{root.AvdName(target), "1"}) {
		return nil, errs.New(errs.NotReady, "目标实例未就绪或设备编号不符")
	}
	src, err := m.mumuSource(ctx, source)
	if err != nil {
		return nil, err
	}
	pkgs, err := packageList(ctx, src, "-3")
	if err != nil {
		return nil, err
	}
	for _, g := range games {
		if !slices.Contains(pkgs, g.Package) {
			return nil, errs.New(errs.InvalidArgument, "请选择第三方应用")
		}
	}
	results := make([]MuMuImportResult, 0, len(games))
	for _, g := range games {
		r, err := importGame(ctx, src, dst, g, progress)
		if e := ctxErr(ctx); e != nil {
			return results, e
		}
		if err != nil {
			return results, err
		}
		m.Log.Infof("已导入 %s", g.Package)
		results = append(results, r)
	}
	return results, nil
}
