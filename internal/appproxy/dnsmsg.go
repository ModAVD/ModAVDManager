package appproxy

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
)

// DNS messages (RFC 1035), only as far as the daemon needs them: the question of a query, and the TTLs and A records
// of an answer.

const (
	dnsHeaderLen  = 12
	dnsTypeA      = 1
	dnsTypeAAAA   = 28
	dnsTypeOPT    = 41
	dnsClassIN    = 1
	rcodeNoError  = 0
	rcodeServFail = 2
	rcodeNXDomain = 3

	flagQR = 0x8000
	flagTC = 0x0200
	flagRA = 0x0080
)

var errDNSFormat = errors.New("DNS 报文格式不对")

// dnsQuestion is the single question of a query.
type dnsQuestion struct {
	id            uint16
	flags         uint16
	key           string // the name in lower case, wire form, plus type and class: the cache key
	name          string // dotted, lower case; "" when not a plain host name (then never used to connect)
	qtype, qclass uint16
	end           int // offset just past the question
	udpSize       int // largest UDP answer the client takes: EDNS0's size, 512 without
}

// parseQuery reads a standard query with exactly one question.
func parseQuery(b []byte) (dnsQuestion, error) {
	if len(b) < dnsHeaderLen {
		return dnsQuestion{}, errDNSFormat
	}
	q := dnsQuestion{id: binary.BigEndian.Uint16(b), flags: binary.BigEndian.Uint16(b[2:]), udpSize: 512}
	if q.flags&flagQR != 0 || (q.flags>>11)&0xf != 0 || binary.BigEndian.Uint16(b[4:]) != 1 {
		return dnsQuestion{}, errDNSFormat
	}
	wire, name, off, err := readName(b, dnsHeaderLen)
	if err != nil || off+4 > len(b) {
		return dnsQuestion{}, errDNSFormat
	}
	q.qtype, q.qclass = binary.BigEndian.Uint16(b[off:]), binary.BigEndian.Uint16(b[off+2:])
	q.end, q.name = off+4, name
	q.key = wire + string(b[off:off+4])
	// EDNS0: an OPT record among the additional ones gives the client's UDP size in its class field
	an, ns, ar := int(binary.BigEndian.Uint16(b[6:])), int(binary.BigEndian.Uint16(b[8:])), int(binary.BigEndian.Uint16(b[10:]))
	off = q.end
	for i := 0; i < an+ns+ar; i++ {
		rr, err := readRR(b, off)
		if err != nil {
			break
		}
		if i >= an+ns && rr.typ == dnsTypeOPT && int(rr.class) > q.udpSize {
			q.udpSize = min(int(rr.class), 4096)
		}
		off = rr.end
	}
	return q, nil
}

// readName reads the name at off: wire is its labels in lower case (uncompressed, for keys), name the dotted form
// when every label is a host name label (else ""), next the offset after it in place (a pointer ends it).
func readName(b []byte, off int) (wire, name string, next int, err error) {
	var w, d strings.Builder
	plain, next, hops := true, -1, 0
	for {
		if off >= len(b) {
			return "", "", 0, errDNSFormat
		}
		l := int(b[off])
		switch {
		case l == 0:
			w.WriteByte(0)
			if next < 0 {
				next = off + 1
			}
			if !plain || d.Len() > 253 {
				return w.String(), "", next, nil
			}
			return w.String(), d.String(), next, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) || hops > 32 {
				return "", "", 0, errDNSFormat
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
			hops++
		case l&0xc0 != 0:
			return "", "", 0, errDNSFormat
		default:
			if off+1+l > len(b) || w.Len()+l+1 > 255 {
				return "", "", 0, errDNSFormat
			}
			label := strings.ToLower(string(b[off+1 : off+1+l]))
			w.WriteByte(byte(l))
			w.WriteString(label)
			if !hostLabel(label) {
				plain = false
			}
			if d.Len() > 0 {
				d.WriteByte('.')
			}
			d.WriteString(label)
			off += 1 + l
		}
	}
}

