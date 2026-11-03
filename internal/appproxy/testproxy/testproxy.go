// Package testproxy is a small SOCKS5 / HTTP CONNECT proxy for testing the per-app proxy: it really relays, and logs
// every CONNECT and every UDP datagram (destination, size) as one JSON line, so a test can tell which traffic went
// through it. Never a stand-in for the user's own proxy program: tests start one of these on a free port of their
// own and leave the user's alone.
//
// Modes: ModeSOCKS5 relays TCP and UDP and, like mihomo, answers UDP ASSOCIATE with 127.0.0.1 as the relay address;
// ModeSOCKS5NoUDP refuses UDP ASSOCIATE; ModeHTTP takes only CONNECT. No authentication.
package testproxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Mode string

const (
	ModeSOCKS5      Mode = "socks5"
	ModeSOCKS5NoUDP Mode = "socks5-noudp"
	ModeHTTP        Mode = "http"
)

// Event is one logged line.
type Event struct {
	Time   string `json:"time"`
	Kind   string `json:"kind"` // "connect", "udp", "associate"
	Client string `json:"client"`
	Target string `json:"target"`          // host:port as asked for
	Bytes  int    `json:"bytes,omitempty"` // UDP payload
	Error  string `json:"error,omitempty"`
}

type Options struct {
	Mode   Mode
	Listen string    // default 127.0.0.1:0
	Log    io.Writer // JSON lines; nil: none
	// Dial reaches targets (tests point it at local servers); nil: a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// ListenUDP opens the outgoing socket of an association; nil: an unbound UDP socket.
	ListenUDP func() (*net.UDPConn, error)
	// Resolve turns a UDP target's name into an address; nil: the system resolver.
	Resolve func(host string) (netip.Addr, error)
	// MapUDP redirects UDP targets (tests point them at local servers); answers from the new address are reported
	// as from the original one. nil: none.
	MapUDP func(netip.AddrPort) netip.AddrPort
}

type Proxy struct {
	o     Options
	l     net.Listener
	mu    sync.Mutex
	ev    []Event
	conns map[net.Conn]bool
	wg    sync.WaitGroup
}

// Start listens and serves until Close.
func Start(o Options) (*Proxy, error) {
	if o.Listen == "" {
		o.Listen = "127.0.0.1:0"
	}
	switch o.Mode {
	case ModeSOCKS5, ModeSOCKS5NoUDP, ModeHTTP:
	default:
		return nil, fmt.Errorf("unknown mode %q", o.Mode)
	}
	l, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return nil, err
	}
	p := &Proxy{o: o, l: l, conns: map[net.Conn]bool{}}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

func (p *Proxy) Addr() netip.AddrPort { return p.l.Addr().(*net.TCPAddr).AddrPort() }

// Close stops listening and ends every connection (associations too). Calling it again is fine.
func (p *Proxy) Close() {
	p.l.Close()
	p.wg.Wait()
	p.mu.Lock()
	for c := range p.conns {
		c.Close()
	}
	p.mu.Unlock()
}

// Events is everything logged so far.
func (p *Proxy) Events() []Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]Event(nil), p.ev...)
}

func (p *Proxy) record(e Event) {
	e.Time = time.Now().UTC().Format(time.RFC3339Nano)
	p.mu.Lock()
	p.ev = append(p.ev, e)
	if p.o.Log != nil {
		b, _ := json.Marshal(e)
		p.o.Log.Write(append(b, '\n'))
	}
	p.mu.Unlock()
}

func (p *Proxy) serve() {
	defer p.wg.Done()
	for {
		c, err := p.l.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns[c] = true
		p.mu.Unlock()
		go func() {
			defer func() {
				c.Close()
				p.mu.Lock()
				delete(p.conns, c)
				p.mu.Unlock()
			}()
			if p.o.Mode == ModeHTTP {
				p.serveHTTP(c)
			} else {
				p.serveSOCKS(c)
			}
		}()
	}
}

