package appproxy

import (
	"encoding/binary"
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

// query builds a standard query; ednsSize > 0 adds an OPT record with that UDP size.
func query(id uint16, name string, qtype uint16, ednsSize int) []byte {
	b := binary.BigEndian.AppendUint16(nil, id)
	ar := 0
	if ednsSize > 0 {
		ar = 1
	}
	b = append(b, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, byte(ar)) // RD
	b = appendName(b, name)
	b = binary.BigEndian.AppendUint16(b, qtype)
	b = binary.BigEndian.AppendUint16(b, dnsClassIN)
	if ednsSize > 0 {
		b = append(b, 0)
		b = binary.BigEndian.AppendUint16(b, dnsTypeOPT)
		b = binary.BigEndian.AppendUint16(b, uint16(ednsSize))
		b = append(b, 0, 0, 0, 0, 0, 0)
	}
	return b
}

func appendName(b []byte, name string) []byte {
	for _, l := range strings.Split(name, ".") {
		if l != "" {
			b = append(append(b, byte(len(l))), l...)
		}
	}
	return append(b, 0)
}

// testRR is a record for answer(); its name is a pointer to the question's.
type testRR struct {
	typ  uint16
	ttl  uint32
	data []byte
}

// answer answers q (a query() without EDNS) with records, an OPT with ttl field optTTL at the end.
func answer(q []byte, rcode int, optTTL uint32, rrs ...testRR) []byte {
	qd, _ := parseQuery(q)
	b := append([]byte(nil), q[:qd.end]...)
	binary.BigEndian.PutUint16(b[2:], 0x8180|uint16(rcode))
	binary.BigEndian.PutUint16(b[6:], uint16(len(rrs)))
	binary.BigEndian.PutUint16(b[8:], 0)
	binary.BigEndian.PutUint16(b[10:], 1)
	for _, r := range rrs {
		b = append(b, 0xc0, 0x0c)
		b = binary.BigEndian.AppendUint16(b, r.typ)
		b = binary.BigEndian.AppendUint16(b, dnsClassIN)
		b = binary.BigEndian.AppendUint32(b, r.ttl)
		b = binary.BigEndian.AppendUint16(b, uint16(len(r.data)))
		b = append(b, r.data...)
	}
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, dnsTypeOPT)
	b = binary.BigEndian.AppendUint16(b, 1232)
	b = binary.BigEndian.AppendUint32(b, optTTL)
	return binary.BigEndian.AppendUint16(b, 0)
}

func aRR(ttl uint32, ip string) testRR {
	a := netip.MustParseAddr(ip).As4()
	return testRR{typ: dnsTypeA, ttl: ttl, data: a[:]}
}

func TestParseQuery(t *testing.T) {
	q, err := parseQuery(query(0x1234, "Game.Example.com", dnsTypeA, 1232))
	if err != nil {
		t.Fatal(err)
	}
	if q.id != 0x1234 || q.name != "game.example.com" || q.qtype != dnsTypeA || q.qclass != dnsClassIN || q.udpSize != 1232 {
		t.Errorf("%+v", q)
	}
	q2, _ := parseQuery(query(1, "GAME.example.COM", dnsTypeA, 0))
	if q2.key != q.key || q2.udpSize != 512 {
		t.Errorf("0x20 case gives another key, or no EDNS is not 512: %+v", q2)
	}
	q3, _ := parseQuery(query(1, "game.example.com", dnsTypeAAAA, 0))
	if q3.key == q.key {
		t.Error("type not in the key")
	}
	if q, _ := parseQuery(query(1, "x.example", dnsTypeA, 9000)); q.udpSize != 4096 {
		t.Errorf("EDNS size capped: %d", q.udpSize)
	}
	weird := query(1, "a.b", dnsTypeA, 0)
	weird[13] = '.' // the label "a" becomes "."
	if q, err := parseQuery(weird); err != nil || q.name != "" {
		t.Errorf("label with a dot: %+v %v", q, err)
	}
	for name, b := range map[string][]byte{
		"short":    {1, 2, 3},
		"response": answer(query(1, "a.b", dnsTypeA, 0), 0, 0),
		"two qs":   func() []byte { b := query(1, "a.b", dnsTypeA, 0); b[5] = 2; return b }(),
		"cut":      query(1, "a.b", dnsTypeA, 0)[:15],
		"loop":     append(query(1, "", dnsTypeA, 0)[:12], 0xc0, 12, 0, 1, 0, 1),
	} {
		if _, err := parseQuery(b); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestFixAnswer(t *testing.T) {
	q := query(7, "game.example.com", dnsTypeA, 0)
	cname := testRR{typ: 5, ttl: 100, data: appendName(nil, "edge.cdn.example")}
	a := answer(q, rcodeNoError, 0x8000, cname, aRR(300, "1.2.3.4"), aRR(60, "5.6.7.8"))
	info, err := fixAnswer(a, 0xbeef)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Addr{netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("5.6.7.8")}
	if info.minTTL != 60 || info.answers != 3 || info.rcode != 0 || !reflect.DeepEqual(info.addrs, want) {
		t.Errorf("%+v", info)
	}
	if binary.BigEndian.Uint16(a) != 0xbeef {
		t.Error("id")
	}
	// every TTL is 0 now, the OPT's (EDNS flags, not a TTL) untouched
	off := len(q)
	for i := 0; i < 4; i++ {
		r, err := readRR(a, off)
		if err != nil {
			t.Fatal(err)
		}
		ttl := binary.BigEndian.Uint32(a[r.ttlAt:])
		if (r.typ == dnsTypeOPT && ttl != 0x8000) || (r.typ != dnsTypeOPT && ttl != 0) {
			t.Errorf("record %d type %d TTL %#x", i, r.typ, ttl)
		}
		off = r.end
	}
	if _, err := fixAnswer(q, 1); err == nil {
		t.Error("a query taken for an answer")
	}
	if _, err := fixAnswer(a[:len(a)-3], 1); err == nil {
		t.Error("cut answer")
	}
}

func TestEmptyAnswerAndFitUDP(t *testing.T) {
	q := query(9, "v6.example", dnsTypeAAAA, 0)
	qd, _ := parseQuery(q)
	e := emptyAnswer(q, qd, rcodeServFail)
	flags := binary.BigEndian.Uint16(e[2:])
	if binary.BigEndian.Uint16(e) != 9 || flags&flagQR == 0 || flags&0x0100 == 0 || flags&0xf != rcodeServFail ||
		binary.BigEndian.Uint16(e[6:]) != 0 || len(e) != qd.end {
		t.Errorf("%x", e)
	}
	q = query(9, "big.example", dnsTypeA, 0)
	qd, _ = parseQuery(q)
	var rrs []testRR
	for i := 0; i < 40; i++ {
		rrs = append(rrs, aRR(60, "1.2.3.4"))
	}
	big := answer(q, 0, 0, rrs...)
	if got := fitUDP(big, q, qd, 4096); len(got) != len(big) {
		t.Error("fits, cut anyway")
	}
	cut := fitUDP(big, q, qd, 512)
	if len(cut) != qd.end || binary.BigEndian.Uint16(cut[2:])&flagTC == 0 || binary.BigEndian.Uint16(cut[6:]) != 0 {
		t.Errorf("cut %x", cut)
	}
}
