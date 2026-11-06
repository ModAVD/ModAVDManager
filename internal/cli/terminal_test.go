package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NUL (/dev/null), a pipe, a file and a reader that is no file are never a console: components fetch must not ask
// which mirror to use when stdin is one of them (`echo y | modavd components fetch`, `< NUL`).
func TestStdinNotAConsole(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	p := filepath.Join(t.TempDir(), "answers.txt")
	if err := os.WriteFile(p, []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for name, f := range map[string]*os.File{"null": null, "pipe": r, "file": file} {
		if readerIsTerminal(f) {
			t.Errorf("%s taken for a console", name)
		}
	}
	if readerIsTerminal(strings.NewReader("y\n")) || readerIsTerminal(nil) {
		t.Error("a reader that is no file taken for a console")
	}
}