func (p *Proxy) dial(addr string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if p.o.Dial != nil {
		return p.o.Dial(ctx, "tcp", addr)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func relay(a, b net.Conn) {
	done := make(chan struct{})
	go func() {
		io.Copy(a, b)
		if cw, ok := a.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		}
		close(done)
	}()
	io.Copy(b, a)
	if cw, ok := b.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	<-done
}

// ---- HTTP

func (p *Proxy) serveHTTP(c net.Conn) {
	br := bufio.NewReader(c)
	line, err := br.ReadString('\n')
	if err != nil {
		return
	}
	f := strings.Fields(line)
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(h) == "" {
			break
		}
	}
	if len(f) != 3 || f[0] != "CONNECT" {
		io.WriteString(c, "HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 0\r\n\r\n")
		return
	}
	e := Event{Kind: "connect", Client: c.RemoteAddr().String(), Target: f[1]}
	up, err := p.dial(f[1])
	if err != nil {
		e.Error = err.Error()
		p.record(e)
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
		return
	}
	defer up.Close()
	p.record(e)
	io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
	if n := br.Buffered(); n > 0 {
		b, _ := br.Peek(n)
		up.Write(b)
	}
	relay(c, up)
}

// ---- SOCKS5

func (p *Proxy) serveSOCKS(c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	var h [2]byte
	if _, err := io.ReadFull(c, h[:]); err != nil || h[0] != 5 {
		return
	}
	methods := make([]byte, h[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if !strings.Contains(string(methods), "\x00") {
		c.Write([]byte{5, 0xff})
		return
	}
	c.Write([]byte{5, 0})
	var req [3]byte
	if _, err := io.ReadFull(c, req[:]); err != nil || req[0] != 5 {
		return
	}
	target, err := readAddr(c)
	if err != nil {
		return
	}
	c.SetDeadline(time.Time{})
	switch req[1] {
	case 1:
		p.socksConnect(c, target)
	case 3:
		if p.o.Mode == ModeSOCKS5NoUDP {
			p.record(Event{Kind: "associate", Client: c.RemoteAddr().String(), Error: "refused"})
			reply(c, 7, netip.AddrPort{})
			return
		}
		p.socksAssociate(c)
	default:
		reply(c, 7, netip.AddrPort{})
	}
}

func (p *Proxy) socksConnect(c net.Conn, target string) {
	e := Event{Kind: "connect", Client: c.RemoteAddr().String(), Target: target}
	up, err := p.dial(target)
	if err != nil {
		e.Error = err.Error()
		p.record(e)
		reply(c, 5, netip.AddrPort{})
		return
	}
	defer up.Close()
	p.record(e)
	reply(c, 0, up.LocalAddr().(*net.TCPAddr).AddrPort())
	relay(c, up)
}

// socksAssociate relays datagrams between the client and their targets until the client closes the TCP connection.
// The relay socket listens on the proxy's own address but is announced as 127.0.0.1, as mihomo does.
func (p *Proxy) socksAssociate(c net.Conn) {
	host, _, _ := net.SplitHostPort(p.l.Addr().String())
	rc, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(host), 0)))
	if err != nil {
		reply(c, 1, netip.AddrPort{})
		return
	}
	defer rc.Close()
	var out *net.UDPConn
	if p.o.ListenUDP != nil {
		out, err = p.o.ListenUDP()
	} else {
		out, err = net.ListenUDP("udp", nil)
	}
	if err != nil {
		reply(c, 1, netip.AddrPort{})
		return
	}
	defer out.Close()
	p.record(Event{Kind: "associate", Client: c.RemoteAddr().String()})
	port := rc.LocalAddr().(*net.UDPAddr).AddrPort().Port()
	reply(c, 0, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port))

	var mu sync.Mutex
	var client netip.AddrPort                     // where the client's datagrams come from (the reply goes there)
	mapped := map[netip.AddrPort]netip.AddrPort{} // MapUDP's address → the target asked for
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := out.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
			mu.Lock()
			to := client
			if orig, ok := mapped[from]; ok {
				from = orig
			}
			mu.Unlock()
			if !to.IsValid() {
				continue
			}
			h := appendAddr([]byte{0, 0, 0}, from)
			rc.WriteToUDPAddrPort(append(h, buf[:n]...), to)
		}
	}()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, from, err := rc.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if n < 4 || buf[2] != 0 {
				continue
			}
			target, m, err := parseAddr(buf[3:n])
			if err != nil {
				continue
			}
			mu.Lock()
			client = from
			mu.Unlock()
			payload := buf[3+m : n]
			p.record(Event{Kind: "udp", Client: from.String(), Target: target, Bytes: len(payload)})
			dst, err := p.resolve(target)
			if err != nil {
				continue
			}
			if p.o.MapUDP != nil {
				local := p.o.MapUDP(dst)
				mu.Lock()
				mapped[local] = dst
				mu.Unlock()
				dst = local
			}
			out.WriteToUDPAddrPort(payload, dst)
		}
	}()
	io.Copy(io.Discard, c)
}

