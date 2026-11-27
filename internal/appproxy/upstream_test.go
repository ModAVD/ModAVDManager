package appproxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"modavd/internal/appproxy/testproxy"
)

func TestSOCKSAddr(t *testing.T) {
	for _, tg := range []Target{
		{Addr: netip.MustParseAddrPort("1.2.3.4:443")},
		{Addr: netip.MustParseAddrPort("[2001:db8::1]:53")},
		{Host: "game.example.com", Port: 8080},
	} {
		b := appendSOCKSAddr(nil, tg)
		got, n, err := parseSOCKSAddr(append(b, 0xaa))
		if err != nil || got != tg || n != len(b) {
			t.Errorf("%v: %v %d %v", tg, got, n, err)
		}
		got, err = readSOCKSAddr(bytes.NewReader(b))
		if err != nil || got != tg {
			t.Errorf("read %v: %v %v", tg, got, err)
		}
		if _, _, err := parseSOCKSAddr(b[:len(b)-1]); err == nil {
			t.Errorf("%v cut: no error", tg)
		}
	}
	if _, _, err := parseSOCKSAddr([]byte{9, 1, 2}); err == nil {
		t.Error("unknown type")
	}
}

func readAll(t *testing.T, c net.Conn, want string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, len(want))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != want {
		t.Fatalf("read %q %v, want %q", b, err, want)
	}
}

func TestDialTCP(t *testing.T) {
	echo := echoServer(t)
	for _, scheme := range []string{"socks5", "http"} {
		mode := testproxy.ModeSOCKS5
		if scheme == "http" {
			mode = testproxy.ModeHTTP
		}
		p := startProxy(t, mode, "", echo, nil)
		h := &Health{}
		u := &Upstream{Server: serverOf(t, scheme, p), Health: h}
		for _, tg := range []Target{{Host: "game.example.com", Port: 443}, {Addr: netip.MustParseAddrPort("1.2.3.4:443")}} {
			c, pending, err := u.DialTCP(context.Background(), tg)
			if err != nil {
				t.Fatalf("%s %v: %v", scheme, tg, err)
			}
			// the echo server speaks first: its greeting may come with the proxy's answer (HTTP: pending)
			got := string(pending)
			if len(got) < 5 {
				buf := make([]byte, 5-len(got))
				c.SetReadDeadline(time.Now().Add(5 * time.Second))
				io.ReadFull(c, buf)
				got += string(buf)
			}
			if got != "echo:" {
				t.Fatalf("%s: greeting %q", scheme, got)
			}
			c.Write([]byte("ping"))
			readAll(t, c, "ping")
			c.Close()
		}
		if got := targets(p, "connect"); !slices.Equal(got, []string{"game.example.com:443", "1.2.3.4:443"}) {
			t.Errorf("%s asked for %v", scheme, got)
		}
		if st := h.Status(u.Server); st.TCP != "ok" || st.LastOKAt == "" {
			t.Errorf("%s health %+v", scheme, st)
		}
	}
}

// oneShot is a server that answers each connection with reply (after reading what the client sends first), then
// hangs up.
func oneShot(t *testing.T, reply string) Server {
	l := listenLocal(t)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				c.Read(make([]byte, 4096))
				io.WriteString(c, reply)
			}()
		}
	}()
	s, _ := ParseServer("socks5://" + l.Addr().String())
	return s
}

