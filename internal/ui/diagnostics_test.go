//go:build windows

// Copyright 2026 Ji Mao Cai
package ui

import (
	"net/http"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestDiagnosticsHTTPPreviewAndBooleanPatch(t *testing.T) {
	h := newHarness(t, nil)
	var preview emu.DiagnosticsPreview
	expect(t, h.do("GET", "/api/instances/3/diagnostics/preview", nil), http.StatusOK, &preview)
	if preview.Index != 3 || preview.Files != 2 || preview.Bytes != 1024 {
		t.Fatalf("preview = %+v", preview)
	}
	h.f.mu.Lock()
	before := len(h.f.updated)
	h.f.mu.Unlock()
	if before != 0 {
		t.Fatal("preview mutated instance")
	}
	for _, enabled := range []bool{true, false} {
		var result emu.UpdateResult
		expect(t, h.do("PATCH", "/api/instances/3", map[string]any{"diagnostics": enabled}), http.StatusOK, &result)
		if result.Instance.Diagnostics != enabled {
			t.Fatalf("result = %+v", result)
		}
		h.f.mu.Lock()
		req := h.f.updated[len(h.f.updated)-1]
		h.f.mu.Unlock()
		if req.Diagnostics == nil || *req.Diagnostics != enabled {
			t.Fatalf("patch lost boolean = %+v", req)
		}
	}
	expectError(t, h.do("PATCH", "/api/instances/3", `{"diagnostics":"off"}`), http.StatusBadRequest, errs.InvalidArgument)
	expectError(t, h.do("GET", "/api/instances/5/diagnostics/preview", nil), http.StatusBadRequest, errs.InvalidArgument)
	expectError(t, h.do("GET", "/api/instances/9/diagnostics/preview", nil), http.StatusNotFound, errs.NotFound)
}
