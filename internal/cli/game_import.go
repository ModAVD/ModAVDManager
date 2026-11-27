// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package cli

import (
	"fmt"
	"io"
	"strconv"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/gamesource"
)

func runGameImport(c *cmdCtx) (any, error) {
	if c.inv.cmd.name == "game-sources" {
		return gamesource.List(c.ctx)
	}
	args := c.inv.pos
	n, err := strconv.Atoi(args[1])
	source := gamesource.Source{Kind: args[0], Index: n}
	if err != nil || !source.Valid() {
		return nil, errs.New(errs.Usage, "请输入源模拟器类型（mumu 或 ldplayer）和编号")
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	switch c.inv.cmd.name {
	case "game-apps":
		return m.SourcePackages(c.ctx, source)
	case "game-files":
		rel := ""
		if len(args) == 4 {
			rel = args[3]
		}
		return m.SourceFiles(c.ctx, source, args[2], rel)
	}
	target, err := parseIndex(args[2])
	if err != nil {
		return nil, err
	}
	return m.ImportGames(c.ctx, target, source, []emu.ImportGame{{Package: args[3], Data: c.inv.strsOpt("data")}}, func(p emu.Progress) { c.log.Infof("%s", p.Message) })
}

func humanGameImport(w io.Writer, data any) {
	switch r := data.(type) {
	case []gamesource.Player:
		for _, p := range r {
			if p.Message != "" {
				fmt.Fprintf(w, "%s\t-\t%s\n", p.Kind, p.Message)
				continue
			}
			fmt.Fprintf(w, "%s\t%d\t%s\n", p.Kind, p.Index, p.Name)
		}
	case []string:
		for _, p := range r {
			fmt.Fprintln(w, p)
		}
	case []emu.ResourceEntry:
		for _, e := range r {
			s := e.Name
			if e.Directory {
				s += "/"
			}
			fmt.Fprintln(w, s)
		}
	case []emu.GameImportResult:
		for _, g := range r {
			fmt.Fprintf(w, "%s  APK %d B  资源 %d B\n", g.Package, g.APKBytes, g.ResourceBytes)
		}
	}
}
