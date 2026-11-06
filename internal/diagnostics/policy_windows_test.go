//go:build windows

// Copyright 2026 Ji Mao Cai
package diagnostics

import (
	"os"
	"testing"
	"time"

	"modavd/internal/root"
)

func TestSetIsRepeatableAndPreservesMetadata(t *testing.T) {
	r := &root.Root{Dir: t.TempDir()}
	if err := os.WriteFile(r.MetadataPath(), []byte(`{"instances":{"0":{"unknown":"keep","createdAt":"created"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 12, 0, 0, 12, time.UTC)
	for _, when := range []time.Time{now, now.Add(time.Hour)} {
		if err := Set(r, 0, true, when); err != nil {
			t.Fatal(err)
		}
	}
	states, err := Read(r)
	if err != nil || states[0].Since != now.Format(time.RFC3339Nano) || !states[0].Enabled {
		t.Fatalf("repeated on = %+v, %v", states, err)
	}
	for range 2 {
		if err := Set(r, 0, false, now); err != nil {
			t.Fatal(err)
		}
	}
	states, err = Read(r)
	if err != nil || states[0].Enabled || states[0].Since != "" || states[0].CreatedAt != "created" {
		t.Fatalf("repeated off = %+v, %v", states, err)
	}
	if text(r.InstanceMeta(0).Get("unknown")) != "keep" {
		t.Fatal("set discarded another writer's unknown key")
	}
}
