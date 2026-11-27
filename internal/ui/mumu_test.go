//go:build windows

package ui

import (
	"context"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestMuMuSourceRequired(t *testing.T) {
	h := newHarness(t, nil)
	expectError(t, h.do("POST", "/api/mumu/browse", `{}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances/3/import-mumu", `{"games":[{"package":"example.game"}]}`), 400, errs.InvalidArgument)
	var pkgs []string
	expect(t, h.do("POST", "/api/mumu/browse", `{"source":0}`), 200, &pkgs)
	if len(pkgs) != 1 {
		t.Fatal(pkgs)
	}
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/import-mumu", `{"source":0,"games":[{"package":"example.game","data":["files/resources"]}]}`), 202, &j)
	if j.Index != 3 || j.Kind != kindMuMuImport {
		t.Fatal(j)
	}
	h.waitJob(j.ID, stateIs(stateDone))
}

func (fakeBackend) MuMuPackages(context.Context, int) ([]string, error) {
	return []string{"example.game"}, nil
}

func (fakeBackend) MuMuFiles(context.Context, int, string, string) ([]emu.MuMuEntry, error) {
	return []emu.MuMuEntry{{Name: "resources", Directory: true}}, nil
}

func (fakeBackend) ImportMuMu(_ context.Context, _, _ int, games []emu.MuMuGame, _ func(emu.Progress)) ([]emu.MuMuImportResult, error) {
	return []emu.MuMuImportResult{{Package: games[0].Package}}, nil
}
