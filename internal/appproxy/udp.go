package appproxy

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// ReplyFunc sends p to an app (to) from the address it talked to (from). In the guest that is a transparent socket
// bound to from (tproxy_linux.go).
type ReplyFunc func(from, to netip.AddrPort, p []byte) error

const (
	udpIdle    = 2 * time.Minute
	udpMax     = 256 // sessions; the one idle longest goes first
	udpQueue   = 64  // datagrams waiting for an association
	udpNoRetry = 30 * time.Second
	udpBufSize = 65535
)

// udpTable gives every app socket (its address and port) a SOCKS5 UDP association of its own, made on its first
// datagram: whatever it sends, to any destination, goes through that association, and whatever comes back, from
// any source, is passed back to it as coming from that source (full cone). Destinations are sent as addresses, not
// names: the app's socket only takes answers from the address it sent to.
type udpTable struct {
	co    *Core
	reply ReplyFunc

	mu       sync.Mutex
	sessions map[netip.AddrPort]*udpSession
	noUDP    time.Time // the proxy refused UDP: drop without asking again until then
	closed   bool
	stop     chan struct{}
}

type udpSession struct {
	client netip.AddrPort
	in     chan udpPacket
	last   atomic.Int64 // unix nanoseconds of the last datagram either way
	done   chan struct{}
	once   sync.Once
}

type udpPacket struct {
	dst netip.AddrPort
	p   []byte
}

func newUDPTable(co *Core, reply ReplyFunc) *udpTable {
	t := &udpTable{co: co, reply: reply, sessions: map[netip.AddrPort]*udpSession{}, stop: make(chan struct{})}
	go t.reap()
	return t
}

func (s *udpSession) touch() { s.last.Store(time.Now().UnixNano()) }
func (s *udpSession) end()   { s.once.Do(func() { close(s.done) }) }
func (s *udpSession) idle() time.Duration {
	return time.Since(time.Unix(0, s.last.Load()))
}

// send passes one datagram of client to dst on (p is copied).
func (t *udpTable) send(client, dst netip.AddrPort, p []byte) {
	t.mu.Lock()
	if t.closed || time.Now().Before(t.noUDP) {
		t.mu.Unlock()
		t.co.c.udpDropped.Add(1)
		return
	}
	s := t.sessions[client]
	if s == nil {
		if len(t.sessions) >= udpMax {
			t.evictLocked()
		}
		s = &udpSession{client: client, in: make(chan udpPacket, udpQueue), done: make(chan struct{})}
		s.touch()
		t.sessions[client] = s
		t.co.c.udpSessions.Add(1)
		go t.run(s)
	}
	t.mu.Unlock()
	s.touch()
	select {
	case s.in <- udpPacket{dst: dst, p: append([]byte(nil), p...)}:
	default:
		t.co.c.udpDropped.Add(1)
	}
}

// evictLocked ends the session idle longest. Under t.mu.
func (t *udpTable) evictLocked() {
	var oldest *udpSession
	for _, s := range t.sessions {
		if oldest == nil || s.last.Load() < oldest.last.Load() {
			oldest = s
		}
	}
	if oldest != nil {
		delete(t.sessions, oldest.client)
		oldest.end()
	}
}

func (t *udpTable) remove(s *udpSession) {
	t.mu.Lock()
	if t.sessions[s.client] == s {
		delete(t.sessions, s.client)
	}
	t.mu.Unlock()
	s.end()
}

// run makes the association, then forwards until the session ends.
func (t *udpTable) run(s *udpSession) {
	defer t.remove(s)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	a, err := t.co.up.Associate(ctx)
	cancel()
	if err != nil {
		if errors.Is(err, errNoUDP) {
			t.mu.Lock()
			t.noUDP = time.Now().Add(udpNoRetry)
			t.mu.Unlock()
		}
		t.co.warn.printf(t.co.log, "UDP：%v", err)
		t.co.c.udpDropped.Add(int64(len(s.in)))
		return
	}
	defer a.Close()
	go func() {
		defer s.end()
		buf := make([]byte, udpBufSize)
		for {
			p, from, err := a.ReadFrom(buf)
			if err != nil {
				return
			}
			if from.Host != "" || !from.Addr.Addr().Unmap().Is4() {
				continue
			}
			from.Addr = netip.AddrPortFrom(from.Addr.Addr().Unmap(), from.Addr.Port())
			if t.reply(from.Addr, s.client, p) == nil {
				t.co.c.bytesDown.Add(int64(len(p)))
				s.touch()
			}
		}
	}()
	for {
		select {
		case pk := <-s.in:
			if err := a.WriteTo(pk.p, Target{Addr: pk.dst}); err != nil {
				t.co.c.udpDropped.Add(1)
				return
			}
			t.co.c.bytesUp.Add(int64(len(pk.p)))
		case <-s.done:
			return
		}
	}
}

// reap ends sessions idle for udpIdle.
func (t *udpTable) reap() {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-t.stop:
			return
		case <-tick.C:
		}
		t.mu.Lock()
		for k, s := range t.sessions {
			if s.idle() > udpIdle {
				delete(t.sessions, k)
				s.end()
			}
		}
		t.mu.Unlock()
	}
}

func (t *udpTable) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	close(t.stop)
	for k, s := range t.sessions {
		delete(t.sessions, k)
		s.end()
	}
}
