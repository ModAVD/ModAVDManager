//go:build linux

package appproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// Linux socket options (linux/in.h); not all are in package syscall.
const (
	ipTransparent     = 19 // IP_TRANSPARENT: bind to and answer from addresses that are not local
	ipRecvOrigDstAddr = 20 // IP_RECVORIGDSTADDR; also the type of its control message (IP_ORIGDSTADDR)
)

// transparent sets IP_TRANSPARENT and SO_REUSEADDR (reply sockets of many sessions share a source address), and
// IP_RECVORIGDSTADDR when origDst.
func transparent(origDst bool) func(network, address string, c syscall.RawConn) error {
	return func(_, _ string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			opts := [][2]int{{syscall.IPPROTO_IP, ipTransparent}, {syscall.SOL_SOCKET, syscall.SO_REUSEADDR}}
			if origDst {
				opts = append(opts, [2]int{syscall.IPPROTO_IP, ipRecvOrigDstAddr})
			}
			for _, o := range opts {
				if serr = syscall.SetsockoptInt(int(fd), o[0], o[1], 1); serr != nil {
					return
				}
			}
		})
		if err != nil {
			return err
		}
		return serr
	}
}

// listenTCP is the TCP socket TPROXY hands connections to; an accepted connection's local address is where the app
// connected to.
//
// It carries ReplyMark: Android sets net.ipv4.tcp_fwmark_accept, so without a mark of its own the SYN-ACK and every
// later packet to the app would carry the app's SYN's mark, Mark, and PREROUTING's TPROXY would take them back to
// this socket instead of the app's.
func listenTCP(addr netip.AddrPort) (*net.TCPListener, error) {
	tr := transparent(false)
	lc := net.ListenConfig{KeepAlive: tcpKeepAlive, Control: func(network, address string, c syscall.RawConn) error {
		if err := tr(network, address, c); err != nil {
			return err
		}
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, ReplyMark)
		}); err != nil {
			return err
		}
		return serr
	}}
	l, err := lc.Listen(context.Background(), "tcp4", addr.String())
	if err != nil {
		return nil, err
	}
	return l.(*net.TCPListener), nil
}

// listenUDP is the UDP socket TPROXY hands datagrams to, each with its original destination in a control message.
func listenUDP(addr netip.AddrPort) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: transparent(true)}
	c, err := lc.ListenPacket(context.Background(), "udp4", addr.String())
	if err != nil {
		return nil, err
	}
	return c.(*net.UDPConn), nil
}

func serveTCP(l *net.TCPListener, core func() *Core) {
	for {
		c, err := l.AcceptTCP()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		orig := c.LocalAddr().(*net.TCPAddr).AddrPort()
		go core().HandleTCP(c, unmap(orig))
	}
}

func serveUDP(c *net.UDPConn, core func() *Core) {
	buf := make([]byte, udpBufSize)
	oob := make([]byte, 256)
	for {
		n, oobn, _, from, err := c.ReadMsgUDPAddrPort(buf, oob)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		dst, ok := origDst(oob[:oobn])
		if !ok {
			continue
		}
		core().HandleUDP(unmap(from), dst, buf[:n])
	}
}

// origDst reads IP_ORIGDSTADDR: a struct sockaddr_in (family, then port and address in network order).
func origDst(oob []byte) (netip.AddrPort, bool) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return netip.AddrPort{}, false
	}
	for _, m := range msgs {
		if m.Header.Level == syscall.IPPROTO_IP && m.Header.Type == ipRecvOrigDstAddr && len(m.Data) >= 8 {
			return netip.AddrPortFrom(netip.AddrFrom4([4]byte(m.Data[4:8])), binary.BigEndian.Uint16(m.Data[2:4])), true
		}
	}
	return netip.AddrPort{}, false
}

func unmap(ap netip.AddrPort) netip.AddrPort { return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()) }

// replySockets answer the apps from the addresses they talked to: one transparent, unconnected UDP socket bound to
// each such address. Unconnected, so that TPROXY (which only looks up connected sockets before its listener) keeps
// handing the apps' datagrams to the listener.
type replySockets struct {
	mu sync.Mutex
	m  map[netip.AddrPort]*replySocket
}

type replySocket struct {
	c    *net.UDPConn
	last time.Time
}

const maxReplySockets = 1024

func newReplySockets() *replySockets {
	r := &replySockets{m: map[netip.AddrPort]*replySocket{}}
	go r.reap()
	return r
}

func (r *replySockets) send(from, to netip.AddrPort, p []byte) error {
	r.mu.Lock()
	s := r.m[from]
	if s == nil {
		if len(r.m) >= maxReplySockets {
			r.evictLocked()
		}
		lc := net.ListenConfig{Control: transparent(false)}
		pc, err := lc.ListenPacket(context.Background(), "udp4", from.String())
		if err != nil {
			r.mu.Unlock()
			return err
		}
		s = &replySocket{c: pc.(*net.UDPConn)}
		r.m[from] = s
	}
	s.last = time.Now()
	r.mu.Unlock()
	_, err := s.c.WriteToUDPAddrPort(p, to)
	return err
}

func (r *replySockets) evictLocked() {
	var oldest netip.AddrPort
	var at time.Time
	for k, s := range r.m {
		if at.IsZero() || s.last.Before(at) {
			oldest, at = k, s.last
		}
	}
	if s := r.m[oldest]; s != nil {
		s.c.Close()
		delete(r.m, oldest)
	}
}

func (r *replySockets) reap() {
	for range time.Tick(30 * time.Second) {
		r.mu.Lock()
		for k, s := range r.m {
			if time.Since(s.last) > udpIdle {
				s.c.Close()
				delete(r.m, k)
			}
		}
		r.mu.Unlock()
	}
}
