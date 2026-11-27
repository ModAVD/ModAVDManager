//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package ui

import (
	"net/http"
	"testing"

	"modavd/internal/emu"
)

func TestRegionalHTTP(t *testing.T) {
	h := newHarness(t, nil)
	var result emu.RegionalResult
	expect(t, h.do("GET", "/api/instances/3/regional", nil), http.StatusOK, &result)
	if !result.Supported || result.Config.Language != "preserve" || result.Guest != nil {
		t.Fatalf("regional: %+v", result)
	}
	expect(t, h.do("PATCH", "/api/instances/3", `{"language":"host","time_zone":"Etc/UTC"}`), http.StatusOK, nil)
	h.f.mu.Lock()
	req := h.f.updated[len(h.f.updated)-1]
	h.f.mu.Unlock()
	if req.Language == nil || *req.Language != "host" || req.TimeZone == nil || *req.TimeZone != "Etc/UTC" {
		t.Fatalf("lost regional fields: %+v", req)
	}
}
