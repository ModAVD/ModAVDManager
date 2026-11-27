package emu

import "testing"

func TestShouldAutoTrim(t *testing.T) {
	const now = int64(1_800_000_000)
	const day = int64(24 * 60 * 60)
	cases := []struct {
		name string
		last int64
		want bool
	}{
		{"never trimmed", 0, true},
		{"negative is garbage", -5, true},
		{"just now", now, false},
		{"an hour ago", now - 3600, false},
		{"one second short of the interval", now - day + 1, false},
		{"exactly the interval", now - day, true},
		{"two days ago", now - 2*day, true},
		{"clock set back", now + 3600, true},
	}
	for _, c := range cases {
		if got := shouldAutoTrim(c.last, now); got != c.want {
			t.Errorf("%s: shouldAutoTrim(%d, %d) = %v, want %v", c.name, c.last, now, got, c.want)
		}
	}
}
