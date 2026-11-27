// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package ui

import (
	"context"
	"net/http"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/gamesource"
)

func (s *server) handleGameList(w http.ResponseWriter, r *http.Request) {
	rows, err := gamesource.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

// Keep the source index optional on the wire: zero is a real instance.
type sourceInput struct {
	Player string `json:"player"`
	Index  *int   `json:"source"`
}

func (in sourceInput) selection() (gamesource.Source, bool) {
	if in.Index == nil {
		return gamesource.Source{}, false
	}
	source := gamesource.Source{Kind: in.Player, Index: *in.Index}
	return source, source.Valid()
}

func (s *server) handleGameBrowse(w http.ResponseWriter, r *http.Request) {
	var b struct {
		sourceInput
		Package string `json:"package"`
		Path    string `json:"path"`
	}
	if !decode(w, r, &b) {
		return
	}
	source, valid := b.selection()
	if !valid {
		writeError(w, errs.New(errs.InvalidArgument, "请选择源模拟器和实例"))
		return
	}
	var rows any
	var err error
	if b.Package == "" {
		rows, err = s.mgr.SourcePackages(r.Context(), source)
	} else {
		rows, err = s.mgr.SourceFiles(r.Context(), source, b.Package, b.Path)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *server) handleGameImport(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var b struct {
		sourceInput
		Games []emu.ImportGame `json:"games"`
	}
	if !decode(w, r, &b) {
		return
	}
	source, valid := b.selection()
	if !valid || len(b.Games) == 0 {
		writeError(w, errs.New(errs.InvalidArgument, "请选择源实例和游戏"))
		return
	}
	s.respondJob(w)(s.startJob(kindGameImport, i, func(ctx context.Context, j *job, m backend) (any, error) {
		return m.ImportGames(ctx, i, source, b.Games, s.progressFn(j))
	}))
}
