package appproxy

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"strings"
)

// The first bytes of a TCP connection often name the server: the SNI of a TLS ClientHello, the Host of an HTTP/1
// request. The relay connects by that name, so the proxy resolves it (and can apply its own rules for it) even when
// the address the app got is wrong for the proxy's side, such as one netd cached for an app going direct.

// sniffMax is the most the relay waits for: a ClientHello with post-quantum key shares is about 2 KB, and one TLS
// record is at most 16 KB.
const sniffMax = 5 + 16384

// sniffHost returns the server name at the start of b. more: b could still turn into a ClientHello or a request
// whose name comes later, so more bytes would tell. A ClientHello with encrypted_client_hello gives "" (its SNI is
// only the public name of the provider), as do names that are IP addresses.
func sniffHost(b []byte) (host string, more bool) {
	if len(b) == 0 {
		return "", true
	}
	if b[0] == 0x16 {
		return sniffTLS(b)
	}
	return sniffHTTP(b)
}

func sniffTLS(b []byte) (string, bool) {
	if len(b) < 5 {
		return "", true
	}
	if b[1] != 3 {
		return "", false
	}
	n := int(binary.BigEndian.Uint16(b[3:]))
	if len(b) < 5+n {
		return "", 5+n <= sniffMax
	}
	return clientHelloSNI(b[5 : 5+n]), false
}

// clientHelloSNI reads the server_name of a handshake message (the whole ClientHello inside one record).
func clientHelloSNI(h []byte) string {
	if len(h) < 4 || h[0] != 1 {
		return ""
	}
	n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	if len(h) < 4+n {
		return ""
	}
	p := h[4 : 4+n]
	// version, random
	if len(p) < 2+32 {
		return ""
	}
	p = p[34:]
	var ok bool
	if p, ok = skipVec(p, 1); !ok { // session id
		return ""
	}
	if p, ok = skipVec(p, 2); !ok { // cipher suites
		return ""
	}
	if p, ok = skipVec(p, 1); !ok { // compression methods
		return ""
	}
	if len(p) < 2 {
		return ""
	}
	ext := p[2:]
	if l := int(binary.BigEndian.Uint16(p)); l <= len(ext) {
		ext = ext[:l]
	}
	sni := ""
	for len(ext) >= 4 {
		typ, l := binary.BigEndian.Uint16(ext), int(binary.BigEndian.Uint16(ext[2:]))
		if len(ext) < 4+l {
			break
		}
		data := ext[4 : 4+l]
		ext = ext[4+l:]
		switch typ {
		case 0xfe0d: // encrypted_client_hello
			return ""
		case 0: // server_name: list length, then type 0 (host_name) and the name
			if len(data) >= 5 && data[2] == 0 {
				if nl := int(binary.BigEndian.Uint16(data[3:])); 5+nl <= len(data) {
					sni = hostName(string(data[5 : 5+nl]))
				}
			}
		}
	}
	return sni
}

func skipVec(p []byte, lenBytes int) ([]byte, bool) {
	if len(p) < lenBytes {
		return nil, false
	}
	n := int(p[0])
	if lenBytes == 2 {
		n = int(binary.BigEndian.Uint16(p))
	}
	if len(p) < lenBytes+n {
		return nil, false
	}
	return p[lenBytes+n:], true
}

var httpMethods = []string{"GET ", "POST ", "HEAD ", "PUT ", "DELETE ", "OPTIONS ", "PATCH ", "TRACE "}

func sniffHTTP(b []byte) (string, bool) {
	method := false
	for _, m := range httpMethods {
		if bytes.HasPrefix(b, []byte(m)) {
			method = true
			break
		}
		if len(b) < len(m) && strings.HasPrefix(m, string(b)) {
			return "", true
		}
	}
	if !method {
		return "", false
	}
	end := bytes.Index(b, []byte("\r\n\r\n"))
	if end < 0 {
		return "", len(b) < 8192
	}
	lines := strings.Split(string(b[:end]), "\r\n")
	for _, l := range lines[1:] {
		k, v, ok := strings.Cut(l, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), "host") {
			h := strings.TrimSpace(v)
			if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.Contains(h[i:], "]") {
				h = h[:i]
			}
			return hostName(h), false
		}
	}
	return "", false
}

// hostName is s in lower case when it is a DNS host name (not an IP address), else "".
func hostName(s string) string {
	s = strings.TrimSuffix(strings.ToLower(s), ".")
	if s == "" || len(s) > 253 {
		return ""
	}
	if _, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return ""
	}
	for _, l := range strings.Split(s, ".") {
		if !hostLabel(l) || len(l) > 63 {
			return ""
		}
	}
	return s
}
