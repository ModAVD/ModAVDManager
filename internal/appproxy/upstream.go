package appproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Target is where a connection or a datagram goes: a name and port, or an address.
type Target struct {
	Host string // a domain name; "" when Addr is the target
	Port uint16 // with Host
	Addr netip.AddrPort
}

func (t Target) String() string {
	if t.Host != "" {
		return net.JoinHostPort(t.Host, strconv.Itoa(int(t.Port)))
	}
	return t.Addr.String()
}

// The SOCKS5 protocol (RFC 1928), client side, no authentication.
const (
	socksVersion   = 5
	socksConnect   = 1
	socksAssociate = 3
	atypIPv4       = 1
	atypDomain     = 3
	atypIPv6       = 4
)

var socksReplies = map[byte]string{
	1: "代理内部出错", 2: "代理的规则不让连", 3: "网络不可达", 4: "主机不可达", 5: "对方拒绝连接",
	6: "连接超时", 7: "代理不支持这个命令", 8: "代理不支持这种地址",
}

// appendSOCKSAddr appends ATYP, address and port.
func appendSOCKSAddr(b []byte, t Target) []byte {
	if t.Host != "" {
		b = append(b, atypDomain, byte(len(t.Host)))
		b = append(b, t.Host...)
		return binary.BigEndian.AppendUint16(b, t.Port)
	}
	a := t.Addr.Addr().Unmap()
	if a.Is4() {
		b = append(b, atypIPv4)
	} else {
		b = append(b, atypIPv6)
	}
	b = append(b, a.AsSlice()...)
	return binary.BigEndian.AppendUint16(b, t.Addr.Port())
}

// parseSOCKSAddr reads ATYP, address and port from the start of b and says how many bytes they took.
func parseSOCKSAddr(b []byte) (Target, int, error) {
	if len(b) < 1 {
		return Target{}, 0, io.ErrUnexpectedEOF
	}
	switch b[0] {
	case atypIPv4:
		if len(b) < 7 {
			return Target{}, 0, io.ErrUnexpectedEOF
		}
		a := netip.AddrFrom4([4]byte(b[1:5]))
		return Target{Addr: netip.AddrPortFrom(a, binary.BigEndian.Uint16(b[5:7]))}, 7, nil
	case atypIPv6:
		if len(b) < 19 {
			return Target{}, 0, io.ErrUnexpectedEOF
		}
		a := netip.AddrFrom16([16]byte(b[1:17])).Unmap()
		return Target{Addr: netip.AddrPortFrom(a, binary.BigEndian.Uint16(b[17:19]))}, 19, nil
	case atypDomain:
		if len(b) < 2 || len(b) < 2+int(b[1])+2 {
			return Target{}, 0, io.ErrUnexpectedEOF
		}
		n := int(b[1])
		return Target{Host: string(b[2 : 2+n]), Port: binary.BigEndian.Uint16(b[2+n : 4+n])}, 4 + n, nil
	}
	return Target{}, 0, fmt.Errorf("SOCKS5 地址类型 %d 不认识", b[0])
}

// readSOCKSAddr reads ATYP, address and port from r.
func readSOCKSAddr(r io.Reader) (Target, error) {
	var b [1 + 1 + 255 + 2]byte
	if _, err := io.ReadFull(r, b[:1]); err != nil {
		return Target{}, err
	}
	need := 0
	switch b[0] {
	case atypIPv4:
		need = 4 + 2
	case atypIPv6:
		need = 16 + 2
	case atypDomain:
		if _, err := io.ReadFull(r, b[1:2]); err != nil {
			return Target{}, err
		}
		need = 1 + int(b[1]) + 2
	default:
		return Target{}, fmt.Errorf("SOCKS5 地址类型 %d 不认识", b[0])
	}
	start := 1
	if b[0] == atypDomain {
		start, need = 2, need-1
	}
	if _, err := io.ReadFull(r, b[start:start+need]); err != nil {
		return Target{}, err
	}
	t, _, err := parseSOCKSAddr(b[:start+need])
	return t, err
}

var (
	errNotSOCKS5 = errors.New("这个端口说的不是 SOCKS5")
	errSOCKSAuth = errors.New("代理要账号密码，这一版只支持不要认证的代理")
)

// socksHello is the no-authentication greeting.
func socksHello(c net.Conn) error {
	if _, err := c.Write([]byte{socksVersion, 1, 0}); err != nil {
		return err
	}
	var b [2]byte
	if _, err := io.ReadFull(c, b[:]); err != nil {
		return err
	}
	switch {
	case b[0] != socksVersion:
		return errNotSOCKS5
	case b[1] == 0xff:
		return errSOCKSAuth
	case b[1] != 0:
		return fmt.Errorf("代理选了认证方式 %d，这一版只支持不要认证的代理", b[1])
	}
	return nil
}

