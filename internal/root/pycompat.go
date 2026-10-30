package root

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The helpers below reproduce the Python built-ins AUTO-MAS reads its files with, so modavd sees the same values in
// the same files (source.properties, config.ini, mas-avd.json ports): bytes.decode("utf-8", errors="replace"),
// str.splitlines(), str.strip() and int(str). They are exported for the avd package.

// DecodeUTF8Replace is bytes.decode("utf-8", errors="replace"): every maximal ill-formed subsequence becomes one
// U+FFFD (the Unicode recommendation Python follows; Go's strings.ToValidUTF8 would merge whole runs).
func DecodeUTF8Replace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	sb.Grow(len(b) + 8)
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || n > 1 {
			sb.Write(b[i : i+n])
			i += n
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += invalidSubpartLen(b[i:])
	}
	return sb.String()
}

// invalidSubpartLen is the length of the maximal subpart of an ill-formed UTF-8 sequence starting at p[0]: a lead
// byte plus the continuation bytes that could still have belonged to it.
func invalidSubpartLen(p []byte) int {
	need, lo, hi := 0, byte(0x80), byte(0xbf)
	switch c := p[0]; {
	case c >= 0xc2 && c <= 0xdf:
		need = 1
	case c == 0xe0:
		need, lo = 2, 0xa0
	case c == 0xed:
		need, hi = 2, 0x9f
	case c >= 0xe1 && c <= 0xef:
		need = 2
	case c == 0xf0:
		need, lo = 3, 0x90
	case c == 0xf4:
		need, hi = 3, 0x8f
	case c >= 0xf1 && c <= 0xf3:
		need = 3
	default:
		return 1
	}
	k := 1
	if k < len(p) && p[k] >= lo && p[k] <= hi {
		k++
		for k < len(p) && k <= need && p[k] >= 0x80 && p[k] <= 0xbf {
			k++
		}
	}
	if k > need {
		k = need // cannot happen for an invalid sequence; keeps the loop moving regardless
	}
	return k
}

// SplitLines is str.splitlines(): splits on \n, \r\n, \r, \v, \f, \x1c, \x1d, \x1e, U+0085, U+2028, U+2029; no
// empty string after a final line break.
func SplitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch r {
		case '\n', '\r', '\v', '\f', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			lines = append(lines, s[start:i])
			if r == '\r' && i+1 < len(s) && s[i+1] == '\n' {
				n = 2
			}
			start = i + n
		}
		i += n
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

// PyIsSpace is str.isspace() for one character: Unicode White_Space plus \x1c-\x1f.
func PyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// PyStrip is str.strip() without arguments.
func PyStrip(s string) string { return strings.TrimFunc(s, PyIsSpace) }

// PyInt is int(s) for a str in base 10: surrounding whitespace, an optional sign, ASCII digits with single
// underscores between them. ok=false where Python raises ValueError, and for values outside int64. int() trims less
// than strip(): in the ASCII range only " \t\n\v\f\r" (not \x1c-\x1f). Non-ASCII digits, which Python accepts, are
// refused.
func PyInt(s string) (int64, bool) {
	s = strings.TrimFunc(s, func(r rune) bool {
		if r < 0x80 {
			return r == ' ' || (r >= '\t' && r <= '\r')
		}
		return unicode.IsSpace(r)
	})
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, false
	}
	var v uint64
	prevDigit := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			d := uint64(c - '0')
			if v > (1<<63)/10 || (v == (1<<63)/10 && d > (1<<63)%10) {
				return 0, false
			}
			v = v*10 + d
			prevDigit = true
		case c == '_' && prevDigit && i+1 < len(s):
			prevDigit = false
		default:
			return 0, false
		}
	}
	if !prevDigit {
		return 0, false
	}
	if neg {
		return -int64(v), true // -(1<<63) wraps to itself, which is the right value
	}
	if v > 1<<63-1 {
		return 0, false
	}
	return int64(v), true
}

// ReadKeyValues parses a flat key=value text the way AUTO-MAS does (_read_properties / read_ini): decoded with
// replacement, split with SplitLines, each line partitioned at the first '=', key and value stripped. Lines without
// '=' are dropped, and so are empty keys when dropEmptyKeys (read_ini; _read_properties keeps them). A repeated key
// keeps its first position and its last value, like assigning into a Python dict.
func ReadKeyValues(data []byte, dropEmptyKeys bool) (keys []string, values map[string]string) {
	values = map[string]string{}
	for _, line := range SplitLines(DecodeUTF8Replace(data)) {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = PyStrip(k)
		if dropEmptyKeys && k == "" {
			continue
		}
		if _, seen := values[k]; !seen {
			keys = append(keys, k)
		}
		values[k] = PyStrip(v)
	}
	return keys, values
}
