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
	"modavd/internal/errs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestRangeVerificationReadErrorDoesNotDownloadWhole(t *testing.T) {
	for _, target := range []string{manifestName, "sdk/emulator/emulator.exe"} {
		t.Run(target, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			c := newRangeCase(t)
			r := writeRoot(t, 36, c.big)
			f := newSources(t, 37, c.zip)
			log := &dlLog{}
			serve := log.serve(c.zip, nil)
			u := f.updater(r)
			var mu sync.Mutex
			var held syscall.Handle
			unlock := func() {
				mu.Lock()
				defer mu.Unlock()
				if held != 0 {
					syscall.CloseHandle(held)
					held = 0
				}
			}
			defer unlock()
			f.dl = func(w http.ResponseWriter, req *http.Request) {
				if req.Header.Get("Range") == "" {
					unlock()
				}
				serve(w, req)
			}
			injected := false
			u.Opt.Progress = func(p Progress) {
				if injected || p.Stage != "verifying" {
					return
				}
				injected = true
				if _, err := os.Stat(u.l.pkgAt(target)); err != nil {
					t.Fatal(err)
				}
				name, err := syscall.UTF16PtrFromString(u.l.pkgAt(target))
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				held, err = syscall.CreateFile(name, syscall.GENERIC_READ,
					syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING,
					syscall.FILE_ATTRIBUTE_NORMAL, 0)
				mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			st, err := u.Prepare(context.Background())
			t.Logf("injected=%v error=%v code=%s mode=%s fullGets=%d notices=%v", injected, err, errs.CodeOf(err), stagedMode(u), log.fullGets(), st.Notices)
			var pathErr *os.PathError
			if !injected || !errors.As(err, &pathErr) || errs.CodeOf(err) == errs.UpdateRejected || log.fullGets() != 0 || len(st.Notices) != 0 {
				t.Errorf("local verification read failure did not stop before whole download")
			}
		})
	}
}

// entryFaultOutput exercises destination errors without changing global file operations.
type entryFaultOutput struct {
	data               bytes.Buffer
	writeErr, closeErr error
	closed             int
}

func (o *entryFaultOutput) Write(p []byte) (int, error) {
	if o.writeErr != nil {
		return 0, o.writeErr
	}
	return o.data.Write(p)
}
func (o *entryFaultOutput) Close() error { o.closed++; return o.closeErr }

type entryReadError struct{ err error }

func (r entryReadError) Read([]byte) (int, error) { return 0, r.err }

func TestEntryCloseErrorTakesPrecedenceOverBadZIP(t *testing.T) {
	disk := &os.PathError{Op: "close", Path: "output", Err: syscall.Errno(112)}
	for _, tc := range []struct {
		name                              string
		readErr, writeErr, closeErr, want error
	}{
		{"bad ZIP and close", zip.ErrChecksum, nil, disk, disk},
		{"bad ZIP alone", zip.ErrChecksum, nil, nil, zip.ErrChecksum},
		{"write error", nil, disk, nil, disk},
		{"close error", nil, nil, disk, disk},
		{"success", nil, nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := &entryFaultOutput{writeErr: tc.writeErr, closeErr: tc.closeErr}
			source := io.Reader(strings.NewReader("payload"))
			if tc.readErr != nil {
				source = io.MultiReader(source, entryReadError{tc.readErr})
			}
			err := copyEntryContents(out, source)
			if !errors.Is(err, tc.want) || out.closed != 1 {
				t.Fatalf("error %v, want %v, closes %d", err, tc.want, out.closed)
			}
		})
	}
}

func TestEmulatorVerificationReadErrorsStayLocal(t *testing.T) {
	for _, target := range []string{"source.properties", "qemu/windows-x86_64/qemu-system-x86_64-headless.exe"} {
		t.Run(target, func(t *testing.T) {
			r := writeRoot(t, 37, nil)
			path := filepath.Join(r.Dir, "sdk", "emulator", filepath.FromSlash(target))
			name, err := syscall.UTF16PtrFromString(path)
			if err != nil {
				t.Fatal(err)
			}
			held, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer syscall.CloseHandle(held)
			version, _ := ParseVersion("v0.37.0")
			err = checkEmulator(filepath.Join(r.Dir, "sdk", "emulator"), 37, version)
			var pathErr *os.PathError
			if !errors.As(err, &pathErr) || errs.CodeOf(err) == errs.UpdateRejected {
				t.Fatalf("local emulator read: %v", err)
			}
		})
	}
}

func TestStageBuildManifestReadErrorIsLocal(t *testing.T) {
	r := writeRoot(t, 36, nil)
	u := New(r, nil, Options{})
	archive := filepath.Join(r.Dir, "package.zip")
	if err := os.WriteFile(archive, zipOf(t, 37, pkgFiles(37, nil), false), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := u.l.extractPackage(archive, "", nil); err != nil {
		t.Fatal(err)
	}
	path, err := syscall.UTF16PtrFromString(u.l.pkgAt(manifestName))
	if err != nil {
		t.Fatal(err)
	}
	held, err := syscall.CreateFile(path, syscall.GENERIC_READ, syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(held)
	version, _ := ParseVersion("v0.37.0")
	err = u.l.buildStage(version)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || lexists(u.l.stageEmulator()) {
		t.Fatalf("unreadable manifest was ignored when building stage: %v", err)
	}
}
