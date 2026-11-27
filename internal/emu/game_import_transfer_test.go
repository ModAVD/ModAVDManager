// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

func TestImportTarAcceptsOnlySelectedOrdinaryFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind byte
		ok   bool
	}{
		{"./assets/music.ogg", tar.TypeReg, true}, {"assets/", tar.TypeDir, true},
		{"./assets/../config", tar.TypeReg, false}, {"other", tar.TypeReg, false},
		{"assets/link", tar.TypeSymlink, false}, {"assets/hardlink", tar.TypeLink, false},
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