func (p *Proxy) resolve(target string) (netip.AddrPort, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return netip.AddrPort{}, err
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return netip.AddrPort{}, err
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		if p.o.Resolve != nil {
			a, err = p.o.Resolve(host)
		} else {
			var ips []netip.Addr
			if ips, err = net.DefaultResolver.LookupNetIP(context.Background(), "ip4", host); err == nil {
				if len(ips) == 0 {
					return netip.AddrPort{}, errors.New("no address")
				}
				a = ips[0]
			}
		}
		if err != nil {
			return netip.AddrPort{}, err
		}
	}
	return netip.AddrPortFrom(a, uint16(n)), nil
}

func reply(c net.Conn, code byte, bnd netip.AddrPort) {
	if !bnd.IsValid() {
		bnd = netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	}
	c.Write(appendAddr([]byte{5, code, 0}, bnd))
}

func appendAddr(b []byte, ap netip.AddrPort) []byte {
	a := ap.Addr().Unmap()
	if a.Is4() {
		b = append(b, 1)
	} else {
		b = append(b, 4)
	}
	b = append(b, a.AsSlice()...)
	return binary.BigEndian.AppendUint16(b, ap.Port())
}

// readAddr reads ATYP, address and port as "host:port".
func readAddr(r io.Reader) (string, error) {
	var t [1]byte
	if _, err := io.ReadFull(r, t[:]); err != nil {
		return "", err
	}
	var n int
	switch t[0] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", err
		}
		b := make([]byte, int(l[0])+2)
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return net.JoinHostPort(string(b[:l[0]]), strconv.Itoa(int(binary.BigEndian.Uint16(b[l[0]:])))), nil
	default:
		return "", errors.New("bad address type")
	}
	b := make([]byte, n+2)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	a, _ := netip.AddrFromSlice(b[:n])
	return netip.AddrPortFrom(a.Unmap(), binary.BigEndian.Uint16(b[n:])).String(), nil
}

func parseAddr(b []byte) (string, int, error) {
	if len(b) < 1 {
		return "", 0, io.ErrUnexpectedEOF
	}
	switch b[0] {
	case 1, 4:
		n := 4
		if b[0] == 4 {
			n = 16
		}
		if len(b) < 1+n+2 {
			return "", 0, io.ErrUnexpectedEOF
		}
		a, _ := netip.AddrFromSlice(b[1 : 1+n])
		return netip.AddrPortFrom(a.Unmap(), binary.BigEndian.Uint16(b[1+n:])).String(), 1 + n + 2, nil
	case 3:
		if len(b) < 2 || len(b) < 2+int(b[1])+2 {
			return "", 0, io.ErrUnexpectedEOF
		}
		l := int(b[1])
		return net.JoinHostPort(string(b[2:2+l]), strconv.Itoa(int(binary.BigEndian.Uint16(b[2+l:])))), 4 + l, nil
	}
	return "", 0, errors.New("bad address type")
}