// socksRequest sends one request and returns the bound address of the reply.
func socksRequest(c net.Conn, cmd byte, t Target) (Target, error) {
	req := appendSOCKSAddr([]byte{socksVersion, cmd, 0}, t)
	if _, err := c.Write(req); err != nil {
		return Target{}, err
	}
	var h [3]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return Target{}, err
	}
	if h[0] != socksVersion {
		return Target{}, errNotSOCKS5
	}
	if h[1] != 0 {
		msg, ok := socksReplies[h[1]]
		if !ok {
			msg = fmt.Sprintf("错误码 %d", h[1])
		}
		return Target{}, &replyError{code: h[1], msg: msg}
	}
	return readSOCKSAddr(c)
}

// replyError is a refusal by the proxy (as opposed to not reaching it).
type replyError struct {
	code byte
	msg  string
}

func (e *replyError) Error() string { return "代理拒绝：" + e.msg }

// httpConnect asks an HTTP proxy for a tunnel. pending is what the proxy already sent after its answer (the far
// end's first bytes).
func httpConnect(c net.Conn, t Target) (pending []byte, err error) {
	hp := t.String()
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: modavd-proxyd\r\n\r\n", hp, hp); err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(c, 4096)
	if b, err := br.Peek(5); !strings.HasPrefix(string(b), "HTTP/"[:len(b)]) || (err == nil && string(b) != "HTTP/") {
		return nil, errors.New("这个端口说的不是 HTTP")
	}
	line, err := readLine(br)
	if err != nil {
		return nil, err
	}
	proto, rest, _ := strings.Cut(line, " ")
	code, _, _ := strings.Cut(rest, " ")
	if !strings.HasPrefix(proto, "HTTP/1.") || len(code) != 3 {
		return nil, errors.New("这个端口说的不是 HTTP")
	}
	for n := 0; ; n++ {
		l, err := readLine(br)
		if err != nil {
			return nil, err
		}
		if l == "" {
			break
		}
		if n > 100 {
			return nil, errors.New("代理的回应头太长")
		}
	}
	switch {
	case code == "407":
		return nil, errors.New("代理要账号密码，这一版只支持不要认证的代理")
	case code[0] != '2':
		return nil, &replyError{msg: "HTTP " + code}
	}
	if n := br.Buffered(); n > 0 {
		pending = make([]byte, n)
		br.Read(pending)
	}
	return pending, nil
}

// readLine reads one CRLF line of at most 4 KB.
func readLine(br *bufio.Reader) (string, error) {
	b, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", errors.New("代理的回应行太长")
		}
		return "", err
	}
	return string(bytes.TrimRight(b, "\r\n")), nil
}

// Upstream is the user's proxy as the daemon uses it.
type Upstream struct {
	Server  Server
	Timeout time.Duration                                                     // connecting plus the handshake; 0 is 10 s
	Dial    func(ctx context.Context, network, addr string) (net.Conn, error) // nil: a net.Dialer
	Health  *Health                                                           // nil: not recorded
}

func (u *Upstream) dial(ctx context.Context) (net.Conn, error) {
	d := u.Timeout
	if d == 0 {
		d = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	dial := u.Dial
	if dial == nil {
		dial = (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext
	}
	c, err := dial(ctx, "tcp", u.Server.Addr.String())
	if err != nil {
		return nil, fmt.Errorf("连不上代理 %s：%v", u.Server.Addr, shortNetErr(err))
	}
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	return c, nil
}

// DialTCP opens a tunnel to t. pending: bytes the far end already sent (HTTP CONNECT only), to hand over first.
func (u *Upstream) DialTCP(ctx context.Context, t Target) (c net.Conn, pending []byte, err error) {
	defer func() { u.Health.tcp(err) }()
	c, err = u.dial(ctx)
	if err != nil {
		return nil, nil, err
	}
	if u.Server.SOCKS5() {
		if err = socksHello(c); err == nil {
			_, err = socksRequest(c, socksConnect, t)
		}
	} else {
		pending, err = httpConnect(c, t)
	}
	if err != nil {
		c.Close()
		var re *replyError
		if errors.As(err, &re) {
			return nil, nil, fmt.Errorf("经代理连 %s 失败：%w", t, re)
		}
		return nil, nil, fmt.Errorf("经代理连 %s 失败：%s", t, shortNetErr(err))
	}
	c.SetDeadline(time.Time{})
	return c, pending, nil
}

// errNoUDP: the proxy does not relay UDP (an HTTP proxy, or SOCKS5 UDP ASSOCIATE refused).
var errNoUDP = errors.New("代理不转发 UDP")

// Association is one SOCKS5 UDP association.
type Association struct {
	ctrl  net.Conn     // closing it (or the proxy closing it) ends the association
	conn  *net.UDPConn // connected to the relay
	relay netip.AddrPort
	once  sync.Once
}

// Associate opens a UDP association. The proxy's relay address is used as given, except that an unspecified or
// loopback one (mihomo and others answer with the address they listen on, 127.0.0.1) means the proxy's own host:
// to the guest, 127.0.0.1 is itself.
func (u *Upstream) Associate(ctx context.Context) (a *Association, err error) {
	defer func() { u.Health.udp(err) }()
	if !u.Server.SOCKS5() {
		return nil, errNoUDP
	}
	c, err := u.dial(ctx)
	if err != nil {
		return nil, err
	}
	var bnd Target
	if err = socksHello(c); err == nil {
		bnd, err = socksRequest(c, socksAssociate, Target{Addr: netip.AddrPortFrom(netip.IPv4Unspecified(), 0)})
	}
	if err != nil {
		c.Close()
		var re *replyError
		if errors.As(err, &re) {
			return nil, fmt.Errorf("%w（%v）", errNoUDP, err)
		}
		return nil, err
	}
	relay, err := relayAddr(bnd, u.Server.Addr)
	if err != nil {
		c.Close()
		return nil, err
	}
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(relay))
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("连不上代理的 UDP 转发地址 %s：%v", relay, err)
	}
	c.SetDeadline(time.Time{})
	a = &Association{ctrl: c, conn: conn, relay: relay}
	go func() {
		io.Copy(io.Discard, c) // returns when the proxy ends the association or Close closes c
		a.Close()
	}()
	return a, nil
}

