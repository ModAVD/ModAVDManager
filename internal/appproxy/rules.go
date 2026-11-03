package appproxy

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// How the listed apps' packets reach the daemon (all IPv4; their IPv6 is refused).
//
// An app's own sockets: mangle OUTPUT matches the uid (-m owner) and sets Mark; the kernel routes the packet again
// with the mark, rule prefMark sends it to table Table, whose only route is "local default dev lo", and back in on
// lo, PREROUTING's TPROXY gives it to the daemon's socket on 127.0.0.1 with the original destination kept. Private,
// loopback and multicast destinations stay direct; protocols other than TCP and UDP (ICMP) are dropped.
//
// DNS that netd sends for an app: netd opens that socket, so -m owner (which looks at the opener) does not see the
// app; netd sets the app's uid on it (sk_uid), which only routing rules look at. Rules prefDNSUDP and prefDNSTCP
// route the app's port-53 traffic to Table directly; OUTPUT then marks port-53 packets leaving on lo, and TPROXY takes
// them as above. The route's src 127.0.0.1 gives netd's unbound socket an address the daemon can answer.
//
// The daemon's own traffic (uid 0) is not listed, so it goes out as usual (the WiFi fast path included). Its answers
// to the apps carry the original addresses; they go to local addresses over lo unmarked and are delivered normally.
//
// The preferences come after the local table's (0) and before netd's (10000 and up) and the WiFi fast path (15000).
// Everything is in chains of our own, entered from the first place of OUTPUT and PREROUTING; netd's chains are not
// touched. Mark is above the 21 low bits netd uses for its fwmark (network id, permissions, billing) and that its
// rules mask, and outside the 0xfffff of its CONNMARK save and restore.
const (
	Mark = 0x1000000
	// ReplyMark is on the daemon's TCP listener and so on what it sends to the apps (tproxy_linux.go listenTCP): any
	// mark without Mark's bit; those packets go to local addresses, which the local table routes before any rule
	// looks at marks.
	ReplyMark = 0x4000000
	Table     = 8990
	TCPPort   = 18990 // on 127.0.0.1 in the guest
	UDPPort   = 18990

	prefMark   = 8990
	prefDNSUDP = 8991
	prefDNSTCP = 8992
	prefV6     = 8993

	chainOut = "modavd_proxy_out"
	chainApp = "modavd_proxy_app"
	chainPre = "modavd_proxy_pre"
)

var markText = fmt.Sprintf("%#x/%#x", Mark, Mark)

// directV4 are the destinations the listed apps reach directly (DNS to them excepted, caught by port before).
var directV4 = []string{"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.168.0.0/16", "224.0.0.0/3"}

