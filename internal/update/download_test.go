package update

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/testutil"
)

func TestDownloadStallRetriesAndResumes(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Length", "6")
			io.WriteString(w, "abc")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Range") != "bytes=3-" {
			t.Errorf("resume %v", r.Header)
		}
		w.Header().Set("Content-Range", "bytes 3-5/6")
		w.WriteHeader(206)
		io.WriteString(w, "def")
	}))
	defer srv.Close()
	u := &Updater{l: layout{root: testutil.TempDir(t)}, Log: logx.Nop(), stallTimeout: 50 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	path, err := u.download(ctx, &release{source: SourceGitHub, url: srv.URL, assetName: "test.zip", size: 6, sha256: hashBytes([]byte("abcdef"))})
	if err != nil || attempts.Load() != 2 {
		t.Fatalf("attempts %d: %v", attempts.Load(), err)
	}
	if b, _ := os.ReadFile(path); string(b) != "abcdef" {
		t.Fatalf("%q", b)
	}
}

type sourceTransport func(*http.Request) (*http.Response, error)

func (f sourceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMirrorAPIUsesSharedHeadersAndRedaction(t *testing.T) {
	secret := "a b/+?CDK"
	u := &Updater{httpClient: &http.Client{Transport: sourceTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Query().Get("cdk") != secret || r.Header.Get("User-Agent") != "ModAVDManager/"+buildinfo.Version || r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("API headers or CDK not forwarded")
		}
		return nil, errors.New("transport echoed " + secret + " " + url.QueryEscape(secret))
	})}}
	_, err := u.mirrorLatest(context.Background(), secret, nil)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), url.QueryEscape(secret)) || !strings.Contains(err.Error(), "***") {
		t.Fatalf("unsafe API error: %v", err)
	}
}

func TestUpdateRejectsWrongContentRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 2-5/6")
		w.WriteHeader(206)
		io.WriteString(w, "cdef")
	}))
	defer srv.Close()
	p := filepath.Join(testutil.TempDir(t), "part")
	os.WriteFile(p, []byte("abc"), 0600)
	u := &Updater{}
	err := u.fetch(context.Background(), &release{url: srv.URL, size: 6}, p, "test")
	b, _ := os.ReadFile(p)
	if err == nil || !strings.Contains(err.Error(), "Content-Range") || string(b) != "abc" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestMirrorDownloadsAlwaysStartFresh(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Errorf("MirrorChyan range %v", r.Header)
		}
		w.Header().Set("Content-Length", "6")
		if hits.Add(1) == 1 {
			io.WriteString(w, "abc")
			return
		}
		io.WriteString(w, "abcdef")
	}))
	defer srv.Close()
	u := &Updater{l: layout{root: testutil.TempDir(t)}, Log: logx.Nop()}
	os.MkdirAll(u.l.download(), 0700)
	os.WriteFile(u.l.downloadAt("test.zip")+".part", []byte("oldold"), 0600)
	p, err := u.download(context.Background(), &release{source: SourceMirrorChyan, url: srv.URL, assetName: "test.zip", size: 6, sha256: hashBytes([]byte("abcdef"))})
	b, _ := os.ReadFile(p)
	if err != nil || string(b) != "abcdef" || hits.Load() != 2 {
		t.Fatal(fmt.Sprint(err), string(b), hits.Load())
	}
}

func TestFullUpdatePartIsDownloadedAgain(t *testing.T) {
	for _, tc := range []struct{ name, part, digest string }{
		{"bad cache", "badbad", hashBytes([]byte("abcdef"))},
		{"complete cache", "abcdef", hashBytes([]byte("abcdef"))},
		{"no source digest", "badbad", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Header.Get("Range") != "" {
					t.Errorf("complete part must restart: %v", r.Header)
				}
				io.WriteString(w, "abcdef")
			}))
			defer srv.Close()
			u := &Updater{l: layout{root: testutil.TempDir(t)}, Log: logx.Nop()}
			if err := os.MkdirAll(u.l.download(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(u.l.downloadAt("test.zip")+".part", []byte(tc.part), 0600); err != nil {
				t.Fatal(err)
			}
			path, err := u.download(context.Background(), &release{source: SourceGitHub, url: srv.URL, assetName: "test.zip", size: 6, sha256: tc.digest})
			if err != nil {
				t.Fatalf("healthy source requested %d times: %v", hits.Load(), err)
			}
			b, err := os.ReadFile(path)
			if err != nil || string(b) != "abcdef" || hits.Load() != 1 {
				t.Fatalf("hits=%d bytes=%q err=%v", hits.Load(), b, err)
			}
		})
	}
}

func TestUpdateStillRejectsBadSourceDigest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, "badbad") }))
	defer srv.Close()
	u := &Updater{l: layout{root: testutil.TempDir(t)}, Log: logx.Nop()}
	path, err := u.download(context.Background(), &release{source: SourceGitHub, url: srv.URL, assetName: "test.zip", size: 6, sha256: hashBytes([]byte("abcdef"))})
	if path != "" || errs.CodeOf(err) != errs.UpdateRejected || hits.Load() != 1 {
		t.Fatalf("path=%q hits=%d err=%v", path, hits.Load(), err)
	}
	if _, err := os.Stat(u.l.downloadAt("test.zip") + ".part"); !os.IsNotExist(err) {
		t.Fatalf("bad part remains: %v", err)
	}
}