// relayAddr is where to send datagrams for a UDP ASSOCIATE answer bnd from the proxy at server.
func relayAddr(bnd Target, server netip.AddrPort) (netip.AddrPort, error) {
	if bnd.Host != "" {
		return netip.AddrPort{}, fmt.Errorf("代理的 UDP 转发地址是域名 %s，不支持", bnd.Host)
	}
	a := bnd.Addr.Addr().Unmap()
	if a.IsUnspecified() || a.IsLoopback() {
		a = server.Addr()
	}
	if !a.Is4() || bnd.Addr.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("代理的 UDP 转发地址 %s 用不了", bnd.Addr)
	}
	return netip.AddrPortFrom(a, bnd.Addr.Port()), nil
}

// WriteTo sends p to t through the relay.
func (a *Association) WriteTo(p []byte, t Target) error {
	b := appendSOCKSAddr(make([]byte, 3, 3+1+1+255+2+len(p)), t) // RSV RSV FRAG = 0
	_, err := a.conn.Write(append(b, p...))
	return err
}

// ReadFrom reads the next datagram: its payload (inside buf) and where it came from. Fragmented ones (FRAG != 0) are
// skipped.
func (a *Association) ReadFrom(buf []byte) ([]byte, Target, error) {
	for {
		n, err := a.conn.Read(buf)
		if err != nil {
			return nil, Target{}, err
		}
		if n < 3 || buf[0] != 0 || buf[1] != 0 || buf[2] != 0 {
			continue
		}
		t, m, err := parseSOCKSAddr(buf[3:n])
		if err != nil {
			continue
		}
		return buf[3+m : n], t, nil
	}
}

// Close ends the association.
func (a *Association) Close() {
	a.once.Do(func() {
		a.ctrl.Close()
		a.conn.Close()
	})
}

// shortNetErr drops Go's "dial tcp 1.2.3.4:5:" prefixes.
func shortNetErr(err error) string {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		err = oe.Err
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "超时"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "超时"
	}
	return err.Error()
}

// Health records how the proxy has been answering, for Status.Server.
type Health struct {
	mu                         sync.Mutex
	tcpState, udpState         string
	lastErr, lastErrAt, lastOK string
	now                        func() time.Time
}

func (h *Health) tcp(err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.record(&h.tcpState, err)
}

func (h *Health) udp(err error) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if errors.Is(err, errNoUDP) {
		h.udpState = "unsupported"
		return
	}
	h.record(&h.udpState, err)
}

// record: a refusal for one target (a SOCKS5 or HTTP error reply) still means the proxy answers.
func (h *Health) record(state *string, err error) {
	at := h.clock().UTC().Format(time.RFC3339)
	var re *replyError
	switch {
	case err == nil || errors.As(err, &re):
		*state, h.lastOK = "ok", at
		if err != nil {
			h.lastErr, h.lastErrAt = err.Error(), at
		}
	default:
		*state, h.lastErr, h.lastErrAt = "failed", err.Error(), at
	}
}

func (h *Health) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

// Status fills ServerStatus.
func (h *Health) Status(s Server) ServerStatus {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := ServerStatus{URL: s.String(), TCP: orUnknown(h.tcpState), UDP: orUnknown(h.udpState),
		LastError: h.lastErr, LastErrorAt: h.lastErrAt, LastOKAt: h.lastOK}
	if !s.SOCKS5() {
		st.UDP = "unsupported"
	}
	return st
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
