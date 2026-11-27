//go:build windows

package ui

import (
	"context"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/gamesource"
)

func TestGameSourceRequired(t *testing.T) {
	h := newHarness(t, nil)
	expectError(t, h.do("POST", "/api/game-sources/browse", `{}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/instances/3/import-game", `{"games":[{"package":"example.game"}]}`), 400, errs.InvalidArgument)
	expectError(t, h.do("POST", "/api/game-sources/browse", `{"source":0}`), 400, errs.InvalidArgument)
	var pkgs []string
	expect(t, h.do("POST", "/api/game-sources/browse", `{"player":"mumu","source":0}`), 200, &pkgs)
	if len(pkgs) != 1 {
		t.Fatal(pkgs)
	}
	var j jobView
	expect(t, h.do("POST", "/api/instances/3/import-game", `{"player":"mumu","source":0,"games":[{"package":"example.game","data":["files/resources"]}]}`), 202, &j)
	if j.Index != 3 || j.Kind != kindGameImport {
		t.Fatal(j)
	}
	h.waitJob(j.ID, stateIs(stateDone))
}

func (fakeBackend) SourcePackages(context.Context, gamesource.Source) ([]string, error) {
	return []string{"example.game"}, nil
}

func (fakeBackend) SourceFiles(context.Context, gamesource.Source, string, string) ([]emu.ResourceEntry, error) {
	return []emu.ResourceEntry{{Name: "resources", Directory: true}}, nil
}

func (fakeBackend) ImportGames(_ context.Context, _ int, _ gamesource.Source, games []emu.ImportGame, _ func(emu.Progress)) ([]emu.GameImportResult, error) {
	return []emu.GameImportResult{{Package: games[0].Package}}, nil
}
