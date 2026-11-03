package emu

import "testing"

func TestParseDiscardMaxBytes(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int64
		ok   bool
	}{
		{"typical", "2147483136\n", 2147483136, true},
		{"zero means no discard", "0\n", 0, true},
		{"padded", "  65536  ", 65536, true},
		{"empty", "", 0, false},
		{"cat error", "cat: /sys/...: No such file or directory", 0, false},
	}
	for _, c := range cases {
		got, ok := parseDiscardMaxBytes(c.out)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: parseDiscardMaxBytes(%q) = (%d, %v), want (%d, %v)",
				c.name, c.out, got, ok, c.want, c.ok)
		}
	}
}
