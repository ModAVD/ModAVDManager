//go:build !windows

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/testutil"
)

func TestWindowUnsupported(t *testing.T) {
	for _, action := range []string{"show", "hide"} {
		t.Run(action, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), []string{"--root", testutil.TempDir(t), "--json", "window", "1", action},
				strings.NewReader(""), &stdout, &stderr, defaultDeps())
			var response struct {
				OK    bool                  `json:"ok"`
				Error struct{ Code string } `json:"error"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
				t.Fatalf("output=%q, stderr=%q: %v", stdout.String(), stderr.String(), err)
			}
			if code != errs.ExitCode(errs.New(errs.Unsupported, "")) || response.OK || response.Error.Code != errs.Unsupported {
				t.Fatalf("exit=%d, output=%s, stderr=%s", code, stdout.String(), stderr.String())
			}
		})
	}
}
