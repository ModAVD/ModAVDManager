package appproxy

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/appproxy/testproxy"
)

// listenLocal is a TCP listener on 127.0.0.1, closed at the end of the test.
func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

// echoServer echoes every TCP connection, prefixed with "echo:" once and followed by "bye" at the client's FIN.
func echoServer(t *testing.T) string {
	l := listenLocal(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte("echo:"))
				io.Copy(c, c)
				c.Write([]byte("bye")) // after the client's FIN: reaches it only if the FIN was passed on as a half close
			}()
		}
	}()
	return l.Addr().String()
}

// udpEcho echoes datagrams.
func udpEcho(t *testing.T) netip.AddrPort {
	c, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := c.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			c.WriteToUDPAddrPort(buf[:n], from)
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).AddrPort()
}

// fakeDNS is a resolver over TCP: names in a are answered with that address (TTL ttl), "nx.example" with NXDOMAIN,
// the rest with SERVFAIL. It counts the queries it got.
type fakeDNS struct {
	addr    string
	queries atomic.Int64
	a       map[string]string
	ttl     uint32
	delay   time.Duration
}

func newFakeDNS(t *testing.T, a map[string]string) *fakeDNS {
	l := listenLocal(t)
	f := &fakeDNS{addr: l.Addr().String(), a: a, ttl: 300}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeDNS) serve(c net.Conn) {
	defer c.Close()
	var wmu sync.Mutex
	var l [2]byte
	for {
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		f.queries.Add(1)
		go func() { // answers out of order, as a pipelining resolver may
			time.Sleep(f.delay)
			qd, err := parseQuery(q)
			if err != nil {
				return
			}
			var a []byte
			switch ip, ok := f.a[qd.name]; {
			case ok:
				a = answer(q[:qd.end], rcodeNoError, 0, aRR(f.ttl, ip))
			case qd.name == "nx.example":
				a = answer(q[:qd.end], rcodeNXDomain, 0)
			default:
				a = answer(q[:qd.end], rcodeServFail, 0)
			}
			wmu.Lock()
			c.Write(binary.BigEndian.AppendUint16(nil, uint16(len(a))))
			c.Write(a)
			wmu.Unlock()
		}()
	}
}

// startProxy runs a testproxy whose TCP targets on port 53 go to dns (when set) and the rest to tcp (when set).
func startProxy(t *testing.T, mode testproxy.Mode, dns, tcp string, mapUDP func(netip.AddrPort) netip.AddrPort) *testproxy.Proxy {
	t.Helper()
	p, err := testproxy.Start(testproxy.Options{Mode: mode, MapUDP: mapUDP,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			switch {
			case strings.HasSuffix(addr, ":53") && dns != "":
				addr = dns
			case tcp != "":
				addr = tcp
			}
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func serverOf(t *testing.T, scheme string, p *testproxy.Proxy) Server {
	s, err := ParseServer(scheme + "://" + p.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// deadServer is an address where nothing listens.
func deadServer(t *testing.T) Server {
	l := listenLocal(t)
	addr := l.Addr().String()
	l.Close()
	s, _ := ParseServer("socks5://" + addr)
	return s
}

func testLogger(t *testing.T) *log.Logger {
	return log.New(testWriter{t}, "", 0)
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// targets are the events' targets of one kind.
func targets(p *testproxy.Proxy, kind string) []string {
	var out []string
	for _, e := range p.Events() {
		if e.Kind == kind {
			out = append(out, e.Target)
		}
	}
	return out
}
