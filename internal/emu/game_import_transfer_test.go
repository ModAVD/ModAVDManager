// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	"modavd/internal/logx"
)

func TestImportTarAcceptsOnlySelectedOrdinaryFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		ok   bool
	}{
		{"./assets/music.ogg", tar.TypeReg, true}, {"assets/", tar.TypeDir, true},
		{"./assets/../config", tar.TypeReg, false}, {"other", tar.TypeReg, false},
		{"assets/hardlink", tar.TypeLink, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := tar.NewWriter(&buf)
			if err := w.WriteHeader(&tar.Header{Name: tc.name, Typeflag: tc.kind, Mode: 0777}); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			err := copyImportTar(io.Discard, &buf, "assets", &importProgress{})
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// ModAVD/ModAVD#274: links and special files are left out (importSize does not count them either), the rest is copied.
func TestImportTarSkipsLinksAndSpecialFiles(t *testing.T) {
	var in bytes.Buffer
	w := tar.NewWriter(&in)
	for _, h := range []tar.Header{
		{Name: "./assets/", Typeflag: tar.TypeDir, Mode: 0755},
		{Name: "./assets/link", Typeflag: tar.TypeSymlink, Linkname: "../other", Mode: 0777},
		{Name: "./assets/music.ogg", Typeflag: tar.TypeReg, Size: 5, Mode: 0644},
		{Name: "./assets/pipe", Typeflag: tar.TypeFifo, Mode: 0644},
		{Name: "./assets/tty", Typeflag: tar.TypeChar, Mode: 0644},
		{Name: "./assets/disk", Typeflag: tar.TypeBlock, Mode: 0644},
	} {
		if err := w.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := w.Write([]byte("sound")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	p := &importProgress{}
	if err := copyImportTar(&out, &in, "assets", p); err != nil {
		t.Fatal(err)
	}
	var names []string
	r := tar.NewReader(&out)
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if want := []string{"assets", "assets/music.ogg"}; !slices.Equal(names, want) {
		t.Fatalf("copied %q, want %q", names, want)
	}
	if p.done != 5 || p.skipped != 4 || p.skippedExample != "assets/link" {
		t.Fatalf("done=%d skipped=%d example=%q", p.done, p.skipped, p.skippedExample)
	}
	// Only links and special files: nothing of the item to publish.
	var only bytes.Buffer
	w = tar.NewWriter(&only)
	if err := w.WriteHeader(&tar.Header{Name: "assets", Typeflag: tar.TypeSymlink, Linkname: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := copyImportTar(io.Discard, &only, "assets", &importProgress{}); err == nil {
		t.Fatal("copied a selection that is itself a link")
	}
}

// ModAVD/ModAVD#274: the skipped entries are logged once per item and the item is still published.
func TestCopyResourceLogsSkippedLinks(t *testing.T) {
	saved := importStreamFunc
	t.Cleanup(func() { importStreamFunc = saved })
	importStreamFunc = func(_ context.Context, _, _ importDevice, _, _ string, copyData func(io.Writer, io.Reader) error) error {
		var buf bytes.Buffer
		w := tar.NewWriter(&buf)
		w.WriteHeader(&tar.Header{Name: "assets/link", Typeflag: tar.TypeSymlink, Linkname: "x"})
		w.WriteHeader(&tar.Header{Name: "assets/a", Size: 10, Mode: 0660, Typeflag: tar.TypeReg})
		w.Write(make([]byte, 10))
		w.Close()
		return copyData(io.Discard, &buf)
	}
	var logged []string
	g := &scriptedGuest{m: &Manager{Log: logx.New(func(_ logx.Level, msg string) { logged = append(logged, msg) })}}
	dev := g.device()
	p := &importProgress{skipped: 7, skippedExample: "stale"}
	if err := copyResource(context.Background(), dev, dev, "/sdcard/Android/data/example.game/files/assets", 10, p); err != nil {
		t.Fatal(err)
	}
	if !g.ran("publish()") || len(logged) != 1 || !strings.Contains(logged[0], "1 个符号链接或特殊文件") || !strings.Contains(logged[0], "assets/link") {
		t.Fatalf("logged=%q scripts=%q", logged, g.scripts)
	}
}
