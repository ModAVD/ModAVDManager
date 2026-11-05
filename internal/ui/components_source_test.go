//go:build windows

package ui

import (
	"net/http"
	"testing"
)

func TestComponentSourceAPI(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	h := newHarness(t, nil)
	var got componentSourceView
	expect(t, h.do("GET", "/api/components/source", nil), http.StatusOK, &got)
	if got.Source != "auto" || got.Configured {
		t.Fatalf("initial source: %+v", got)
	}
	expect(t, h.do("PUT", "/api/components/source", `{"source":"custom","mirror_urls":["https://mirror.example/sdk"]}`), http.StatusOK, &got)
	if got.Source != "custom" || len(got.MirrorURLs) != 1 || got.MirrorURLs[0] != "https://mirror.example/sdk/" {
		t.Fatalf("custom source: %+v", got)
	}
	expectError(t, h.do("PUT", "/api/components/source", `{"source":"custom"}`), http.StatusBadRequest, "usage")
	expectError(t, h.do("PUT", "/api/components/source", `{"source":"ftp"}`), http.StatusBadRequest, "invalid_argument")
}
