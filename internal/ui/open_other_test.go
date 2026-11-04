//go:build !windows

package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
)

func TestOpenUnsupportedBeforeSideEffects(t *testing.T) {
	for _, body := range []string{`{"what":"root"}`, `{"what":"logs"}`, `{"what":"instance","index":1}`} {
		t.Run(body, func(t *testing.T) {
			r, err := root.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			opened := false
			s := &server{root: r, openPath: func(string) error { opened = true; return nil }}
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/open", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			s.handleOpen(w, req)
			var response struct{ Error errorBody }
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != http.StatusUnprocessableEntity || response.Error.Code != errs.Unsupported {
				t.Errorf("status=%d, response=%s", w.Code, w.Body.String())
			}
			if opened {
				t.Error("unsupported request reached directory opener")
			}
			entries, err := os.ReadDir(r.Dir)
			if err != nil || len(entries) != 0 {
				t.Errorf("root changed: entries=%v, error=%v", entries, err)
			}
		})
	}
}

func TestShellOpenUnsupported(t *testing.T) {
	if err := shellOpen(t.TempDir()); errs.CodeOf(err) != errs.Unsupported {
		t.Fatalf("shellOpen: %v, want unsupported", err)
	}
}
