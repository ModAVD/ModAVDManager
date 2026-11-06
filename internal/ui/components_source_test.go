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
	// no custom address is an empty list, not null
	for _, req := range [][2]string{{"GET", ""}, {"PUT", `{"source":"google"}`}} {
		var raw map[string]any
		var body any
		if req[1] != "" {
			body = req[1]
		}
		expect(t, h.do(req[0], "/api/components/source", body), http.StatusOK, &raw)
		if urls, ok := raw["mirror_urls"].([]any); !ok || len(urls) != 0 {
			t.Fatalf("%s mirror_urls = %#v", req[0], raw["mirror_urls"])
		}
	}
	expect(t, h.do("PUT", "/api/components/source", `{"source":"custom","mirror_urls":["https://mirror.example/sdk"]}`), http.StatusOK, &got)
	if got.Source != "custom" || len(got.MirrorURLs) != 1 || got.MirrorURLs[0] != "https://mirror.example/sdk/" {
		t.Fatalf("custom source: %+v", got)
	}
	expectError(t, h.do("PUT", "/api/components/source", `{"source":"custom"}`), http.StatusBadRequest, "usage")
	expectError(t, h.do("PUT", "/api/components/source", `{"source":"ftp"}`), http.StatusBadRequest, "invalid_argument")
}
