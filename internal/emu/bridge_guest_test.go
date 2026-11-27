package emu

import (
	"testing"

	"modavd/internal/avd"
)

// TestBridgeGuestSupported: the bridge is for x86_64 guests only. The decision is a function of config.ini's values as
// avd parsed them; without either key (or without a readable config) it is the x86_64 behaviour of before.
func TestBridgeGuestSupported(t *testing.T) {
	ini := func(kv ...string) *avd.Ini {
		c := avd.NewIni()
		for k := 0; k+1 < len(kv); k += 2 {
			c.Set(kv[k], kv[k+1])
		}
		return c
	}
	cases := []struct {
		name string
		cfg  *avd.Ini
		want bool
	}{
		{"x86_64 (what create writes)", ini("abi.type", "x86_64", "hw.cpu.arch", "x86_64"), true},
		{"abi.type arm64", ini("abi.type", "arm64"), false},
		{"abi.type arm64-v8a", ini("abi.type", "arm64-v8a"), false},
		{"abi.type aarch64", ini("abi.type", "aarch64"), false},
		{"hw.cpu.arch arm64", ini("hw.cpu.arch", "arm64"), false},
		{"hw.cpu.arch arm64-v8a", ini("hw.cpu.arch", "arm64-v8a"), false},
		{"hw.cpu.arch aarch64", ini("hw.cpu.arch", "aarch64"), false},
		{"upper case", ini("abi.type", "ARM64-V8A"), false},
		{"x86_64 abi, arm64 cpu", ini("abi.type", "x86_64", "hw.cpu.arch", "arm64"), false},
		{"arm64 abi, x86_64 cpu", ini("abi.type", "arm64-v8a", "hw.cpu.arch", "x86_64"), false},
		{"32-bit arm is not arm64", ini("abi.type", "armeabi-v7a", "hw.cpu.arch", "arm"), true},
		{"x86", ini("abi.type", "x86", "hw.cpu.arch", "x86"), true},
		{"arch keys missing", ini("hw.cpu.ncore", "4"), true},
		{"arch keys empty", ini("abi.type", "", "hw.cpu.arch", ""), true},
		{"other keys say arm64", ini("image.sysdir.1", "arm64-v8a", "tag.id", "arm64"), true},
		{"empty config (unreadable)", avd.NewIni(), true},
		{"no config", nil, true},
	}
	for _, c := range cases {
		if got := bridgeGuestSupported(c.cfg); got != c.want {
			t.Errorf("%s: bridgeGuestSupported = %v, want %v", c.name, got, c.want)
		}
	}
}
