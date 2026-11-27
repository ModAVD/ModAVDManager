package appproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"
)

// The listed apps' DNS: netd sends their queries (with the app's uid on the socket), or the apps send their own;
// either way they come to the daemon, which answers them:
//
//   - AAAA: an empty answer, so the apps use IPv4 (their IPv6 is blocked, rules.go).
//   - Otherwise from its own cache, or by asking Config.DNS over TCP through the proxy, whichever resolver the query
//     was sent to: the guest's own 10.0.2.3 cannot be reached from the proxy, and the cache is shared by the listed
//     apps, so an answer from a resolver one app picked (one it runs) would reach the others. TCP so that DNS works
//     with proxies that do not relay UDP; queries are pipelined on up to two connections.
//   - Every answer goes back with TTL 0, so that netd's cache, which the apps going direct share, keeps none of
//     them. The other way round cannot be helped: an answer netd cached for an app going direct reaches the listed
//     apps without asking the daemon. The TCP relay makes up for most of it by connecting by name (relay.go).
//   - The proxy unreachable: SERVFAIL.
const (
	dnsTimeout     = 5 * time.Second
	dnsMaxInFlight = 256
	dnsMinTTL      = 30 * time.Second
	dnsMaxTTL      = time.Hour
	dnsNegativeTTL = 30 * time.Second
	dnsCacheMax    = 4096
	namesMax       = 16384
	namesMinTTL    = 10 * time.Minute
	dnsConnsPer    = 2
	dnsPoolIdle    = 2 * time.Minute
)

type cacheEntry struct {
	answer  []byte // TTLs already 0
	expires time.Time
}

type nameEntry struct {
	name    string
	expires time.Time
}

type dnsEngine struct {
	up       *Upstream
	resolver netip.AddrPort
	c        *counters
	now      func() time.Time

	sem chan struct{}

	mu    sync.Mutex
	cache map[string]cacheEntry
	names map[netip.Addr]nameEntry // A records the daemon handed out: the name asked for
	pools map[netip.AddrPort]*dnsPool
}

func newDNSEngine(up *Upstream, resolver netip.AddrPort, c *counters) *dnsEngine {
	return &dnsEngine{up: up, resolver: resolver, c: c, now: time.Now, sem: make(chan struct{}, dnsMaxInFlight),
		cache: map[string]cacheEntry{}, names: map[netip.Addr]nameEntry{}, pools: map[netip.AddrPort]*dnsPool{}}
}

// answer answers query (over TCP, or UDP: then cut to the client's UDP size), wherever it was sent. nil: not a query,
// drop it.
func (e *dnsEngine) answer(ctx context.Context, query []byte, overTCP bool) []byte {
	q, err := parseQuery(query)
	if err != nil {
		return nil
	}
	e.c.dns.Add(1)
	size := q.udpSize
	if overTCP {
		size = 65535
	}
	if q.qtype == dnsTypeAAAA {
		return emptyAnswer(query, q, rcodeNoError)
	}
	if a := e.cached(query, q); a != nil {
		return fitUDP(a, query, q, size)
	}
	select {
	case e.sem <- struct{}{}:
		defer func() { <-e.sem }()
	default:
		e.c.dnsFailed.Add(1)
		return emptyAnswer(query, q, rcodeServFail)
	}
	a, err := e.ask(ctx, e.resolver, query)
	var info answerInfo
	if err == nil {
		info, err = fixAnswer(a, q.id)
	}
	if err != nil {
		e.c.dnsFailed.Add(1)
		return emptyAnswer(query, q, rcodeServFail)
	}
	e.store(q, a, info)
	return fitUDP(a, query, q, size)
}

// cached is a copy of the cached answer with the query's ID and question (the client may use 0x20 case), or nil.
func (e *dnsEngine) cached(query []byte, q dnsQuestion) []byte {
	e.mu.Lock()
	ent, ok := e.cache[q.key]
	e.mu.Unlock()
	if !ok || !e.now().Before(ent.expires) {
		return nil
	}
	a := append([]byte(nil), ent.answer...)
	binary.BigEndian.PutUint16(a, q.id)
	if len(a) >= q.end {
		copy(a[dnsHeaderLen:q.end], query[dnsHeaderLen:q.end])
	}
	return a
}