func TestDialTCPErrors(t *testing.T) {
	tg := Target{Host: "game.example.com", Port: 443}
	cases := []struct {
		name, scheme, reply, want string
	}{
		{"socks auth", "socks5", "\x05\xff", "账号密码"},
		{"not socks", "socks5", "HTTP/1.1 400 Bad Request\r\n\r\n", "不是 SOCKS5"},
		{"http 407", "http", "HTTP/1.1 407 Proxy Authentication Required\r\n\r\n", "账号密码"},
		{"http 403", "http", "HTTP/1.1 403 Forbidden\r\nX: y\r\n\r\n", "HTTP 403"},
		{"not http", "http", "\x05\x00", "不是 HTTP"},
	}
	for _, c := range cases {
		s := oneShot(t, c.reply)
		s.Scheme = c.scheme
		_, _, err := (&Upstream{Server: s, Timeout: 3 * time.Second}).DialTCP(context.Background(), tg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// a refusal by the proxy still means the proxy answers
	p := startProxy(t, testproxy.ModeSOCKS5, "", "127.0.0.1:1", nil)
	h := &Health{}
	_, _, err := (&Upstream{Server: serverOf(t, "socks5", p), Health: h}).DialTCP(context.Background(), tg)
	var re *replyError
	if !errors.As(err, &re) {
		t.Errorf("refused: %v", err)
	}
	if st := h.Status(serverOf(t, "socks5", p)); st.TCP != "ok" || st.LastError == "" {
		t.Errorf("health after a refusal %+v", st)
	}
	// nobody there
	h = &Health{}
	if _, _, err := (&Upstream{Server: deadServer(t), Health: h}).DialTCP(context.Background(), tg); err == nil ||
		!strings.Contains(err.Error(), "连不上代理") {
		t.Errorf("dead: %v", err)
	}
	if st := h.Status(deadServer(t)); st.TCP != "failed" {
		t.Errorf("health %+v", st)
	}
}

func TestRelayAddr(t *testing.T) {
	server := netip.MustParseAddrPort("10.0.2.2:7890")
	for bnd, want := range map[string]string{
		"0.0.0.0:40000":     "10.0.2.2:40000",
		"127.0.0.1:40000":   "10.0.2.2:40000", // mihomo answers with where it listens
		"[::]:40000":        "10.0.2.2:40000",
		"[::1]:40000":       "10.0.2.2:40000",
		"192.168.1.5:40000": "192.168.1.5:40000",
	} {
		got, err := relayAddr(Target{Addr: netip.MustParseAddrPort(bnd)}, server)
		if err != nil || got.String() != want {
			t.Errorf("%s: %v %v, want %s", bnd, got, err, want)
		}
	}
	for _, bnd := range []Target{{Host: "relay.example", Port: 1}, {Addr: netip.MustParseAddrPort("1.2.3.4:0")},
		{Addr: netip.MustParseAddrPort("[2001:db8::1]:5")}} {
		if got, err := relayAddr(bnd, server); err == nil {
			t.Errorf("%v: %v", bnd, got)
		}
	}
}

func TestAssociate(t *testing.T) {
	echo := udpEcho(t)
	remote := netip.MustParseAddrPort("93.184.216.34:9")
	p := startProxy(t, testproxy.ModeSOCKS5, "", "", func(netip.AddrPort) netip.AddrPort { return echo })
	h := &Health{}
	u := &Upstream{Server: serverOf(t, "socks5", p), Health: h}
	a, err := u.Associate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if err := a.WriteTo([]byte("ping"), Target{Addr: remote}); err != nil {
		t.Fatal(err)
	}
	a.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, from, err := a.ReadFrom(make([]byte, 1500))
	if err != nil || string(got) != "ping" || from.Addr != remote {
		t.Fatalf("%q from %v: %v", got, from, err)
	}
	if st := h.Status(u.Server); st.UDP != "ok" {
		t.Errorf("health %+v", st)
	}
	if got := targets(p, "udp"); !slices.Equal(got, []string{remote.String()}) {
		t.Errorf("proxy saw %v", got)
	}
	// the proxy ending the association ends it here too
	p.Close()
	a.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := a.ReadFrom(make([]byte, 1500)); err == nil {
		t.Error("association outlived the proxy")
	}
}

func TestAssociateRefused(t *testing.T) {
	p := startProxy(t, testproxy.ModeSOCKS5NoUDP, "", "", nil)
	h := &Health{}
	_, err := (&Upstream{Server: serverOf(t, "socks5", p), Health: h}).Associate(context.Background())
	if !errors.Is(err, errNoUDP) {
		t.Errorf("no UDP: %v", err)
	}
	if st := h.Status(serverOf(t, "socks5", p)); st.UDP != "unsupported" {
		t.Errorf("%+v", st)
	}
	hp := startProxy(t, testproxy.ModeHTTP, "", "", nil)
	h = &Health{}
	if _, err := (&Upstream{Server: serverOf(t, "http", hp), Health: h}).Associate(context.Background()); !errors.Is(err, errNoUDP) {
		t.Errorf("HTTP: %v", err)
	}
	if st := h.Status(serverOf(t, "http", hp)); st.UDP != "unsupported" {
		t.Errorf("%+v", st)
	}
}