// hostLabel: letters, digits, hyphens and underscores (seen in real names), not starting or ending with a hyphen.
func hostLabel(s string) bool {
	if s == "" || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// skipName returns the offset after the name at off.
func skipName(b []byte, off int) (int, error) {
	for {
		if off >= len(b) {
			return 0, errDNSFormat
		}
		l := int(b[off])
		switch {
		case l == 0:
			return off + 1, nil
		case l&0xc0 == 0xc0:
			if off+1 >= len(b) {
				return 0, errDNSFormat
			}
			return off + 2, nil
		case l&0xc0 != 0:
			return 0, errDNSFormat
		}
		off += 1 + l
	}
}

// rr is one resource record's fixed fields.
type rr struct {
	typ, class uint16
	ttlAt      int // offset of the 4-byte TTL
	data, end  int // offset of RDATA, and just past it
}

func readRR(b []byte, off int) (rr, error) {
	off, err := skipName(b, off)
	if err != nil || off+10 > len(b) {
		return rr{}, errDNSFormat
	}
	r := rr{typ: binary.BigEndian.Uint16(b[off:]), class: binary.BigEndian.Uint16(b[off+2:]), ttlAt: off + 4, data: off + 10}
	r.end = r.data + int(binary.BigEndian.Uint16(b[off+8:]))
	if r.end > len(b) {
		return rr{}, errDNSFormat
	}
	return r, nil
}

// answerInfo is what fixAnswer found.
type answerInfo struct {
	rcode   int
	answers int          // records in the answer section
	minTTL  uint32       // over all records but OPT; 0 when there are none
	addrs   []netip.Addr // A records of the answer section
}

// fixAnswer sets the ID of an answer to id and every TTL (except OPT's, which is not one) to 0, so that netd does not
// cache it: the cache is shared with the apps that go direct.
func fixAnswer(b []byte, id uint16) (answerInfo, error) {
	if len(b) < dnsHeaderLen || binary.BigEndian.Uint16(b[2:])&flagQR == 0 {
		return answerInfo{}, errDNSFormat
	}
	binary.BigEndian.PutUint16(b, id)
	info := answerInfo{rcode: int(binary.BigEndian.Uint16(b[2:]) & 0xf)}
	qd := int(binary.BigEndian.Uint16(b[4:]))
	an, ns, ar := int(binary.BigEndian.Uint16(b[6:])), int(binary.BigEndian.Uint16(b[8:])), int(binary.BigEndian.Uint16(b[10:]))
	off := dnsHeaderLen
	for i := 0; i < qd; i++ {
		n, err := skipName(b, off)
		if err != nil || n+4 > len(b) {
			return answerInfo{}, errDNSFormat
		}
		off = n + 4
	}
	info.answers = an
	first := true
	for i := 0; i < an+ns+ar; i++ {
		r, err := readRR(b, off)
		if err != nil {
			return answerInfo{}, err
		}
		if r.typ != dnsTypeOPT {
			if ttl := binary.BigEndian.Uint32(b[r.ttlAt:]); first || ttl < info.minTTL {
				info.minTTL, first = ttl, false
			}
			binary.BigEndian.PutUint32(b[r.ttlAt:], 0)
		}
		if i < an && r.typ == dnsTypeA && r.class == dnsClassIN && r.end-r.data == 4 {
			info.addrs = append(info.addrs, netip.AddrFrom4([4]byte(b[r.data:r.end])))
		}
		off = r.end
	}
	return info, nil
}

// emptyAnswer answers query (whose question q is) with rcode and no records.
func emptyAnswer(query []byte, q dnsQuestion, rcode int) []byte {
	b := append([]byte(nil), query[:q.end]...)
	flags := q.flags&0x7900 | flagQR | flagRA | uint16(rcode) // keep opcode and RD
	binary.BigEndian.PutUint16(b[2:], flags)
	binary.BigEndian.PutUint16(b[4:], 1)
	clear(b[6:12])
	return b
}

// fitUDP returns answer as it may go back over UDP to a client that takes size bytes: whole, or (too long) just the
// header and question with TC set, so that the client asks again over TCP.
func fitUDP(answer, query []byte, q dnsQuestion, size int) []byte {
	if len(answer) <= size {
		return answer
	}
	b := append(append([]byte(nil), answer[:dnsHeaderLen]...), query[dnsHeaderLen:q.end]...)
	binary.BigEndian.PutUint16(b[2:], binary.BigEndian.Uint16(b[2:])|flagTC)
	binary.BigEndian.PutUint16(b[4:], 1)
	clear(b[6:12])
	return b
}
