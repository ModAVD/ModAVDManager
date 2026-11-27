package appproxy

import (
	"encoding/binary"
	"testing"
)

// clientHello builds a TLS record with a ClientHello carrying these extensions (type, data).
func clientHello(exts ...[]byte) []byte {
	var ext []byte
	for _, e := range exts {
		ext = append(ext, e...)
	}
	body := []byte{3, 3}
	body = append(body, make([]byte, 32)...)          // random
	body = append(body, 32)                           // session id
	body = append(body, make([]byte, 32)...)          //
	body = append(body, 0, 4, 0x13, 0x01, 0x13, 0x02) // cipher suites
	body = append(body, 1, 0)                         // compression
	body = binary.BigEndian.AppendUint16(body, uint16(len(ext)))
	body = append(body, ext...)
	hs := append([]byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
	rec := []byte{0x16, 3, 1, byte(len(hs) >> 8), byte(len(hs))}
	return append(rec, hs...)
}

func extension(typ uint16, data []byte) []byte {
	b := binary.BigEndian.AppendUint16(nil, typ)
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...)
}

func sniExt(name string) []byte {
	d := binary.BigEndian.AppendUint16(nil, uint16(3+len(name)))
	d = append(d, 0)
	d = binary.BigEndian.AppendUint16(d, uint16(len(name)))
	return extension(0, append(d, name...))
}

func TestSniffTLS(t *testing.T) {
	keyShare := extension(51, make([]byte, 1200)) // big, and before the SNI (Chrome shuffles extensions)
	full := clientHello(keyShare, sniExt("Game.Example.COM"), extension(16, []byte{0, 3, 2, 'h', '2'}))
	if h, more := sniffHost(full); h != "game.example.com" || more {
		t.Errorf("SNI: %q %v", h, more)
	}
	for _, n := range []int{1, 4, 5, 200, len(full) - 1} {
		if h, more := sniffHost(full[:n]); h != "" || !more {
			t.Errorf("first %d bytes: %q %v", n, h, more)
		}
	}
	if h, more := sniffHost(clientHello(keyShare)); h != "" || more {
		t.Errorf("no SNI: %q %v", h, more)
	}
	if h, _ := sniffHost(clientHello(sniExt("public.example"), extension(0xfe0d, make([]byte, 100)))); h != "" {
		t.Errorf("ECH: %q", h)
	}
	if h, _ := sniffHost(clientHello(sniExt("1.2.3.4"))); h != "" {
		t.Errorf("IP as SNI: %q", h)
	}
	if h, more := sniffHost([]byte{0x16, 2, 0, 0, 10}); h != "" || more {
		t.Errorf("not TLS: %q %v", h, more)
	}
}

func TestSniffHTTP(t *testing.T) {
	cases := []struct {
		in   string
		host string
		more bool
	}{
		{"GET / HTTP/1.1\r\nHost: Example.com:8080\r\nAccept: */*\r\n\r\n", "example.com", false},
		{"POST /x HTTP/1.1\r\nhost:api.example.com\r\n\r\nbody", "api.example.com", false},
		{"GET / HTTP/1.1\r\nHost: [2001:db8::1]:80\r\n\r\n", "", false},
		{"GET / HTTP/1.1\r\nHost: 1.2.3.4\r\n\r\n", "", false},
		{"GET / HTTP/1.1\r\nHost: exa", "", true},
		{"GE", "", true},
		{"SSH-2.0-OpenSSH\r\n", "", false},
		{"\x00\x01binary", "", false},
	}
	for _, c := range cases {
		h, more := sniffHost([]byte(c.in))
		if h != c.host || more != c.more {
			t.Errorf("%q: %q %v, want %q %v", c.in, h, more, c.host, c.more)
		}
	}
	if h, more := sniffHost(nil); h != "" || !more {
		t.Error("empty")
	}
}
