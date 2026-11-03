package appproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"time"
)

// ProbeResult is what Probe found at a proxy address.
type ProbeResult struct {
	Kind string `json:"kind"` // "socks5" or "http"
	// UDP: "ok", "unsupported" (an HTTP proxy, or UDP ASSOCIATE refused), "failed" (UDPError says why) or "" (not
	// tried).
	UDP      string `json:"udp"`
	UDPError string `json:"udp_error,omitempty"`
}

const probeTimeout = 3 * time.Second

// Probe asks what speaks at addr, as the host reaches it (127.0.0.1:7890 for the guest's 10.0.2.2:7890): a SOCKS5
// greeting first, then an HTTP CONNECT to a closed local port (an HTTP proxy answers it with an error, without going
// anywhere). Mixed ports (Clash and its kin) speak both and come out as SOCKS5. withUDP also opens a UDP association
// and sends one DNS query through it to resolver.
func Probe(ctx context.Context, addr netip.AddrPort, withUDP bool, resolver netip.AddrPort) (ProbeResult, error) {
	var r ProbeResult
	socks, err := probeSOCKS(ctx, addr)
	switch {
	case err != nil && !errors.Is(err, errNotSOCKS5):
		return r, err
	case socks:
		r.Kind = "socks5"
	default:
		if err := probeHTTP(ctx, addr); err != nil {
			return r, fmt.Errorf("%s 上的程序既不说 SOCKS5，也不说 HTTP 代理（%v）", addr, err)
		}
		r.Kind = "http"
	}
	if !withUDP {
		return r, nil
	}
	if r.Kind != "socks5" {
		r.UDP = "unsupported"
		return r, nil
	}
	r.UDP, r.UDPError = probeUDP(ctx, addr, resolver)
	return r, nil
}

func probeConn(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, fmt.Errorf("连不上 %s：%s（代理软件开着吗？端口对吗？）", addr, shortNetErr(err))
	}
	c.SetDeadline(time.Now().Add(probeTimeout))
	return c, nil
}

func probeSOCKS(ctx context.Context, addr netip.AddrPort) (bool, error) {
	c, err := probeConn(ctx, addr)
	if err != nil {
		return false, err
	}
	defer c.Close()
	if _, err := c.Write([]byte{socksVersion, 1, 0}); err != nil {
		return false, errNotSOCKS5
	}
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil || b[0] != socksVersion {
		return false, errNotSOCKS5
	}
	if b[1] != 0 {
		return false, errSOCKSAuth
	}
	return true, nil
}

func probeHTTP(ctx context.Context, addr netip.AddrPort) error {
	c, err := probeConn(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = httpConnect(c, Target{Addr: netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), 9)})
	var re *replyError
	if err == nil || errors.As(err, &re) {
		return nil // an HTTP answer, whatever it says
	}
	return err
}

// probeUDP asks resolver for example.com's address through a UDP association.
func probeUDP(ctx context.Context, addr, resolver netip.AddrPort) (string, string) {
	up := &Upstream{Server: Server{Scheme: "socks5", Addr: addr}, Timeout: probeTimeout}
	a, err := up.Associate(ctx)
	if errors.Is(err, errNoUDP) {
		return "unsupported", err.Error()
	}
	if err != nil {
		return "failed", err.Error()
	}
	defer a.Close()
	q := binary.BigEndian.AppendUint16(nil, 0x6d61)
	q = append(q, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0)
	for _, l := range strings.Split("example.com", ".") {
		q = append(append(q, byte(len(l))), l...)
	}
	q = append(q, 0, 0, dnsTypeA, 0, dnsClassIN)
	if err := a.WriteTo(q, Target{Addr: resolver}); err != nil {
		return "failed", err.Error()
	}
	a.conn.SetReadDeadline(time.Now().Add(probeTimeout))
	p, _, err := a.ReadFrom(make([]byte, 1500))
	if err != nil {
		return "failed", fmt.Sprintf("经代理向 %s 发的 DNS 查询 %s 内没有回音", resolver, probeTimeout)
	}
	if len(p) < dnsHeaderLen || binary.BigEndian.Uint16(p) != 0x6d61 {
		return "failed", "经代理回来的不是 DNS 回答"
	}
	return "ok", ""
}
