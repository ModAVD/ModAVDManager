package appproxy

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxTCP       = 4096
	sniffWait    = 300 * time.Millisecond
	dnsTCPIdle   = 30 * time.Second
	tcpKeepAlive = 30 * time.Second
)

// counters back Status.Counters.
type counters struct {
	tcp, tcpFailed, udpSessions, udpDropped, dns, dnsFailed, bytesUp, bytesDown atomic.Int64
}

func (c *counters) snapshot() Counters {
	return Counters{TCP: c.tcp.Load(), TCPFailed: c.tcpFailed.Load(), UDPSessions: c.udpSessions.Load(),
		UDPDropped: c.udpDropped.Load(), DNS: c.dns.Load(), DNSFailed: c.dnsFailed.Load(),
		BytesUp: c.bytesUp.Load(), BytesDown: c.bytesDown.Load()}
}

// Core is the daemon's work, apart from the sockets the kernel hands traffic to (tproxy_linux.go): it takes the
// listed apps' connections and datagrams with their original destination and sends them through the proxy.
type Core struct {
	up   *Upstream
	dns  *dnsEngine
	udp  *udpTable
	c    *counters
	log  *log.Logger
	tcpN chan struct{}
	warn throttle // proxy failures, which come in floods when it is down
}

// throttle lets a log line through at most every 10 s, saying how many it held back.
type throttle struct {
	mu      sync.Mutex
	last    time.Time
	dropped int
}

func (t *throttle) printf(lg *log.Logger, format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if time.Since(t.last) < 10*time.Second {
		t.dropped++
		return
	}
	if t.dropped > 0 {
		format += "（之前 10 秒里还有 %d 条类似的没记）"
		args = append(args, t.dropped)
	}
	t.last, t.dropped = time.Now(), 0
	lg.Printf(format, args...)
}

// newCore builds the core for one config. reply sends a datagram to an app from a remote address (a socket bound to
// that address; see udpTable).
func newCore(cfg *Config, c *counters, health *Health, lg *log.Logger, reply ReplyFunc) *Core {
	srv, _ := ParseServer(cfg.Server)
	res, _ := cfg.Resolver()
	up := &Upstream{Server: srv, Health: health}
	co := &Core{up: up, c: c, log: lg, tcpN: make(chan struct{}, maxTCP)}
	co.dns = newDNSEngine(up, res, c)
	co.udp = newUDPTable(co, reply)
	return co
}

// Close ends the UDP associations and the DNS connections (open TCP relays finish on their own).
func (co *Core) Close() {
	co.udp.close()
	co.dns.close()
}

// refused: the destination is not one the kernel would have handed over (loopback, our own listener on 127.0.0.1,
// private addresses), so the daemon is not an open proxy into the guest or the host's loopback.
func (co *Core) refused(dst netip.AddrPort) bool {
	a := dst.Addr().Unmap()
	return !a.Is4() || dst.Port() == 0 || (!publicAddr(a) && dst.Port() != 53)
}

// HandleTCP serves one connection of a listed app; orig is where the app connected to. It owns c.
func (co *Core) HandleTCP(c net.Conn, orig netip.AddrPort) {
	defer c.Close()
	if co.refused(orig) {
		return
	}
	select {
	case co.tcpN <- struct{}{}:
		defer func() { <-co.tcpN }()
	default:
		co.c.tcpFailed.Add(1)
		return
	}
	if orig.Port() == 53 {
		co.serveDNSTCP(c)
		return
	}
	first, t := co.target(c, orig)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	up, pending, err := co.up.DialTCP(ctx, t)
	cancel()
	if err != nil {
		co.c.tcpFailed.Add(1)
		co.warn.printf(co.log, "TCP %s：%v", t, err)
		return
	}
	defer up.Close()
	co.c.tcp.Add(1)
	if len(first) > 0 {
		if _, err := up.Write(first); err != nil {
			return
		}
		co.c.bytesUp.Add(int64(len(first)))
	}
	if len(pending) > 0 {
		if _, err := c.Write(pending); err != nil {
			return
		}
		co.c.bytesDown.Add(int64(len(pending)))
	}
	upN, downN := relay(c, up)
	co.c.bytesUp.Add(upN)
	co.c.bytesDown.Add(downN)
}

// target decides what to ask the proxy for: by name when the first bytes name the server (TLS SNI, HTTP Host) or
// the daemon resolved this address itself, else the address. Web ports wait (up to sniffWait) for the client to
// speak first; elsewhere a name the daemon resolved is used at once, and only failing that it waits. first are the
// bytes read meanwhile.
func (co *Core) target(c net.Conn, orig netip.AddrPort) ([]byte, Target) {
	byName := func(h string) Target { return Target{Host: h, Port: orig.Port()} }
	known := co.dns.nameOf(orig.Addr())
	web := orig.Port() == 443 || orig.Port() == 80 || orig.Port() == 8080 || orig.Port() == 8443
	if known != "" && !web {
		return nil, byName(known)
	}
	first, host := peek(c)
	switch {
	case host != "":
		return first, byName(host)
	case known != "":
		return first, byName(known)
	}
	return first, Target{Addr: orig}
}

// peek reads what the client sends within sniffWait (up to sniffMax bytes) and the server name in it.
func peek(c net.Conn) ([]byte, string) {
	c.SetReadDeadline(time.Now().Add(sniffWait))
	defer c.SetReadDeadline(time.Time{})
	buf := make([]byte, sniffMax)
	n := 0
	for n < len(buf) {
		m, err := c.Read(buf[n:])
		n += m
		host, more := sniffHost(buf[:n])
		if host != "" || !more || err != nil {
			return buf[:n], host
		}
	}
	return buf[:n], ""
}

// relay copies both ways until both sides are done. A side that ends cleanly gets its FIN passed on (half close),
// so protocols that rely on it work; an error on either side ends both. The two are TCPConns, so on Linux the copy
// is splice(2), without going through user space.
func relay(app, up net.Conn) (upN, downN int64) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var err error
		downN, err = io.Copy(app, up)
		finish(app, up, err)
	}()
	upN, err := io.Copy(up, app)
	finish(up, app, err)
	wg.Wait()
	return upN, downN
}

// finish ends the direction into dst: FIN after a clean end, both connections closed after an error.
func finish(dst, src net.Conn, err error) {
	cw, ok := dst.(interface{ CloseWrite() error })
	if err == nil && ok {
		cw.CloseWrite()
		return
	}
	dst.Close()
	src.Close()
}

// serveDNSTCP answers DNS over TCP (two-byte length, then the message), one query after another.
func (co *Core) serveDNSTCP(c net.Conn) {
	var l [2]byte
	for {
		c.SetReadDeadline(time.Now().Add(dnsTCPIdle))
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		q := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		a := co.dns.answer(context.Background(), q, true)
		if a == nil {
			return
		}
		out := binary.BigEndian.AppendUint16(make([]byte, 0, 2+len(a)), uint16(len(a)))
		c.SetWriteDeadline(time.Now().Add(dnsTimeout))
		if _, err := c.Write(append(out, a...)); err != nil {
			return
		}
	}
}

// HandleUDP takes one datagram of a listed app (from client to orig); answers go back through the ReplyFunc. It does
// not keep p.
func (co *Core) HandleUDP(client, orig netip.AddrPort, p []byte) {
	if co.refused(orig) {
		return
	}
	if orig.Port() == 53 {
		q := append([]byte(nil), p...)
		go func() {
			if a := co.dns.answer(context.Background(), q, false); a != nil {
				co.udp.reply(orig, client, a)
			}
		}()
		return
	}
	co.udp.send(client, orig, p)
}
