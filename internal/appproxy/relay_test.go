package appproxy

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/appproxy/testproxy"
)

type sent struct {
	from, to netip.AddrPort
	p        string
}

// newTestCore is a core whose answers to apps arrive on the returned channel.
func newTestCore(t *testing.T, server string) (*Core, *counters, chan sent) {
	t.Helper()
	cfg := &Config{Version: 1, Server: server}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	c := &counters{}
	out := make(chan sent, 16)
	co := newCore(cfg, c, &Health{}, testLogger(t), func(from, to netip.AddrPort, p []byte) error {
		out <- sent{from, to, string(p)}
		return nil
	})
	t.Cleanup(co.Close)
	return co, c, out
}

// appConn returns both ends of a TCP connection: the app's, and the one the kernel would hand the daemon.
func appConn(t *testing.T) (app, daemon net.Conn) {
	l := listenLocal(t)
	app, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	daemon, err = l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close(); daemon.Close() })
	app.SetDeadline(time.Now().Add(10 * time.Second))
	return app, daemon
}

var remote = netip.MustParseAddrPort("93.184.216.34:5000")

func TestHandleTCP(t *testing.T) {
	p := startProxy(t, testproxy.ModeSOCKS5, "", echoServer(t), nil)
	co, c, _ := newTestCore(t, "socks5://"+p.Addr().String())

	// by address: the first bytes name nothing
	app, d := appConn(t)
	done := make(chan struct{})
	go func() { co.HandleTCP(d, remote); close(done) }()
	app.Write([]byte("hello"))
	readAll(t, app, "echo:hello")
	app.(*net.TCPConn).CloseWrite() // half close passes through: the echo server still answers, then ends
	if b, err := io.ReadAll(app); err != nil || string(b) != "bye" {
		t.Errorf("after FIN: %q %v", b, err)
	}
	<-done

	// by the TLS server name
	app, d = appConn(t)
	go co.HandleTCP(d, netip.MustParseAddrPort("93.184.216.34:443"))
	hello := clientHello(sniExt("game.example.com"))
	app.Write(hello)
	readAll(t, app, "echo:"+string(hello))
	app.Close()

	// by a name the daemon resolved: at once, without waiting for the app to speak
	co.dns.mu.Lock()
	co.dns.names[netip.MustParseAddr("93.184.216.35")] = nameEntry{name: "srv.example", expires: time.Now().Add(time.Hour)}
	co.dns.mu.Unlock()
	app, d = appConn(t)
	start := time.Now()
	go co.HandleTCP(d, netip.MustParseAddrPort("93.184.216.35:7000"))
	readAll(t, app, "echo:")
	if time.Since(start) >= sniffWait {
		t.Errorf("waited %v for the app to speak first", time.Since(start))
	}
	app.Close()

	// on a web port the name the app sends wins over the resolved one; when it sends none, the resolved one is used
	app, d = appConn(t)
	go co.HandleTCP(d, netip.MustParseAddrPort("93.184.216.35:443"))
	app.Write(hello)
	readAll(t, app, "echo:"+string(hello))
	app.Close()
	app, d = appConn(t)
	go co.HandleTCP(d, netip.MustParseAddrPort("93.184.216.35:443"))
	readAll(t, app, "echo:")
	app.Close()

	want := []string{remote.String(), "game.example.com:443", "srv.example:7000", "game.example.com:443", "srv.example:443"}
	if got := targets(p, "connect"); !slices.Equal(got, want) {
		t.Errorf("asked %v, want %v", got, want)
	}
	time.Sleep(50 * time.Millisecond)
	if c.tcp.Load() != 5 || c.bytesUp.Load() < int64(5+2*len(hello)) || c.bytesDown.Load() < 10 {
		t.Errorf("%+v", c.snapshot())
	}
}

