// Copyright 2026 Ji Mao Cai
package diagnostics

import (
	"os"
	"path/filepath"
	"testing"

	"modavd/internal/jsonord"
	"modavd/internal/root"
)

func TestReadRejectsCorruptPolicy(t *testing.T) {
	r := &root.Root{Dir: t.TempDir()}
	for _, raw := range []string{`{`, `[]`, `{"instances":{"0":{"diagnostics":"true"}}}`, `{"instances":{"0":{"diagnostics":true,"diagnosticsSince":"bad"}}}`} {
		if err := os.WriteFile(filepath.Join(r.Dir, root.MetadataFile), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(r); err == nil {
			t.Fatalf("accepted unsafe deletion policy: %s", raw)
		}
	}
}

func TestParseInstancePolicy(t *testing.T) {
	v, err := jsonord.Parse([]byte(`{"instances":{"0":{"diagnostics":true,"diagnosticsSince":"2026-11-05T12:00:00Z","createdAt":"2026-11-01T08:00:00","unknown":"keep"},"1":{},"3":{"diagnostics":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	states, err := Parse(v.Obj())
	if err != nil {
		t.Fatal(err)
	}
	if !states[0].Enabled || states[0].Since != "2026-11-05T12:00:00Z" || states[0].CreatedAt != "2026-11-01T08:00:00" || states[1].Enabled || !states[3].Enabled {
		t.Fatalf("states = %+v", states)
	}
	if text(v.Obj().Get("instances").Obj().Get("0").Obj().Get("unknown")) != "keep" {
		t.Fatal("parser changed unrelated metadata")
	}
}
