//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type rangeEOFReader struct {
	io.Reader
	onEOF func()
}

func (r *rangeEOFReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF && r.onEOF != nil {
		r.onEOF()
		r.onEOF = nil
	}
	return n, err
}
func (*rangeEOFReader) Close() error { return nil }

func TestRangeLargeGroupCleanupErrorsStopFallback(t *testing.T) {
	// Store the fixture so the range exceeds 8 MiB without spending time compressing test padding.
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "payload.bin", Method: zip.Store})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(w, bytes.NewReader(bytes.Repeat([]byte{7}, 9<<20)), 9<<20); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"close", "remove"} {
		t.Run(failure, func(t *testing.T) {
			r := writeRoot(t, 36, nil)
			f := newSources(t, 37, archive.Bytes())
			u := f.updater(r)
			if err := os.MkdirAll(u.l.pkg(), 0o755); err != nil {
				t.Fatal(err)
			}
			zr, err := zip.NewReader(bytes.NewReader(archive.Bytes()), int64(archive.Len()))
			if err != nil {
				t.Fatal(err)
			}
			remote := &remoteZip{size: int64(archive.Len())}
			var held syscall.Handle
			defer func() {
				if held != 0 {
					syscall.CloseHandle(held)
				}
			}()
			injected := false
			zr.RegisterDecompressor(zip.Store, func(input io.Reader) io.ReadCloser {
				return &rangeEOFReader{Reader: input, onEOF: func() {
					// Inject the OS failure after extraction has read every byte, so only cleanup can report it.
					for _, s := range remote.segs {
						file, ok := s.reader.(*os.File)
						if !ok {
							continue
						}
						injected = true
						if failure == "close" {
							if err := file.Close(); err != nil {
								t.Fatal(err)
							}
						} else {
							name, err := syscall.UTF16PtrFromString(file.Name())
							if err != nil {
								t.Fatal(err)
							}
							held, err = syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE,
								nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
							if err != nil {
								t.Fatal(err)
							}
						}
					}
				}}
			})
			g := rangeGroup{length: int64(archive.Len()), members: []rangeEntry{{zipEntry: zipEntry{rel: "payload.bin", f: zr.File[0]}}}}
			rel, err := u.githubLatest(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			err = u.stageRangeGroup(context.Background(), rel, g, remote)
			var ru *rangeUnavailable
			var pathErr *os.PathError
			if !injected || err == nil || errors.As(err, &ru) || !errors.As(err, &pathErr) || len(remote.segs) != 0 {
				t.Fatalf("cleanup %s: injected %v, error %v, remaining segments %d", failure, injected, err, len(remote.segs))
			}
			if failure == "close" && !errors.Is(err, os.ErrClosed) {
				t.Fatalf("want close error, got %v", err)
			}
			if held != 0 {
				syscall.CloseHandle(held)
				held = 0
			}
			temps, err := filepath.Glob(filepath.Join(u.l.pkg(), ".range-*"))
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range temps {
				if err := os.Remove(name); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestRangeLargeGroupCreateErrorIsLocal(t *testing.T) {
	r := writeRoot(t, 36, nil)
	u := New(r, nil, Options{})
	if err := os.MkdirAll(u.l.dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(u.l.pkg(), []byte("blocks creation"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := u.stageRangeGroup(context.Background(), &release{}, rangeGroup{length: rangeMaxGroup + 1}, &remoteZip{})
	var ru *rangeUnavailable
	var pathErr *os.PathError
	if err == nil || errors.As(err, &ru) || !errors.As(err, &pathErr) {
		t.Fatalf("create error: %v", err)
	}
}