func (e *dnsEngine) store(q dnsQuestion, a []byte, info answerInfo) {
	var ttl time.Duration
	switch {
	case info.rcode == rcodeNoError && info.answers > 0:
		ttl = min(max(time.Duration(info.minTTL)*time.Second, dnsMinTTL), dnsMaxTTL)
	case info.rcode == rcodeNoError || info.rcode == rcodeNXDomain:
		ttl = dnsNegativeTTL
	default:
		return
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.cache) >= dnsCacheMax {
		prune(e.cache, now, func(c cacheEntry) time.Time { return c.expires })
	}
	e.cache[q.key] = cacheEntry{answer: append([]byte(nil), a...), expires: now.Add(ttl)}
	if q.name == "" {
		return
	}
	if len(e.names)+len(info.addrs) > namesMax {
		prune(e.names, now, func(n nameEntry) time.Time { return n.expires })
	}
	for _, ip := range info.addrs {
		e.names[ip] = nameEntry{name: q.name, expires: now.Add(max(ttl, namesMinTTL))}
	}
}

// prune drops the expired entries of m, and a quarter of the rest when that is not enough.
func prune[K comparable, V any](m map[K]V, now time.Time, expires func(V) time.Time) {
	for k, v := range m {
		if !now.Before(expires(v)) {
			delete(m, k)
		}
	}
	drop := len(m) / 4
	for k := range m {
		if drop <= 0 {
			break
		}
		delete(m, k)
		drop--
	}
}

// nameOf is the name the daemon resolved to ip, "" when none (or long ago).
func (e *dnsEngine) nameOf(ip netip.Addr) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	n, ok := e.names[ip]
	if !ok || !e.now().Before(n.expires) {
		return ""
	}
	return n.name
}

// publicAddr: an address the proxy can reach for the app (not loopback, private, link-local, CGNAT or the like).
func publicAddr(a netip.Addr) bool {
	a = a.Unmap()
	if !a.Is4() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified() || a.IsMulticast() {
		return false
	}
	return !cgnat.Contains(a) && !thisNet.Contains(a) && !reserved.Contains(a)
}

var (
	cgnat    = netip.MustParsePrefix("100.64.0.0/10")
	thisNet  = netip.MustParsePrefix("0.0.0.0/8")
	reserved = netip.MustParsePrefix("240.0.0.0/4")
)

// ask sends query to server over TCP through the proxy, once more on another connection if the first fails.
func (e *dnsEngine) ask(ctx context.Context, server netip.AddrPort, query []byte) ([]byte, error) {
	e.mu.Lock()
	p := e.pools[server]
	if p == nil {
		e.closeIdlePools()
		p = newDNSPool(e, server)
		e.pools[server] = p
	}
	e.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var c *dnsConn
		if c, err = p.conn(ctx, attempt > 0); err != nil {
			continue
		}
		var a []byte
		if a, err = c.exchange(ctx, query); err == nil {
			return a, nil
		}
	}
	return nil, err
}

// closeIdlePools drops the pools of resolvers not asked for a while (apps that picked their own). Under e.mu.
func (e *dnsEngine) closeIdlePools() {
	for k, p := range e.pools {
		if k != e.resolver && p.idleSince(e.now()) > dnsPoolIdle {
			p.close()
			delete(e.pools, k)
		}
	}
}

// close ends every upstream connection (on reload).
func (e *dnsEngine) close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, p := range e.pools {
		p.close()
		delete(e.pools, k)
	}
}

// dnsPool holds up to dnsConnsPer pipelined connections to one resolver.
type dnsPool struct {
	e       *dnsEngine
	server  netip.AddrPort
	dialing chan struct{} // one dial at a time: the queries waiting meanwhile use its connection

	mu    sync.Mutex
	conns []*dnsConn
	last  time.Time
}

func newDNSPool(e *dnsEngine, server netip.AddrPort) *dnsPool {
	return &dnsPool{e: e, server: server, dialing: make(chan struct{}, 1)}
}