// restoreInput is the `iptables-restore --noflush` input that builds the three chains for these uids. Declaring a
// chain flushes it when it exists, so one commit replaces the old rules with no gap.
func restoreInput(uids []UIDRange) string {
	var b strings.Builder
	b.WriteString("*mangle\n")
	for _, c := range []string{chainOut, chainApp, chainPre} {
		b.WriteString(":" + c + " - [0:0]\n")
	}
	for _, r := range chainRules(uids) {
		b.WriteString(r + "\n")
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

// chainRules are the -A lines of our chains, as iptables-save prints them back (roughly: only counted).
func chainRules(uids []UIDRange) []string {
	mark := "-j MARK --set-xmark " + markText
	r := []string{
		"-A " + chainOut + " -d 127.0.0.0/8 -o lo -j RETURN",
		"-A " + chainOut + " -o lo -p udp -m udp --dport 53 " + mark,
		"-A " + chainOut + " -o lo -p tcp -m tcp --dport 53 " + mark,
	}
	for _, u := range uids {
		r = append(r, "-A "+chainOut+" -m owner --uid-owner "+u.String()+" -j "+chainApp)
	}
	for _, d := range directV4 {
		r = append(r, "-A "+chainApp+" -d "+d+" -j RETURN")
	}
	// MARK does not end the chain: marked packets leave by the RETURN, what is left (ICMP) meets the DROP
	r = append(r,
		"-A "+chainApp+" -p tcp "+mark,
		"-A "+chainApp+" -p udp "+mark,
		"-A "+chainApp+" -m mark --mark "+markText+" -j RETURN",
		"-A "+chainApp+" -j DROP",
	)
	for _, p := range []struct {
		proto string
		port  int
	}{{"tcp", TCPPort}, {"udp", UDPPort}} {
		r = append(r, fmt.Sprintf("-A %s -i lo -p %s -m mark --mark %s -j TPROXY --on-port %d --on-ip 127.0.0.1 --tproxy-mark %s",
			chainPre, p.proto, markText, p.port, markText))
	}
	return r
}

// jumps lead from the built-in chains to ours (iptables arguments after the -C/-I/-D and before -j's target).
var jumps = []struct{ builtin, chain string }{{"OUTPUT", chainOut}, {"PREROUTING", chainPre}}

// mangleState is what iptables-save -t mangle says about our part.
type mangleState struct {
	jumps int // how many of the jumps are there
	rules int // -A lines in our chains
}

func parseMangle(save string) mangleState {
	var s mangleState
	for _, l := range strings.Split(save, "\n") {
		l = strings.TrimSpace(l)
		for _, j := range jumps {
			if l == "-A "+j.builtin+" -j "+j.chain {
				s.jumps++
			}
		}
		if strings.HasPrefix(l, "-A modavd_proxy_") {
			s.rules++
		}
	}
	return s
}

// ipRule is one of the routing rules.
type ipRule struct {
	v6   bool
	pref int
	uids UIDRange // the fwmark rule has none
}

func (r ipRule) family() string {
	if r.v6 {
		return "-6"
	}
	return "-4"
}

// addArgs and delArgs follow `ip -4|-6 rule`.
func (r ipRule) addArgs() []string {
	a := []string{"add", "pref", strconv.Itoa(r.pref)}
	switch r.pref {
	case prefMark:
		return append(a, "fwmark", markText, "lookup", strconv.Itoa(Table))
	case prefDNSUDP, prefDNSTCP:
		proto := "udp"
		if r.pref == prefDNSTCP {
			proto = "tcp"
		}
		return append(a, "uidrange", r.uids.String(), "ipproto", proto, "dport", "53", "lookup", strconv.Itoa(Table))
	}
	return append(a, "uidrange", r.uids.String(), "prohibit")
}

func (r ipRule) delArgs() []string {
	a := []string{"del", "pref", strconv.Itoa(r.pref)}
	if r.pref != prefMark {
		a = append(a, "uidrange", r.uids.String())
	}
	return a
}

// wantRules are the routing rules for these uids.
func wantRules(uids []UIDRange) []ipRule {
	r := []ipRule{{pref: prefMark}}
	for _, u := range uids {
		r = append(r, ipRule{pref: prefDNSUDP, uids: u}, ipRule{pref: prefDNSTCP, uids: u})
	}
	for _, u := range uids {
		r = append(r, ipRule{v6: true, pref: prefV6, uids: u})
	}
	return r
}

func ourPref(p int) bool { return p >= prefMark && p <= prefV6 }

// parseRules picks ours out of `ip -4|-6 rule show`, lines like "8991:	from all uidrange 10123-10125 ipproto udp
// dport 53 lookup 8990".
func parseRules(show string, v6 bool) []ipRule {
	var out []ipRule
	for _, l := range strings.Split(show, "\n") {
		head, rest, ok := strings.Cut(strings.TrimSpace(l), ":")
		pref, err := strconv.Atoi(head)
		if !ok || err != nil || !ourPref(pref) {
			continue
		}
		r := ipRule{v6: v6, pref: pref}
		f := strings.Fields(rest)
		for i := 0; i+1 < len(f); i++ {
			if f[i] == "uidrange" {
				a, b, _ := strings.Cut(f[i+1], "-")
				r.uids.First, _ = strconv.Atoi(a)
				r.uids.Last, _ = strconv.Atoi(b)
			}
		}
		out = append(out, r)
	}
	return out
}

// planRules: which of have to delete (not wanted, or a second copy: `rule del` removes one of them) and which of
// want to add. Rules that stay are left alone, so a change of the list never opens a gap for the uids it keeps.
func planRules(have, want []ipRule) (del, add []ipRule) {
	seen := map[ipRule]bool{}
	for _, h := range have {
		if seen[h] || !slices.Contains(want, h) {
			del = append(del, h)
		}
		seen[h] = true
	}
	for _, w := range want {
		if !seen[w] {
			add = append(add, w)
		}
	}
	return del, add
}

// dnsRule: r is one of the port-53 rules, which go in by netlink (ruleMessage): Android's ip (iproute2 of 2017 on
// the Android 14 image) knows neither ipproto nor dport, while the kernel (4.17 and later) does. `ip rule show` of
// that ip still lists them by preference and uidrange, which is all parseRules reads, and `ip rule del pref P
// uidrange R` (delArgs) removes them, as the kernel compares only what a delete names.
func dnsRule(r ipRule) bool { return r.pref == prefDNSUDP || r.pref == prefDNSTCP }

// ruleMessage is the RTM_NEWRULE request (with NLM_F_ACK, NLM_F_CREATE and NLM_F_EXCL) for a port-53 rule: struct
// fib_rule_hdr, then FRA_PRIORITY, FRA_TABLE, FRA_UID_RANGE, FRA_IP_PROTO and FRA_DPORT_RANGE, in the host's byte
// order (little endian on the x86 guests), as iproute2 builds it.
func ruleMessage(r ipRule, seq uint32) []byte {
	const (
		rtmNewRule    = 32
		nlmRequest    = 0x1
		nlmAck        = 0x4
		nlmExcl       = 0x200
		nlmCreate     = 0x400
		fraPriority   = 6
		fraTable      = 15
		fraUIDRange   = 20
		fraIPProto    = 22
		fraDportRange = 24
		frActToTbl    = 1
		afInet        = 2
		ipprotoTCP    = 6
		ipprotoUDP    = 17
	)
	le32 := func(b []byte, v uint32) []byte { return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	le16 := func(b []byte, v uint16) []byte { return append(b, byte(v), byte(v>>8)) }
	// family, dst_len, src_len, tos, table (RT_TABLE_UNSPEC: the id is above 255, so FRA_TABLE), res1, res2, action,
	// flags
	body := []byte{afInet, 0, 0, 0, 0, 0, 0, frActToTbl, 0, 0, 0, 0}
	attr := func(typ uint16, val []byte) {
		body = le16(body, uint16(4+len(val)))
		body = le16(body, typ)
		body = append(body, val...)
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
	}
	proto := byte(ipprotoUDP)
	if r.pref == prefDNSTCP {
		proto = ipprotoTCP
	}
	attr(fraPriority, le32(nil, uint32(r.pref)))
	attr(fraTable, le32(nil, Table))
	attr(fraUIDRange, le32(le32(nil, uint32(r.uids.First)), uint32(r.uids.Last)))
	attr(fraIPProto, []byte{proto})
	attr(fraDportRange, le16(le16(nil, 53), 53))
	msg := le32(nil, uint32(16+len(body)))
	msg = le16(msg, rtmNewRule)
	msg = le16(msg, nlmRequest|nlmAck|nlmExcl|nlmCreate)
	msg = le32(msg, seq)
	msg = le32(msg, 0)
	return append(msg, body...)
}

// routeOK: `ip -4 route show table Table` has our route.
func routeOK(show string) bool {
	return strings.Contains(show, "local default") && strings.Contains(show, "dev lo")
}

// routeArgs set the route of Table (`ip -4 route …`).
var routeArgs = []string{"replace", "local", "0.0.0.0/0", "dev", "lo", "src", "127.0.0.1", "table", strconv.Itoa(Table)}
