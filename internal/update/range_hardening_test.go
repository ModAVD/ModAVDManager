//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"modavd/internal/errs"
)

func TestRangeManifestAlwaysRemote(t *testing.T) {
	c := newRangeCase(t)
	manifest := manifestFor(37, c.newFiles)
	forged := forgeCRC(bytes.Repeat([]byte("x"), len(manifest)), crc32.ChecksumIEEE(manifest))
	if bytes.Equal(forged, manifest) || crc32.ChecksumIEEE(forged) != crc32.ChecksumIEEE(manifest) {
		t.Fatal("invalid collision fixture")
	}
	r := writeRoot(t, 36, c.big)
	f := newSources(t, 37, c.zip)
	u := f.updater(r)
	if err := os.WriteFile(u.l.at(manifestName), forged, 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := u.githubLatest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = u.stageByRange(context.Background(), rel); err != nil {
		t.Fatalf("remote manifest should allow staging despite the local CRC collision: %v", err)
	}
	got, err := os.ReadFile(u.l.pkgAt(manifestName))
	if err != nil || !bytes.Equal(got, manifest) {
		t.Fatalf("manifest was not fetched from the package: %v", err)
	}
}

func TestRangeThresholdDoesNotAddNotice(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	c := newRangeCase(t)
	r := writeRoot(t, 36, nil)
	f := newSources(t, 37, c.zip)
	log := &dlLog{}
	f.dl = log.serve(c.zip, nil)
	u := f.updater(r)
	st, err := u.Prepare(context.Background())
	if err != nil || st.Phase != phaseStaged || stagedMode(u) != "full" || len(st.Notices) != 0 || log.fullGets() != 1 {
		t.Fatalf("normal full-package fallback: status %+v, error %v, full GETs %d", st, err, log.fullGets())
	}
	st, err = u.Install(context.Background())
	if err != nil || !st.Updated || strings.Contains(st.Message, "改下整包") {
		t.Fatalf("normal fallback in final message: %+v %v", st, err)
	}
}

func TestRangeLocalExtractionErrorDoesNotDownloadWhole(t *testing.T) {
	for _, operation := range []string{"mkdir", "open", "copy"} {
		t.Run(operation, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			c := newRangeCase(t)
			r := writeRoot(t, 36, c.big)
			f := newSources(t, 37, c.zip)
			log := &dlLog{}
			f.dl = log.serve(c.zip, nil)
			u := f.updater(r)
			injected := false
			u.Opt.Progress = func(p Progress) {
				if injected || p.Stage != "downloading" || p.Total == 0 {
					return
				}
				injected = true
				dst := u.l.pkgAt("sdk")
				if operation == "mkdir" {
					if err := os.WriteFile(dst, []byte("blocks the destination directory"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					name := "sdk/emulator/emulator.exe"
					if operation == "copy" {
						name = "sdk/emulator/lib/big0.dll"
					}
					if err := os.MkdirAll(u.l.pkgAt(name), 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
			st, err := u.Prepare(context.Background())
			var unavailable *rangeUnavailable
			if !injected || err == nil || errors.As(err, &unavailable) || log.fullGets() != 0 || len(st.Notices) != 0 || st.Phase == phaseStaged {
				t.Fatalf("local %s error triggered a fallback: injected %v, status %+v, error %v, full GETs %d", operation, injected, st, err, log.fullGets())
			}
		})
	}
}

func TestRangeLargeEntryLifecycle(t *testing.T) {
	const largePath = "sdk/emulator/lib/new-large.dll"
	big := bigFiles(1, 4, 6<<20)
	large := bigFiles(2, 1, 9<<20)["sdk/emulator/lib/big0.dll"]
	files := pkgFiles(37, merged(big, map[string]string{largePath: large}))
	archive := zipOf(t, 37, files, false)
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	var dataOffset int64
	for _, f := range zr.File {
		if f.Name == largePath {
			dataOffset, err = f.DataOffset()
		}
	}
	if err != nil || dataOffset == 0 {
		t.Fatal("large entry not found", err)
	}
	for _, mode := range []string{"success", "retry", "cancel", "corrupt", "local-create", "local-open"} {
		t.Run(mode, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			r := writeRoot(t, 36, big)
			f := newSources(t, 37, archive)
			u := f.updater(r)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			log := &dlLog{}
			var requests, tempSeen atomic.Int32
			rangeBody := archive
			if mode == "corrupt" {
				rangeBody = append([]byte(nil), archive...)
				rangeBody[dataOffset+12345] ^= 0x55
			}
			serve := log.serve(archive, rangeBody)
			f.dl = func(w http.ResponseWriter, req *http.Request) {
				var start, end int64
				_, parseErr := fmt.Sscanf(req.Header.Get("Range"), "bytes=%d-%d", &start, &end)
				if parseErr != nil || end-start+1 <= rangeMaxGroup {
					serve(w, req)
					return
				}
				attempt := requests.Add(1)
				temps, err := filepath.Glob(filepath.Join(u.l.pkg(), ".range-*"))
				if err != nil || len(temps) != 1 {
					t.Errorf("large transfer has no private file: %v %v", temps, err)
				} else {
					tempSeen.Add(1)
				}
				if (mode == "retry" && attempt == 1) || mode == "cancel" {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(archive)))
					w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
					w.WriteHeader(http.StatusPartialContent)
					w.Write(bytes.Repeat([]byte{0x55}, 64<<10))
					w.(http.Flusher).Flush()
					if mode == "cancel" {
						cancel()
						<-req.Context().Done()
					}
					return
				}
				if mode == "local-open" {
					// Deny the next extracted output's open, after the large range was received.
					if err := os.MkdirAll(u.l.pkgAt(largePath), 0o755); err != nil {
						t.Error(err)
					}
				}
				serve(w, req)
			}
			if mode == "local-create" {
				u.Opt.Progress = func(p Progress) {
					if p.Stage != "downloading" || p.Total == 0 || p.Done != 0 {
						return
					}
					if err := os.Remove(u.l.pkg()); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(u.l.pkg(), []byte("cannot create a range file here"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			st, err := u.Prepare(ctx)
			switch mode {
			case "success", "retry", "corrupt":
				wantMode, wantNotices, wantFull := "range", 0, 0
				if mode == "corrupt" {
					wantMode, wantNotices, wantFull = "full", 1, 1
				}
				if err != nil || st.Phase != phaseStaged || stagedMode(u) != wantMode || len(st.Notices) != wantNotices || log.fullGets() != wantFull {
					t.Fatalf("prepare: %+v, error %v, full GETs %d", st, err, log.fullGets())
				}
				// buildStage has moved sdk/emulator into stage/emulator.
				got, err := os.ReadFile(filepath.Join(u.l.stageEmulator(), "lib", "new-large.dll"))
				if err != nil || !bytes.Equal(got, []byte(large)) {
					t.Fatalf("large staged output differs: %v", err)
				}
				if mode == "retry" && requests.Load() != 2 {
					t.Fatalf("want two large attempts, got %d", requests.Load())
				}
			case "cancel":
				if errs.CodeOf(err) != errs.Canceled || log.fullGets() != 0 || requests.Load() != 1 {
					t.Fatalf("cancel: %v, large attempts %d, full GETs %d", err, requests.Load(), log.fullGets())
				}
			default:
				if err == nil || log.fullGets() != 0 || st.Phase == phaseStaged {
					t.Fatalf("local failure: %+v %v, full GETs %d", st, err, log.fullGets())
				}
			}
			if mode != "local-create" && tempSeen.Load() == 0 {
				t.Fatal("large entry never used a private temporary file")
			}
			// Also catches leaked Windows handles: the existing abort and temp cleanup must remove the file.
			temps, err := filepath.Glob(filepath.Join(u.l.pkg(), ".range-*"))
			if err != nil || len(temps) != 0 {
				t.Fatalf("range file leaked: %v %v", temps, err)
			}
		})
	}
}

func TestRangeRetryDoesNotRetryLocalFailures(t *testing.T) {
	r := writeRoot(t, 36, nil)
	u := New(r, nil, Options{})
	for _, failure := range []error{io.ErrShortWrite, &os.PathError{Op: "write", Path: "range file", Err: syscall.Errno(112)}} {
		attempts := 0
		err := u.rangeRetry(context.Background(), 0, 1, "test", func() error { attempts++; return failure })
		var ru *rangeUnavailable
		if !errors.Is(err, failure) || attempts != 1 || errors.As(err, &ru) {
			t.Fatalf("attempts %d, error %v", attempts, err)
		}
	}
}
