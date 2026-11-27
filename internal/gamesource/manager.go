// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
// Package gamesource discovers local emulator instances using their vendor's CLI.
package gamesource

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

type Source struct {
	Kind  string `json:"kind"`
	Index int    `json:"index"`
}

func (s Source) Valid() bool { return (s.Kind == "mumu" || s.Kind == "ldplayer") && s.Index >= 0 }

type Player struct {
	Source
	Name      string `json:"name"`
	Running   bool   `json:"running"`
	Ready     bool   `json:"ready"`
	Port      int    `json:"adb_port,omitempty"`
	Error     int    `json:"error_code,omitempty"`
	Message   string `json:"error,omitempty"` // only on the placeholder row of a player that could not be read (Index -1)
	enginePID uint32
}

// run replaces the control program in tests.
type installation struct {
	kind, exe, data string
	run             func(ctx context.Context, args ...string) ([]byte, error)
}

// mumuExe finds the control program in the first install directory that has one. Older versions keep it in shell\.
func mumuExe(dirs []string) string {
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		for _, sub := range []string{"nx_main", "shell"} {
			if exe := filepath.Join(dir, sub, "MuMuManager.exe"); isFile(exe) {
				return exe
			}
		}
	}
	return ""
}

func locate(kind string) (installation, error) {
	if runtime.GOOS != "windows" {
		return installation{}, errs.New(errs.Unsupported, "从模拟器导入游戏需要 Windows")
	}
	pf := os.Getenv("ProgramFiles")
	if kind == "mumu" {
		if exe := mumuExe(append(mumuDirs(), filepath.Join(pf, "NetEase", "MuMu Player 12"), filepath.Join(pf, "NetEase", "MuMuPlayer"))); exe != "" {
			return installation{kind: kind, exe: exe}, nil
		}
	} else if kind == "ldplayer" {
		for _, i := range ldInstallations() {
			if filepath.IsAbs(i.exe) && isFile(i.exe) {
				return i, nil
			}
		}
	} else {
		return installation{}, errs.New(errs.InvalidArgument, "模拟器类型须为 mumu 或 ldplayer")
	}
	return installation{}, errs.Newf(errs.NotFound, "找不到模拟器 %s 的控制程序", kind)
}
func isFile(name string) bool { s, err := os.Stat(name); return err == nil && s.Mode().IsRegular() }

func (i installation) command(ctx context.Context, args ...string) ([]byte, error) {
	if i.run != nil {
		return i.run(ctx, args...)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, i.exe, args...)
	cmd.Dir = filepath.Dir(i.exe)
	host.HideConsoleCmd(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if ctx.Err() == context.Canceled {
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errs.Newf(errs.Timeout, "%s 命令超时", i.kind)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", filepath.Base(i.exe), args[0], err)
	}
	return out, nil
}

// List returns the instances of every installed player. A player that cannot be read gets one placeholder row
// (Index -1, Error and Message set), so the other players are still listed.
func List(ctx context.Context) ([]Player, error) {
	var found []installation
	for _, kind := range []string{"mumu", "ldplayer"} {
		i, err := locate(kind)
		if errs.CodeOf(err) == errs.NotFound {
			continue
		}
		if err != nil {
			return nil, err
		}
		found = append(found, i)
	}
	return listAll(ctx, found)
}

func listAll(ctx context.Context, found []installation) ([]Player, error) {
	players := []Player{}
	for _, i := range found {
		list, err := i.list(ctx)
		if err != nil {
			if errs.CodeOf(err) == errs.Canceled {
				return nil, err
			}
			players = append(players, Player{Source: Source{Kind: i.kind, Index: -1}, Error: -1, Message: err.Error()})
			continue
		}
		if i.kind == "ldplayer" {
			for pos, p := range list {
				if p.Ready {
					list[pos].Port = ldPort(i.data, p)
				}
			}
		}
		players = append(players, list...)
	}
	return players, nil
}

func (i installation) list(ctx context.Context) ([]Player, error) {
	if i.kind == "ldplayer" {
		out, err := i.command(ctx, "list2")
		if err != nil {
			return nil, err
		}
		return parseLDPlayers(out)
	}
	out, err := i.command(ctx, "info", "-v", "all")
	if err != nil {
		return nil, err
	}
	return parseMuMuPlayers(out)
}
func (i installation) launch(ctx context.Context, index int) error {
	n := strconv.Itoa(index)
	if i.kind == "ldplayer" {
		_, err := i.command(ctx, "launch", "--index", n)
		return err
	}
	out, err := i.command(ctx, "control", "-v", n, "launch")
	if err != nil {
		return err
	}
	var reply struct {
		Code    *int   `json:"errcode"`
		Message string `json:"errmsg"`
	}
	if err := json.Unmarshal(out, &reply); err != nil {
		return err
	}
	if reply.Code == nil || *reply.Code != 0 {
		return errs.Newf(errs.LaunchFailed, "MuMu 实例 %d 启动失败：%s", index, reply.Message)
	}
	return nil
}

func Ready(ctx context.Context, s Source) (Player, error) {
	if !s.Valid() {
		return Player{}, errs.New(errs.InvalidArgument, "请选择源模拟器和实例")
	}
	i, err := locate(s.Kind)
	if err != nil {
		return Player{}, err
	}
	return i.ready(ctx, s)
}

func (i installation) ready(ctx context.Context, s Source) (Player, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	launched := false
	for {
		players, err := i.list(ctx)
		if err != nil {
			return Player{}, err
		}
		pos := sort.Search(len(players), func(n int) bool { return players[n].Index >= s.Index })
		if pos == len(players) || players[pos].Index != s.Index {
			return Player{}, errs.Newf(errs.NotFound, "找不到 %s 实例 %d", s.Kind, s.Index)
		}
		p := players[pos]
		if p.Error != 0 {
			return Player{}, fmt.Errorf("%s info %d: %d", s.Kind, s.Index, p.Error)
		}
		if p.Ready {
			if s.Kind == "ldplayer" {
				p.Port = ldPort(i.data, p)
			}
			if p.Port > 0 && p.Port <= 65535 {
				return p, nil
			}
		}
		if !p.Running && !launched {
			if err := i.launch(ctx, s.Index); err != nil {
				return Player{}, err
			}
			launched = true
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.DeadlineExceeded {
				return Player{}, errs.Newf(errs.Timeout, "等待 %s 实例 %d 的本机 ADB 连接超时", s.Kind, s.Index)
			}
			return Player{}, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		case <-time.After(time.Second):
		}
	}
}
