package download

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
	"reflect"
	"strings"
	"testing"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/errs"
	"modavd/internal/testutil"
)

func TestFileProtocol(t *testing.T) {
	for _, tc := range []struct {
		name               string
		status             int
		cr, encoding, body string
		ok, fresh          bool
	}{
		{"resume", 206, "bytes 3-5/6", "", "def", true, false},
		{"restart", 200, "", "", "abcdef", true, false},
		{"fresh", 200, "", "", "abcdef", true, true},
		{"wrong start", 206, "bytes 2-5/6", "", "cdef", false, false},
		{"wrong total", 206, "bytes 3-6/7", "", "defg", false, false},
		{"missing range", 206, "", "", "def", false, false},
		{"416", 416, "bytes */2", "", "", false, false},
		{"compressed", 200, "", "gzip", "abcdef", false, false},
		{"wrong length", 200, "", "", "abcde", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				want := "bytes=3-"
				if tc.fresh {
					want = ""
				}
				if r.Header.Get("Range") != want || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("User-Agent") != "ModAVDManager/"+buildinfo.Version {
					t.Errorf("headers %v", r.Header)
				}
				w.Header().Set("Content-Range", tc.cr)
				w.Header().Set("Content-Encoding", tc.encoding)
				w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)))
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			part := filepath.Join(testutil.TempDir(t), "x.part")
			if err := os.WriteFile(part, []byte("abc"), 0600); err != nil {
				t.Fatal(err)
			}
			d := Client{HTTP: NewHTTPClient(0)}
			var counts []int64
			var start int64 = -1
			var restarted bool
			err := d.File(context.Background(), FileOptions{URL: srv.URL, Path: part, Size: 6, Fresh: tc.fresh,
				Start: func(n int64, reset bool) { start, restarted = n, reset }, Progress: func(n int64) { counts = append(counts, n) }})
			got, _ := os.ReadFile(part)
			if tc.ok {
				if err != nil || string(got) != "abcdef" || len(counts) == 0 || counts[len(counts)-1] != 6 {
					t.Fatalf("%q %v %v", got, counts, err)
				}
				want := int64(0)
				if tc.status == 206 {
					want = 3
				}
				if start != want || restarted != (tc.name == "restart") {
					t.Fatalf("start %d reset %v", start, restarted)
				}
			} else if err == nil || string(got) != "abc" || start != -1 || len(counts) != 0 {
				t.Fatalf("rejected response changed part: %q %d %v %v", got, start, counts, err)
			}
		})
	}
}

func TestStallAndCancellation(t *testing.T) {
	for _, cancelNow := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelNow), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			closed := make(chan struct{})
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "6")
				io.WriteString(w, "abc")
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(closed)
			}))
			defer srv.Close()
			d := Client{HTTP: NewHTTPClient(0), StallTimeout: 50 * time.Millisecond}
			p := filepath.Join(testutil.TempDir(t), "part")
			err := d.File(ctx, FileOptions{URL: srv.URL, Path: p, Size: 6, Progress: func(int64) {
				if cancelNow {
					cancel()
				}
			}})
			if cancelNow {
				if errs.CodeOf(err) != errs.Canceled {
					t.Fatal(err)
				}
			} else {
				var e *Error
				if !errors.As(err, &e) || e.Kind != Timeout {
					t.Fatalf("want stall timeout, got %v", err)
				}
			}
			select {
			case <-closed:
			case <-time.After(time.Second):
				t.Fatal("response was not canceled")
			}
			if got, _ := os.ReadFile(p); string(got) != "abc" {
				t.Fatalf("lost partial bytes %q", got)
			}
		})
	}
}

func TestRedirectLimit(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; http.Redirect(w, r, "/again", http.StatusFound) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d := Client{HTTP: NewHTTPClient(0)}
	err := d.File(ctx, FileOptions{URL: srv.URL, Path: filepath.Join(testutil.TempDir(t), "part"), Size: 1})
	if err == nil || !strings.Contains(err.Error(), "重定向超过 10 次") || hits != 10 {
		t.Fatalf("hits %d: %v", hits, err)
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type watchedBody struct {
	io.Reader
	closed bool
}

func (b *watchedBody) Close() error { b.closed = true; return nil }
func TestRejectedResponsesClose(t *testing.T) {
	for _, code := range []int{301, 400, 404, 416, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			b := &watchedBody{Reader: strings.NewReader("refused")}
			var request *http.Request
			h := &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				request = r
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: b, Request: r}, nil
			})}
			d := Client{HTTP: h}
			_, err := d.Bytes(context.Background(), "http://example.invalid", "test", 1024, time.Second)
			if err == nil || !b.closed || request.Context().Err() == nil {
				t.Fatalf("body closed %v, context %v, err %v", b.closed, request.Context().Err(), err)
			}
		})
	}
}
func TestNetErrorRedactsCDK(t *testing.T) {
	secret := "a b/+?秘密"
	err := &url.Error{Op: "Get", URL: "https://mirror.invalid/?cdk=" + url.QueryEscape(secret), Err: fmt.Errorf("dial failed %s %s", secret, url.QueryEscape(secret))}
	got := NetError(err, secret)
	if strings.Contains(got, secret) || strings.Contains(got, url.QueryEscape(secret)) || strings.Contains(got, "mirror.invalid") || got != "dial failed *** ***" {
		t.Fatal(got)
	}
}
func TestDefaultTimeouts(t *testing.T) {
	c := Client{}
	tr := NewHTTPClient(0).Transport.(*http.Transport)
	if !reflect.DeepEqual([]time.Duration{c.connectTimeout(), c.stallTimeout(), tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout}, []time.Duration{20 * time.Second, 30 * time.Second, 20 * time.Second, 20 * time.Second}) {
		t.Fatal("defaults differ")
	}
}
