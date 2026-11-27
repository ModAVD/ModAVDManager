//go:build windows

package cli

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/gamesource"
)

func (*fakeManager) SourcePackages(context.Context, gamesource.Source) ([]string, error) {
	return []string{"example.game"}, nil
}

func (*fakeManager) SourceFiles(context.Context, gamesource.Source, string, string) ([]emu.ResourceEntry, error) {
	return []emu.ResourceEntry{{Name: "resources", Directory: true}}, nil
}

func TestImportGamesArguments(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "import-game", "ldplayer", "2", "mas_3", "example.game", "--data", "files/one", "--data", "files/two", "--json")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	if ok := envelope(t, r.stdout)["ok"]; ok != true {
		t.Fatal(r.stdout)
	}
}

func (*fakeManager) ImportGames(_ context.Context, target int, source gamesource.Source, games []emu.ImportGame, _ func(emu.Progress)) ([]emu.GameImportResult, error) {
	if target != 3 || source != (gamesource.Source{Kind: "ldplayer", Index: 2}) || len(games) != 1 || games[0].Package != "example.game" || !slices.Equal(games[0].Data, []string{"files/one", "files/two"}) {
		return nil, fmt.Errorf("unexpected request %v %d %v", source, target, games)
	}
	return []emu.GameImportResult{{Package: games[0].Package}}, nil
}
