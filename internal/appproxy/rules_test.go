package appproxy

import (
	"reflect"
	"strings"
	"testing"
)

func TestRestoreInput(t *testing.T) {
	got := restoreInput(Ranges([]int{10123, 10124, 10200}))
	want := `*mangle
:modavd_proxy_out - [0:0]
:modavd_proxy_app - [0:0]
:modavd_proxy_pre - [0:0]
-A modavd_proxy_out -d 127.0.0.0/8 -o lo -j RETURN
-A modavd_proxy_out -o lo -p udp -m udp --dport 53 -j MARK --set-xmark 0x1000000/0x1000000
-A modavd_proxy_out -o lo -p tcp -m tcp --dport 53 -j MARK --set-xmark 0x1000000/0x1000000
-A modavd_proxy_out -m owner --uid-owner 10123-10124 -j modavd_proxy_app
-A modavd_proxy_out -m owner --uid-owner 10200-10200 -j modavd_proxy_app
-A modavd_proxy_app -d 0.0.0.0/8 -j RETURN
-A modavd_proxy_app -d 10.0.0.0/8 -j RETURN
-A modavd_proxy_app -d 100.64.0.0/10 -j RETURN
-A modavd_proxy_app -d 127.0.0.0/8 -j RETURN
-A modavd_proxy_app -d 169.254.0.0/16 -j RETURN
-A modavd_proxy_app -d 172.16.0.0/12 -j RETURN
-A modavd_proxy_app -d 192.168.0.0/16 -j RETURN
-A modavd_proxy_app -d 224.0.0.0/3 -j RETURN
-A modavd_proxy_app -p tcp -j MARK --set-xmark 0x1000000/0x1000000
-A modavd_proxy_app -p udp -j MARK --set-xmark 0x1000000/0x1000000
-A modavd_proxy_app -j DROP
-A modavd_proxy_pre -i lo -p tcp -m mark --mark 0x1000000/0x1000000 -j TPROXY --on-port 18990 --on-ip 127.0.0.1 --tproxy-mark 0x1000000/0x1000000
-A modavd_proxy_pre -i lo -p udp -m mark --mark 0x1000000/0x1000000 -j TPROXY --on-port 18990 --on-ip 127.0.0.1 --tproxy-mark 0x1000000/0x1000000
COMMIT
`
	if got != want {
		t.Errorf("got\n%s", got)
	}
	// with no uid installed, the chains are there but nothing jumps to the app chain
	if strings.Contains(restoreInput(nil), "--uid-owner") {
		t.Error("uid rule without uids")
	}
}

func TestWantRules(t *testing.T) {
	r := wantRules(Ranges([]int{10123, 10124}))
	var lines []string
	for _, x := range r {
		lines = append(lines, x.family()+" "+strings.Join(x.addArgs(), " "))
	}
	want := []string{
		"-4 add pref 8990 fwmark 0x1000000/0x1000000 lookup 8990",
		"-4 add pref 8991 uidrange 10123-10124 ipproto udp dport 53 lookup 8990",
		"-4 add pref 8992 uidrange 10123-10124 ipproto tcp dport 53 lookup 8990",
		"-6 add pref 8993 uidrange 10123-10124 prohibit",
	}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("%q", lines)
	}
	if got := strings.Join(r[1].delArgs(), " "); got != "del pref 8991 uidrange 10123-10124" {
		t.Error(got)
	}
	if got := strings.Join(r[0].delArgs(), " "); got != "del pref 8990" {
		t.Error(got)
	}
}

const ruleShow4 = `0:	from all lookup local
8990:	from all fwmark 0x1000000/0x1000000 lookup 8990
8991:	from all uidrange 10123-10124 ipproto udp dport 53 lookup 8990
8991:	from all uidrange 10300-10300 ipproto udp dport 53 lookup 8990
8992:	from all uidrange 10123-10124 ipproto tcp dport 53 lookup 8990
8992:	from all uidrange 10123-10124 ipproto tcp dport 53 lookup 8990
10000:	from all fwmark 0xc0000/0xd0000 lookup legacy_system
15000:	from all iif lo lookup eth0
32000:	from all unreachable
`

func TestParseAndPlanRules(t *testing.T) {
	have := parseRules(ruleShow4, false)
	want := []ipRule{
		{pref: 8990},
		{pref: 8991, uids: UIDRange{10123, 10124}},
		{pref: 8991, uids: UIDRange{10300, 10300}},
		{pref: 8992, uids: UIDRange{10123, 10124}},
		{pref: 8992, uids: UIDRange{10123, 10124}},
	}
	if !reflect.DeepEqual(have, want) {
		t.Fatalf("parsed %+v", have)
	}
	del, add := planRules(have, wantRules(Ranges([]int{10123, 10124})))
	wantDel := []ipRule{{pref: 8991, uids: UIDRange{10300, 10300}}, {pref: 8992, uids: UIDRange{10123, 10124}}}
	wantAdd := []ipRule{{v6: true, pref: 8993, uids: UIDRange{10123, 10124}}}
	if !reflect.DeepEqual(del, wantDel) || !reflect.DeepEqual(add, wantAdd) {
		t.Errorf("del %+v add %+v", del, add)
	}
	if d, a := planRules(nil, nil); d != nil || a != nil {
		t.Error("nothing to do")
	}
	// all in place: nothing to do
	all := wantRules(Ranges([]int{10123}))
	if d, a := planRules(all, all); d != nil || a != nil {
		t.Errorf("in place: %v %v", d, a)
	}
}

func TestParseMangle(t *testing.T) {
	save := "*mangle\n:PREROUTING ACCEPT [0:0]\n-A PREROUTING -j modavd_proxy_pre\n-A PREROUTING -j bw_mangle_PREROUTING\n" +
		"-A OUTPUT -j modavd_proxy_out\n-A OUTPUT -j connmark_mangle_OUTPUT\n" + restoreInput(Ranges([]int{10123}))
	s := parseMangle(save)
	if s.jumps != 2 || s.rules != len(chainRules(Ranges([]int{10123}))) {
		t.Errorf("%+v", s)
	}
	if s := parseMangle("*mangle\n-A OUTPUT -j connmark_mangle_OUTPUT\nCOMMIT\n"); s != (mangleState{}) {
		t.Errorf("none: %+v", s)
	}
	if !routeOK("local default dev lo scope host src 127.0.0.1 \n") || routeOK("") {
		t.Error("routeOK")
	}
}
