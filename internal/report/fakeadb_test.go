package report

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeAdb is an adb server on 127.0.0.1 that answers the host protocol from a scenario's adb data: host:devices,
// host-serial:<serial>:features, host:transport:<serial> and then shell: (stdout and stderr merged),
// shell,v2[,raw]: (the shell protocol's packets with the exit code) or exec: (raw bytes).
type fakeAdb struct {
	ln   net.Listener
	data scenarioAdb

	mu       sync.Mutex
	requests []string
	conns    int
}

func startFakeAdb(t *testing.T, data scenarioAdb) *fakeAdb {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAdb{ln: ln, data: data}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns++
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeAdb) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeAdb) seen() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns, append([]string(nil), f.requests...)
}

func readRequest(c net.Conn) (string, error) {
	h := make([]byte, 4)
	if _, err := io.ReadFull(c, h); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(h), 16, 16)
	if err != nil {
		return "", err
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(c, b); err != nil {
		return "", err
	}
	return string(b), nil
}

func okay(c net.Conn, payload *string) {
	io.WriteString(c, "OKAY")
	if payload != nil {
		fmt.Fprintf(c, "%04x%s", len(*payload), *payload)
	}
}

func fail(c net.Conn, msg string) { fmt.Fprintf(c, "FAIL%04x%s", len(msg), msg) }

func (f *fakeAdb) serve(c net.Conn) {
	defer c.Close()
	req, err := readRequest(c)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	switch {
	case req == "host:devices":
		okay(c, &f.data.Devices)
	case strings.HasPrefix(req, "host-serial:") && strings.HasSuffix(req, ":features"):
		serial := strings.TrimSuffix(strings.TrimPrefix(req, "host-serial:"), ":features")
		s, ok := f.data.Serials[serial]
		if !ok {
			fail(c, "device '"+serial+"' not found")
			return
		}
		okay(c, &s.Features)
	case strings.HasPrefix(req, "host:transport:"):
		serial := strings.TrimPrefix(req, "host:transport:")
		s, ok := f.data.Serials[serial]
		if !ok {
			fail(c, "device '"+serial+"' not found")
			return
		}
		okay(c, nil)
		svc, err := readRequest(c)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, serial+" "+svc)
		f.mu.Unlock()
		f.device(c, s, svc)
	default:
		fail(c, "unknown host service")
	}
}

func (f *fakeAdb) device(c net.Conn, s scenarioSerial, svc string) {
	switch {
	case strings.HasPrefix(svc, "exec:"):
		okay(c, nil)
		if e, ok := s.Exec[strings.TrimPrefix(svc, "exec:")]; ok {
			b, _ := base64.StdEncoding.DecodeString(e.B64)
			c.Write(b)
		}
	case strings.HasPrefix(svc, "shell,v2,raw:"), strings.HasPrefix(svc, "shell,v2:"):
		_, cmd, _ := strings.Cut(svc, ":")
		okay(c, nil)
		r := s.Shell[cmd]
		packet := func(id byte, data []byte) {
			var h [5]byte
			h[0] = id
			binary.LittleEndian.PutUint32(h[1:], uint32(len(data)))
			c.Write(append(h[:], data...))
		}
		if r.Out != "" {
			packet(1, []byte(r.Out))
		}
		if r.Err != "" {
			packet(2, []byte(r.Err))
		}
		code := 0
		if r.Exit != nil {
			code = *r.Exit
		}
		packet(3, []byte{byte(code)})
	case strings.HasPrefix(svc, "shell:"):
		okay(c, nil)
		r := s.Shell[strings.TrimPrefix(svc, "shell:")]
		var b bytes.Buffer
		b.WriteString(r.Out)
		b.WriteString(r.Err)
		c.Write(b.Bytes())
	default:
		fail(c, "unknown service "+svc)
	}
}

// guardDials lets dialAdb reach only the given ports (the fake servers) for the rest of the test and records every
// port it was asked for: a test never connects to a real adb server of this computer.
func guardDials(t *testing.T, allowed ...int) *[]string {
	t.Helper()
	var mu sync.Mutex
	var dialed []string
	old := dialTCP
	dialTCP = func(ctx context.Context, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		for _, p := range allowed {
			if addr == "127.0.0.1:"+strconv.Itoa(p) {
				return old(ctx, addr)
			}
		}
		return nil, fmt.Errorf("test: no dialing %s", addr)
	}
	t.Cleanup(func() { dialTCP = old })
	return &dialed
}