func TestHandleTCPRefused(t *testing.T) {
	p := startProxy(t, testproxy.ModeSOCKS5, "", echoServer(t), nil)
	co, _, _ := newTestCore(t, "socks5://"+p.Addr().String())
	for _, dst := range []string{"10.0.0.5:80", "127.0.0.1:18990", "192.168.1.1:443", "93.184.216.34:0",
		"127.0.0.1:8080"} {
		app, d := appConn(t)
		go co.HandleTCP(d, netip.MustParseAddrPort(dst)) // a relay that should not be there would block
		app.SetReadDeadline(time.Now().Add(2 * time.Second))
		if b, err := io.ReadAll(app); len(b) != 0 || err != nil {
			t.Errorf("%s: %q %v", dst, b, err)
		}
		app.Close()
	}
	if got := p.Events(); len(got) != 0 {
		t.Errorf("proxy asked: %v", got)
	}
}

func TestHandleTCPPending(t *testing.T) {
	// an HTTP proxy that sends the server's first bytes along with its answer: they reach the app
	s := oneShot(t, "HTTP/1.1 200 Connection established\r\n\r\nSSH-2.0-banner\r\n")
	co, _, _ := newTestCore(t, "http://"+s.Addr.String())
	app, d := appConn(t)
	go co.HandleTCP(d, netip.MustParseAddrPort("93.184.216.34:22"))
	readAll(t, app, "SSH-2.0-banner\r\n")
}

func TestHandleTCPProxyDown(t *testing.T) {
	co, c, _ := newTestCore(t, "socks5://"+deadServer(t).Addr.String())
	app, d := appConn(t)
	go app.Write([]byte("GET / HTTP/1.1\r\nHost: game.example.com\r\n\r\n"))
	co.HandleTCP(d, netip.MustParseAddrPort("93.184.216.34:80"))
	app.SetReadDeadline(time.Now().Add(2 * time.Second))
	if b, _ := io.ReadAll(app); len(b) != 0 {
		t.Errorf("%q", b)
	}
	if c.tcpFailed.Load() != 1 || c.tcp.Load() != 0 {
		t.Errorf("%+v", c.snapshot())
	}
}

func TestHandleDNS(t *testing.T) {
	f := newFakeDNS(t, map[string]string{"game.example.com": "1.2.3.4"})
	p := startProxy(t, testproxy.ModeSOCKS5, f.addr, "", nil)
	co, _, out := newTestCore(t, "socks5://"+p.Addr().String())
	client := netip.MustParseAddrPort("127.0.0.1:40000") // netd's socket, through the route's src

	// over UDP: the answer comes from the address asked
	co.HandleUDP(client, guestDNS, query(5, "game.example.com", dnsTypeA, 0))
	select {
	case s := <-out:
		info, err := fixAnswer([]byte(s.p), 5)
		if s.from != guestDNS || s.to != client || err != nil || len(info.addrs) != 1 {
			t.Errorf("%+v %+v %v", s, info, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer")
	}

	// over TCP: framed, several on one connection
	app, d := appConn(t)
	go co.HandleTCP(d, guestDNS)
	for id := uint16(1); id <= 2; id++ {
		q := query(id, "game.example.com", dnsTypeA, 0)
		app.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...))
		var l [2]byte
		io.ReadFull(app, l[:])
		a := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(app, a); err != nil || binary.BigEndian.Uint16(a) != id {
			t.Errorf("TCP answer %d: %x %v", id, a, err)
		}
	}
}

