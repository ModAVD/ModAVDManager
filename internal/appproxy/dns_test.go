package appproxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"modavd/internal/appproxy/testproxy"
)

var guestDNS = netip.MustParseAddrPort("10.0.2.3:53")

func newTestEngine(t *testing.T, srv Server) (*dnsEngine, *counters) {
	c := &counters{}
	e := newDNSEngine(&Upstream{Server: srv, Health: &Health{}}, netip.MustParseAddrPort("1.1.1.1:53"), c)
	t.Cleanup(e.close)
	return e, c
}

func rcodeOf(a []byte) int { return int(binary.BigEndian.Uint16(a[2:]) & 0xf) }

func TestDNSAnswers(t *testing.T) {
	f := newFakeDNS(t, map[string]string{"game.example.com": "1.2.3.4"})
	p := startProxy(t, testproxy.ModeSOCKS5, f.addr, "", nil)
	e, c := newTestEngine(t, serverOf(t, "socks5", p))
	ctx := context.Background()

	// AAAA: empty, without asking anyone
	a := e.answer(ctx, query(1, "game.example.com", dnsTypeAAAA, 0), false)
	if rcodeOf(a) != rcodeNoError || binary.BigEndian.Uint16(a[6:]) != 0 || f.queries.Load() != 0 {
		t.Fatalf("AAAA: %x, %d queries", a, f.queries.Load())
	}

	q := query(0x4242, "game.example.com", dnsTypeA, 0)
	a = e.answer(ctx, q, false)
	info, err := fixAnswer(append([]byte(nil), a...), 0x4242)
	if err != nil || binary.BigEndian.Uint16(a) != 0x4242 || len(info.addrs) != 1 || info.addrs[0].String() != "1.2.3.4" {
		t.Fatalf("A: %x %+v %v", a, info, err)
	}
	if info.minTTL != 0 {
		t.Errorf("TTL %d reached the app (netd would cache it)", info.minTTL)
	}
	// the guest's resolver (private) is replaced by the configured one, through the proxy, over TCP
	if got := targets(p, "connect"); !slices.Equal(got, []string{"1.1.1.1:53"}) {
		t.Errorf("asked %v", got)
	}
	if e.nameOf(netip.MustParseAddr("1.2.3.4")) != "game.example.com" {
		t.Error("name of the address not kept")
	}

	// again, other ID and 0x20 case: from the cache, with the query's ID and question
	q2 := query(0x1111, "GaMe.ExAmPlE.cOm", dnsTypeA, 0)
	a2 := e.answer(ctx, q2, false)
	if f.queries.Load() != 1 {
		t.Errorf("%d queries upstream, want 1 (cached)", f.queries.Load())
	}
	if binary.BigEndian.Uint16(a2) != 0x1111 || string(a2[12:len(q2)]) != string(q2[12:]) {
		t.Errorf("cached answer: %x", a2)
	}

	// NXDOMAIN is cached a short while; SERVFAIL not at all
	if a := e.answer(ctx, query(3, "nx.example", dnsTypeA, 0), false); rcodeOf(a) != rcodeNXDomain {
		t.Errorf("NXDOMAIN: %x", a)
	}
	e.answer(ctx, query(4, "nx.example", dnsTypeA, 0), false)
	e.answer(ctx, query(5, "fail.example", dnsTypeA, 0), false)
	e.answer(ctx, query(6, "fail.example", dnsTypeA, 0), false)
	if got := f.queries.Load(); got != 4 {
		t.Errorf("%d queries upstream, want 4", got)
	}
	if c.dns.Load() != 7 {
		t.Errorf("counted %d queries", c.dns.Load())
	}

	// expiry
	now := time.Now()
	e.now = func() time.Time { return now.Add(dnsMaxTTL + time.Second) }
	e.answer(ctx, query(8, "game.example.com", dnsTypeA, 0), false)
	if f.queries.Load() != 5 {
		t.Errorf("expired entry served: %d queries", f.queries.Load())
	}
	if e.answer(ctx, []byte("junk"), false) != nil {
		t.Error("junk answered")
	}
}

