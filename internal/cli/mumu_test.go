//go:build windows

package cli

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"modavd/internal/emu"
)

func (*fakeManager) MuMuPackages(context.Context, int) ([]string, error) {
	return []string{"example.game"}, nil
}

func (*fakeManager) MuMuFiles(context.Context, int, string, string) ([]emu.MuMuEntry, error) {
	return []emu.MuMuEntry{{Name: "resources", Directory: true}}, nil
}

func TestImportMuMuArguments(t *testing.T) {
	h := newHarness()
	r := h.run(t, nil, "import-mumu", "2", "mas_3", "example.game", "--data", "files/one", "--data", "files/two", "--json")
	if r.code != 0 {
		t.Fatal(r.stderr)
	}
	if ok := envelope(t, r.stdout)["ok"]; ok != true {
		t.Fatal(r.stdout)
	}
}

func (*fakeManager) ImportMuMu(_ context.Context, target, source int, games []emu.MuMuGame, _ func(emu.Progress)) ([]emu.MuMuImportResult, error) {
	if target != 3 || source != 2 || len(games) != 1 || games[0].Package != "example.game" || !slices.Equal(games[0].Data, []string{"files/one", "files/two"}) {
		return nil, fmt.Errorf("unexpected request %d %d %v", source, target, games)
	}
	return []emu.MuMuImportResult{{Package: games[0].Package}}, nil
}
