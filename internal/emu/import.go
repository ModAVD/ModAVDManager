//go:build windows

package emu

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
	"modavd/internal/importer"
	"modavd/internal/root"
)

type ImportRequest struct {
	Target   int      `json:"target"`
	Source   string   `json:"source"` // host:port accepted by adb connect
	Packages []string `json:"packages"`
	TempRoot string   `json:"temp_root,omitempty"`
}

type ImportResult struct {
	Target   int      `json:"target"`
	Source   string   `json:"source"`
	Packages []string `json:"packages"`
	Bytes    int64    `json:"bytes"`
}

// ImportGames connects the manager's private adb server to a source emulator,
// installs selected split APKs, then streams each package's Android/data and
// Android/obb directories directly into the target. The source is never started
// or modified by this method.
func (m *Manager) ImportGames(ctx context.Context, req ImportRequest, progress func(importer.Progress)) (*ImportResult, error) {
	if !root.ValidIndex(req.Target) {
		return nil, errs.Newf(errs.InvalidArgument, "目标实例编号 %d 不可用", req.Target)
	}
	if strings.TrimSpace(req.Source) == "" {
		return nil, errs.New(errs.InvalidArgument, "缺少源实例的 host:port")
	}
	if len(req.Packages) == 0 {
		return nil, errs.New(errs.InvalidArgument, "至少选择一个应用包名")
	}
	if req.TempRoot == "" {
		req.TempRoot = filepath.Join(m.Root.Dir, "logs")
	}
	target := importer.RootDevice(m.Root, req.Target)
	if !m.bootCompleted(ctx, req.Target, statusAdbTimeout, true) {
		return nil, errs.Newf(errs.Unsupported, "目标实例 %d 没有开机，请先开机", req.Target)
	}
	connect := m.ADB.Run(ctx, runOpts(req.Source), "connect", req.Source)
	if !connect.OK() && !strings.Contains(strings.ToLower(connect.Out), "already connected") {
		return nil, errs.Newf(errs.Internal, "连接源实例 %s 失败：%s", req.Source, connect.Out)
	}
	source := importer.Device{Client: m.ADB, Serial: req.Source}
	result := &ImportResult{Target: req.Target, Source: req.Source, Packages: append([]string{}, req.Packages...)}
	for _, pkg := range req.Packages {
		if err := importer.InstallMultiple(ctx, source, target, pkg, req.TempRoot); err != nil {
			return nil, errs.Wrap(errs.Internal, err, "安装应用失败")
		}
		var last int64
		cb := func(p importer.Progress) {
			last = p.Bytes
			if progress != nil {
				progress(p)
			}
		}
		if err := importer.CopyGameData(ctx, source, target, pkg, cb); err != nil {
			return nil, errs.Wrap(errs.Internal, err, "拷贝游戏数据失败")
		}
		result.Bytes += last
	}
	_ = m.ADB.Run(ctx, runOpts(req.Source), "disconnect", req.Source)
	return result, nil
}

func runOpts(serial string) adb.RunOptions {
	return adb.RunOptions{Serial: serial, Timeout: 30 * time.Second, Reregister: false}
}