// TestDNSOwnResolverIgnored: a listed app that asks a resolver of its own (one it runs, say) gets the configured
// resolver's answer. The cache is shared by the listed apps, so an answer from the app's resolver would reach the
// others: a game asking through netd would be sent to an address of that app's choosing for up to an hour.
func TestDNSOwnResolverIgnored(t *testing.T) {
	good := newFakeDNS(t, map[string]string{"game.example.com": "1.2.3.4"})
	evil := newFakeDNS(t, map[string]string{"game.example.com": "203.0.113.66"})
	evil.ttl = 3600
	p, err := testproxy.Start(testproxy.Options{Mode: testproxy.ModeSOCKS5,
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			to := good.addr
			if strings.HasPrefix(addr, "6.6.6.6:") {
				to = evil.addr
			}
			return (&net.Dialer{}).DialContext(ctx, network, to)
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	co, _, out := newTestCore(t, "socks5://"+p.Addr().String())
	ask := func(client, to netip.AddrPort, id uint16) string {
		t.Helper()
		co.HandleUDP(client, to, query(id, "game.example.com", dnsTypeA, 0))
		select {
		case s := <-out:
			info, err := fixAnswer([]byte(s.p), id)
			if err != nil || len(info.addrs) != 1 {
				t.Fatalf("answer %+v %v", info, err)
			}
			return info.addrs[0].String()
		case <-time.After(5 * time.Second):
			t.Fatal("no answer")
		}
		return ""
	}
	if got := ask(netip.MustParseAddrPort("10.0.2.16:40001"), netip.MustParseAddrPort("6.6.6.6:53"), 1); got != "1.2.3.4" {
		t.Errorf("the app's own resolver answered: %s", got)
	}
	if got := ask(netip.MustParseAddrPort("127.0.0.1:40002"), guestDNS, 2); got != "1.2.3.4" {
		t.Errorf("another app through netd got %s", got)
	}
	if evil.queries.Load() != 0 || slices.ContainsFunc(targets(p, "connect"), func(s string) bool { return s != "1.1.1.1:53" }) {
		t.Errorf("asked %v, the app's resolver %d times", targets(p, "connect"), evil.queries.Load())
	}
}

func TestHandleUDP(t *testing.T) {
	other := netip.MustParseAddrPort("93.184.216.35:9")
	echoes := map[netip.AddrPort]netip.AddrPort{remote: udpEcho(t), other: udpEcho(t)}
	p := startProxy(t, testproxy.ModeSOCKS5, "", "", func(dst netip.AddrPort) netip.AddrPort { return echoes[dst] })
	co, c, out := newTestCore(t, "socks5://"+p.Addr().String())
	client := netip.MustParseAddrPort("10.0.2.16:4000")
	co.HandleUDP(client, remote, []byte("one"))
	co.HandleUDP(client, other, []byte("two"))
	got := map[netip.AddrPort]string{}
	for len(got) < 2 {
		select {
		case s := <-out:
			if s.to != client {
				t.Fatalf("answer to %v", s.to)
			}
			got[s.from] = s.p
		case <-time.After(5 * time.Second):
			t.Fatalf("answers so far %v", got)
		}
	}
	if got[remote] != "one" || got[other] != "two" {
		t.Errorf("%v", got)
	}
	// one association for the app's socket, both destinations through it
	if n := len(targets(p, "associate")); n != 1 || c.udpSessions.Load() != 1 {
		t.Errorf("%d associations, %d sessions", n, c.udpSessions.Load())
	}
	co.HandleUDP(netip.MustParseAddrPort("10.0.2.16:4001"), netip.MustParseAddrPort("10.0.0.9:9"), []byte("lan"))
	co.HandleUDP(client, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), UDPPort), []byte("self"))
	time.Sleep(100 * time.Millisecond)
	if c.udpSessions.Load() != 1 {
		t.Error("private or own address relayed")
	}
}

func TestHandleUDPNoUDP(t *testing.T) {
	p := startProxy(t, testproxy.ModeSOCKS5NoUDP, "", "", nil)
	co, c, out := newTestCore(t, "socks5://"+p.Addr().String())
	co.HandleUDP(netip.MustParseAddrPort("10.0.2.16:4000"), remote, []byte("one"))
	deadline := time.Now().Add(5 * time.Second)
	for {
		co.udp.mu.Lock()
		refused := !co.udp.noUDP.IsZero()
		co.udp.mu.Unlock()
		if refused || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// not asked again for a while: dropped at once
	before := c.udpDropped.Load()
	co.HandleUDP(netip.MustParseAddrPort("10.0.2.16:4001"), remote, []byte("two"))
	if c.udpDropped.Load() != before+1 {
		t.Errorf("%+v", c.snapshot())
	}
	if n := len(targets(p, "associate")); n != 1 {
		t.Errorf("asked %d times", n)
	}
	select {
	case s := <-out:
		t.Errorf("answer %+v", s)
	default:
	}
}
