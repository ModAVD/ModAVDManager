// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
)

type rangeWriter struct {
	count, max int
	err        error
	cancel     context.CancelFunc
}

func (w *rangeWriter) Write(p []byte) (int, error) {
	w.count += len(p)
	w.max = max(w.max, len(p))
	if w.cancel != nil {
		w.cancel()
	}
	if w.err != nil {
		return 0, w.err
	}
	return len(p), nil
}

func TestRangeToStreamsWithBoundedMemory(t *testing.T) {
	const size = 12 << 20
	// A generated body has no size-dependent allocation to hide the client's allocation cost.
	body := &watchedBody{Reader: io.LimitReader(zeroReader{}, size)}
	d := Client{HTTP: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {fmt.Sprintf("bytes 0-%d/%d", size-1, size)}}, ContentLength: size, Body: body}, nil
	})}}
	w := &rangeWriter{}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	err := d.RangeTo(context.Background(), "https://example.invalid/pkg", 0, size, size, "test", w)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if err != nil || w.count != size || w.max > 64<<10 || !body.closed || allocated > 2<<20 {
		t.Fatalf("stream: bytes %d, max chunk %d, allocated %d, closed %v, error %v", w.count, w.max, allocated, body.closed, err)
	}
	t.Logf("%d bytes transferred, max write %d, allocated %d", size, w.max, allocated)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestRangeToProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, cr, body, encoding string
		status                   int
		length                   int64
		kind                     Kind
		want                     string
	}{
		{"valid", "bytes 2-4/10", "cde", "", 206, 3, Network, "cde"},
		{"ignored", "", "abcdefghij", "", 200, 10, NoRange, ""},
		{"other range", "bytes 2-5/10", "cdef", "", 206, 4, NoRange, ""},
		{"wrong total", "bytes 2-4/11", "cde", "", 206, 3, NoRange, ""},
		{"wrong length", "bytes 2-4/10", "cd", "", 206, 2, Size, ""},
		{"encoding", "bytes 2-4/10", "cde", "gzip", 206, 3, Encoding, ""},
		{"short", "bytes 2-4/10", "cd", "", 206, -1, Network, "cd"},
		{"long", "bytes 2-4/10", "cdef", "", 206, -1, Size, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &watchedBody{Reader: strings.NewReader(tc.body)}
			d := Client{HTTP: &http.Client{Transport: roundTrip(func(req *http.Request) (*http.Response, error) {
				if req.Header.Get("Range") != "bytes=2-4" || req.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("headers: %v", req.Header)
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Range": {tc.cr}, "Content-Encoding": {tc.encoding}}, ContentLength: tc.length, Body: body}, nil
			})}}
			var dst bytes.Buffer
			err := d.RangeTo(context.Background(), "https://example.invalid/pkg", 2, 3, 10, "test", &dst)
			var de *Error
			if tc.name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.As(err, &de) || de.Kind != tc.kind {
				t.Fatalf("want kind %d, got %v", tc.kind, err)
			}
			if dst.String() != tc.want || !body.closed {
				t.Fatalf("output %q, closed %v", dst.String(), body.closed)
			}
		})
	}
}

func TestRangeToLocalErrorAndCancel(t *testing.T) {
	local := errors.New("disk full")
	for _, cancelWrite := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		body := &watchedBody{Reader: strings.NewReader("abc")}
		d := Client{HTTP: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 206, Header: http.Header{"Content-Range": {"bytes 0-2/3"}}, ContentLength: 3, Body: body}, nil
		})}}
		w := &rangeWriter{err: local}
		if cancelWrite {
			w.err, w.cancel = nil, cancel
		}
		err := d.RangeTo(ctx, "https://example.invalid/pkg", 0, 3, 3, "test", w)
		cancel()
		if (!cancelWrite && !errors.Is(err, local)) || (cancelWrite && errs.CodeOf(err) != errs.Canceled) || !body.closed {
			t.Fatalf("cancel %v: closed %v, error %v", cancelWrite, body.closed, err)
		}
	}
}

func TestRangeToStallClosesResponse(t *testing.T) {
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-5/6")
		w.Header().Set("Content-Length", "6")
		w.WriteHeader(206)
		io.WriteString(w, "abc")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(closed)
	}))
	defer srv.Close()
	d := Client{HTTP: NewHTTPClient(0), StallTimeout: 30 * time.Millisecond}
	err := d.RangeTo(context.Background(), srv.URL, 0, 6, 6, "test", io.Discard)
	var de *Error
	if !errors.As(err, &de) || de.Kind != Timeout {
		t.Fatalf("want timeout, got %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("response not canceled")
	}
}

func TestRangeToOverflow(t *testing.T) {
	requests := 0
	d := Client{HTTP: &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		requests++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}}
	for _, bounds := range [][3]int64{{math.MaxInt64 - 1, 5, math.MaxInt64}, {-1, 1, 10}, {0, 0, 10}} {
		if err := d.RangeTo(context.Background(), "https://example.invalid/pkg", bounds[0], bounds[1], bounds[2], "test", io.Discard); err == nil {
			t.Fatal("invalid range accepted")
		}
	}
	if requests != 0 {
		t.Fatal("invalid range sent a request")
	}
}
