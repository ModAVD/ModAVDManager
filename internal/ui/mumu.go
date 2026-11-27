// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package ui

import (
	"context"
	"net/http"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/mumu"
)

func (s *server) handleMuMuList(w http.ResponseWriter, r *http.Request) {
	rows, err := mumu.List(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *server) handleMuMuBrowse(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Source  *int   `json:"source"`
		Package string `json:"package"`
		Path    string `json:"path"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Source == nil || *b.Source < 0 {
		writeError(w, errs.New(errs.InvalidArgument, "请输入 MuMu 实例编号"))
		return
	}
	var rows any
	var err error
	if b.Package == "" {
		rows, err = s.mgr.MuMuPackages(r.Context(), *b.Source)
	} else {
		rows, err = s.mgr.MuMuFiles(r.Context(), *b.Source, b.Package, b.Path)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rows)
}

func (s *server) handleMuMuImport(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var b struct {
		Source *int           `json:"source"`
		Games  []emu.MuMuGame `json:"games"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Source == nil || *b.Source < 0 || len(b.Games) == 0 {
		writeError(w, errs.New(errs.InvalidArgument, "请选择 MuMu 实例和游戏"))
		return
	}
	s.respondJob(w)(s.startJob(kindMuMuImport, i, func(ctx context.Context, j *job, m backend) (any, error) {
		return m.ImportMuMu(ctx, i, *b.Source, b.Games, s.progressFn(j))
	}))
}
