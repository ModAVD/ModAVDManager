package guest

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"modavd/internal/testutil"
)

// TestProxyDaemon: a build.ps1 build carries a linux ELF; any other has none, and says so.
func TestProxyDaemon(t *testing.T) {
	b, err := ProxyDaemon()
	if errors.Is(err, ErrNoProxyDaemon) {
		if _, err := LocalProxyDaemon(testutil.TempDir(t)); !errors.Is(err, ErrNoProxyDaemon) {
			t.Errorf("LocalProxyDaemon: %v", err)
		}
		t.Skip("built without build.ps1: no daemon embedded")
	}
	if err != nil || !bytes.HasPrefix(b, []byte("\x7fELF\x02\x01")) {
		t.Fatalf("not a 64-bit little-endian ELF: %v", err)
	}
	p, err := LocalProxyDaemon(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); !bytes.Equal(got, b) {
		t.Error("host copy differs")
	}
}
