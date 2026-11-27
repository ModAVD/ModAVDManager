// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
// Package mumu discovers local players through MuMu's own instance inventory.
package mumu

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"time"

	"modavd/internal/errs"
	"modavd/internal/host"
)

type Player struct {
	Index   int    `json:"index,string"`
	Name    string `json:"name"`
	Running bool   `json:"is_process_started"`
	Ready   bool   `json:"is_android_started"`
	Port    int    `json:"adb_port,omitempty"`
	Error   int    `json:"error_code,omitempty"`
}

func command(ctx context.Context, dest any, args ...string) error {
	if runtime.GOOS != "windows" {
		return errs.New(errs.Unsupported, "MuMu 导入需要 Windows")
	}
	dirs := append(installDirs(), filepath.Join(os.Getenv("ProgramFiles"), "NetEase", "MuMu Player 12"), filepath.Join(os.Getenv("ProgramFiles"), "NetEase", "MuMuPlayer"))
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		exe := filepath.Join(dir, "nx_main", "MuMuManager.exe")
		if _, err := os.Stat(exe); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, exe, args...)
		host.HideConsoleCmd(cmd)
		cmd.WaitDelay = 2 * time.Second
		out, err := cmd.Output()
		if ctx.Err() == context.Canceled {
			return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
		if ctx.Err() == context.DeadlineExceeded {
			return errs.Newf(errs.Timeout, "MuMuManager %s 超时", args[0])
		}
		if err != nil {
			return fmt.Errorf("MuMuManager %s: %w", args[0], err)
		}
		return json.Unmarshal(out, dest)
	}
	return errs.New(errs.NotFound, "找不到 MuMuManager.exe，请先安装 MuMu 模拟器")
}

func List(ctx context.Context) ([]Player, error) {
	var rows map[string]Player
	if err := command(ctx, &rows, "info", "-v", "all"); err != nil {
		return nil, err
	}
	result := make([]Player, 0, len(rows))
	for key, p := range rows {
		if key != strconv.Itoa(p.Index) || p.Index < 0 {
			return nil, fmt.Errorf("MuMu info %s: %d", key, p.Error)
		}
		result = append(result, p)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Index < result[j].Index })
	return result, nil
}

// Ready launches only the selected player, then waits for its live adb port.
func Ready(ctx context.Context, index int) (Player, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	launched := false
	for {
		rows, err := List(ctx)
		if err != nil {
			return Player{}, err
		}
		var p *Player
		for i := range rows {
			if rows[i].Index == index {
				p = &rows[i]
				break
			}
		}
		if p == nil {
			return Player{}, errs.Newf(errs.NotFound, "找不到 MuMu 实例 %d", index)
		}
		if p.Error != 0 {
			return Player{}, fmt.Errorf("MuMu info %d: %d", index, p.Error)
		}
		if p.Ready && p.Port > 0 && p.Port <= 65535 {
			return *p, nil
		}
		if !p.Running && !launched {
			var r struct {
				Code    *int   `json:"errcode"`
				Message string `json:"errmsg"`
			}
			if err := command(ctx, &r, "control", "-v", strconv.Itoa(index), "launch"); err != nil {
				return Player{}, err
			}
			if r.Code == nil || *r.Code != 0 {
				return Player{}, errs.Newf(errs.LaunchFailed, "MuMu 实例 %d 启动失败：%s", index, r.Message)
			}
			launched = true
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return Player{}, errs.Newf(errs.Timeout, "等待 MuMu 实例 %d 开机超时", index)
			}
			return Player{}, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		case <-time.After(time.Second):
		}
	}
}