func TestDNSTTLBounds(t *testing.T) {
	f := newFakeDNS(t, map[string]string{"short.example": "1.2.3.4"})
	f.ttl = 1
	p := startProxy(t, testproxy.ModeSOCKS5, f.addr, "", nil)
	e, _ := newTestEngine(t, serverOf(t, "socks5", p))
	now := time.Now()
	e.now = func() time.Time { return now }
	e.answer(context.Background(), query(1, "short.example", dnsTypeA, 0), false)
	e.now = func() time.Time { return now.Add(dnsMinTTL - time.Second) }
	e.answer(context.Background(), query(2, "short.example", dnsTypeA, 0), false)
	if f.queries.Load() != 1 {
		t.Errorf("a TTL of 1 s was not raised to %v", dnsMinTTL)
	}
	// the name of an address outlives a short TTL
	e.now = func() time.Time { return now.Add(namesMinTTL - time.Second) }
	if e.nameOf(netip.MustParseAddr("1.2.3.4")) != "short.example" {
		t.Error("name forgotten")
	}
}

func TestDNSProxyDown(t *testing.T) {
	e, c := newTestEngine(t, deadServer(t))
	a := e.answer(context.Background(), query(1, "game.example.com", dnsTypeA, 0), false)
	if rcodeOf(a) != rcodeServFail || binary.BigEndian.Uint16(a) != 1 {
		t.Errorf("%x", a)
	}
	if c.dnsFailed.Load() != 1 {
		t.Error("not counted")
	}
	st := e.up.Health.Status(e.up.Server)
	if st.TCP != "failed" || st.LastError == "" {
		t.Errorf("%+v", st)
	}
}

// TestDNSPipelined: many queries at once share the two connections and all get their own answer back.
func TestDNSPipelined(t *testing.T) {
	names := map[string]string{}
	for i := 0; i < 64; i++ {
		names[fmt.Sprintf("n%d.example", i)] = fmt.Sprintf("10.9.%d.%d", i/250, i%250+1)
	}
	f := newFakeDNS(t, names)
	f.delay = 20 * time.Millisecond
	p := startProxy(t, testproxy.ModeSOCKS5, f.addr, "", nil)
	e, _ := newTestEngine(t, serverOf(t, "socks5", p))
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name := fmt.Sprintf("n%d.example", i)
			a := e.answer(context.Background(), query(uint16(1000+i), name, dnsTypeA, 0), false)
			info, err := fixAnswer(a, 0)
			if err != nil || binary.BigEndian.Uint16(a) != 0 || len(info.addrs) != 1 || info.addrs[0].String() != names[name] {
				t.Errorf("%s: %+v %v", name, info, err)
			}
		}()
	}
	wg.Wait()
	if n := len(targets(p, "connect")); n > dnsConnsPer+1 {
		t.Errorf("%d connections to the resolver", n)
	}
}

func TestDNSOverUDPTruncates(t *testing.T) {
	f := newFakeDNS(t, nil)
	p := startProxy(t, testproxy.ModeSOCKS5, f.addr, "", nil)
	e, _ := newTestEngine(t, serverOf(t, "socks5", p))
	// a cached answer bigger than 512 bytes
	q := query(1, "big.example", dnsTypeA, 0)
	qd, _ := parseQuery(q)
	var rrs []testRR
	for i := 0; i < 40; i++ {
		rrs = append(rrs, aRR(60, "1.2.3.4"))
	}
	big := answer(q, 0, 0, rrs...)
	info, _ := fixAnswer(big, 1)
	e.store(qd, big, info)
	a := e.answer(context.Background(), q, false)
	if binary.BigEndian.Uint16(a[2:])&flagTC == 0 {
		t.Error("UDP answer not truncated")
	}
	if a := e.answer(context.Background(), q, true); len(a) != len(big) {
		t.Error("TCP answer truncated")
	}
	if a := e.answer(context.Background(), query(1, "big.example", dnsTypeA, 4096), false); len(a) != len(big) {
		t.Error("EDNS size ignored")
	}
}

func TestPublicAddr(t *testing.T) {
	for s, want := range map[string]bool{"1.1.1.1": true, "8.8.8.8": true, "10.0.2.3": false, "127.0.0.1": false,
		"192.168.1.1": false, "172.16.0.1": false, "169.254.1.1": false, "100.64.0.1": false, "0.1.2.3": false,
		"224.0.0.1": false, "255.255.255.255": false, "2606:4700::1111": false} {
		if publicAddr(netip.MustParseAddr(s)) != want {
			t.Errorf("%s", s)
		}
	}
}