// conn returns a live connection: the least busy one, or a new one when fresh (a retry) or when all are busy and
// there is room for another.
func (p *dnsPool) conn(ctx context.Context, fresh bool) (*dnsConn, error) {
	if c := p.pick(fresh, false); c != nil {
		return c, nil
	}
	select {
	case p.dialing <- struct{}{}:
		defer func() { <-p.dialing }()
	case <-ctx.Done():
		return nil, errors.New("DNS 查询超时")
	}
	if c := p.pick(fresh, false); c != nil {
		return c, nil // dialed by another query meanwhile
	}
	nc, _, err := p.e.up.DialTCP(ctx, Target{Addr: p.server})
	if err != nil {
		if c := p.pick(false, true); c != nil {
			return c, nil
		}
		return nil, err
	}
	c := newDNSConn(nc)
	p.mu.Lock()
	p.conns = append(p.conns, c)
	for len(p.conns) > dnsConnsPer {
		p.conns[0].close(errors.New("连接太多"))
		p.conns = p.conns[1:]
	}
	p.mu.Unlock()
	return c, nil
}

// pick is the least busy live connection, or nil when a new one should be dialed (fresh; or all busy with room for
// another, unless anyLive).
func (p *dnsPool) pick(fresh, anyLive bool) *dnsConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = p.e.now()
	live := p.conns[:0]
	for _, c := range p.conns {
		if !c.isDead() {
			live = append(live, c)
		}
	}
	clear(p.conns[len(live):])
	p.conns = live
	var best *dnsConn
	for _, c := range live {
		if best == nil || c.pending() < best.pending() {
			best = c
		}
	}
	if best == nil || anyLive {
		return best
	}
	if fresh || (best.pending() >= 8 && len(live) < dnsConnsPer) {
		return nil
	}
	return best
}

func (p *dnsPool) idleSince(now time.Time) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return now.Sub(p.last)
}

func (p *dnsPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.close(errors.New("已关闭"))
	}
	p.conns = nil
}

// dnsConn is one TCP connection to a resolver; queries get IDs of its own and are matched back by them.
type dnsConn struct {
	c   net.Conn
	wmu sync.Mutex

	mu      sync.Mutex
	next    uint16
	waiting map[uint16]chan []byte
	err     error // set once dead
}

func newDNSConn(c net.Conn) *dnsConn {
	d := &dnsConn{c: c, waiting: map[uint16]chan []byte{}}
	go d.read()
	return d
}

func (d *dnsConn) read() {
	var l [2]byte
	for {
		if _, err := io.ReadFull(d.c, l[:]); err != nil {
			d.close(err)
			return
		}
		b := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(d.c, b); err != nil {
			d.close(err)
			return
		}
		if len(b) < dnsHeaderLen {
			continue
		}
		d.mu.Lock()
		ch := d.waiting[binary.BigEndian.Uint16(b)]
		delete(d.waiting, binary.BigEndian.Uint16(b))
		d.mu.Unlock()
		if ch != nil {
			ch <- b
		}
	}
}

func (d *dnsConn) exchange(ctx context.Context, query []byte) ([]byte, error) {
	ch := make(chan []byte, 1)
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return nil, d.err
	}
	if len(d.waiting) >= 1024 {
		d.mu.Unlock()
		return nil, errors.New("DNS 连接上等着的查询太多")
	}
	id := d.next
	for d.waiting[id] != nil {
		id++
	}
	d.next = id + 1
	d.waiting[id] = ch
	d.mu.Unlock()

	msg := make([]byte, 2+len(query))
	binary.BigEndian.PutUint16(msg, uint16(len(query)))
	copy(msg[2:], query)
	binary.BigEndian.PutUint16(msg[2:], id)
	d.wmu.Lock()
	d.c.SetWriteDeadline(time.Now().Add(dnsTimeout))
	_, err := d.c.Write(msg)
	d.wmu.Unlock()
	if err != nil {
		d.close(err)
		return nil, err
	}
	select {
	case a, ok := <-ch:
		if !ok {
			return nil, d.deadErr()
		}
		return a, nil
	case <-ctx.Done():
		d.mu.Lock()
		delete(d.waiting, id)
		d.mu.Unlock()
		return nil, errors.New("DNS 查询超时")
	}
}

func (d *dnsConn) pending() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.waiting)
}

func (d *dnsConn) isDead() bool { return d.deadErr() != nil }

func (d *dnsConn) deadErr() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

// close marks the connection dead and wakes every waiting query.
func (d *dnsConn) close(err error) {
	d.mu.Lock()
	if d.err != nil {
		d.mu.Unlock()
		return
	}
	if err == nil {
		err = io.EOF
	}
	d.err = err
	w := d.waiting
	d.waiting = map[uint16]chan []byte{}
	d.mu.Unlock()
	d.c.Close()
	for _, ch := range w {
		close(ch)
	}
}
