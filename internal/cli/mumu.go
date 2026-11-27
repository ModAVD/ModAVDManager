// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package cli

import (
	"fmt"
	"io"
	"strconv"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/mumu"
)

func runMuMu(c *cmdCtx) (any, error) {
	if c.inv.cmd.name == "mumu-list" {
		return mumu.List(c.ctx)
	}
	args := c.inv.pos
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 0 {
		return nil, errs.New(errs.Usage, "请输入 MuMu 实例编号")
	}
	m, err := c.mgr()
	if err != nil {
		return nil, err
	}
	switch c.inv.cmd.name {
	case "mumu-apps":
		return m.MuMuPackages(c.ctx, n)
	case "mumu-files":
		rel := ""
		if len(args) == 3 {
			rel = args[2]
		}
		return m.MuMuFiles(c.ctx, n, args[1], rel)
	}
	target, err := parseIndex(args[1])
	if err != nil {
		return nil, err
	}
	return m.ImportMuMu(c.ctx, target, n, []emu.MuMuGame{{Package: args[2], Data: c.inv.strsOpt("data")}}, func(p emu.Progress) { c.log.Infof("%s", p.Message) })
}

func humanMuMu(w io.Writer, data any) {
	switch r := data.(type) {
	case []mumu.Player:
		for _, p := range r {
			fmt.Fprintf(w, "%d\t%s\n", p.Index, p.Name)
		}
	case []string:
		for _, p := range r {
			fmt.Fprintln(w, p)
		}
	case []emu.MuMuEntry:
		for _, e := range r {
			s := e.Name
			if e.Directory {
				s += "/"
			}
			fmt.Fprintln(w, s)
		}
	case []emu.MuMuImportResult:
		for _, g := range r {
			fmt.Fprintf(w, "%s  APK %d B  资源 %d B\n", g.Package, g.APKBytes, g.ResourceBytes)
		}
	}
}
