// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/logx"
)

// scriptedGuest answers the shell commands of the import code the way a guest would, and records every script.
// Nothing here starts adb: importDevice.exec stands in for it.
type scriptedGuest struct {
	mu      sync.Mutex
	scripts []string
	reply   func(script string) (string, error)
}

func (g *scriptedGuest) device() importDevice {
	return importDevice{m: &Manager{Log: logx.Nop()}, exec: func(_ context.Context, args ...string) (string, error) {
		script := args[len(args)-1]
		g.mu.Lock()
		g.scripts = append(g.scripts, script)
		g.mu.Unlock()
		if strings.Contains(script, "mktemp -d") {
			// mktemp -d '<parent>/.modavd-import.XXXXXXXX'
			parent := script[strings.Index(script, "&& mktemp -d '")+len("&& mktemp -d '"):]
			parent = parent[:strings.Index(parent, "/.modavd-import.")]
			return parent + "/.modavd-import.AbCd1234\n", nil
		}
		if g.reply != nil {
			return g.reply(script)
		}
		return "", nil
	}}
}

func (g *scriptedGuest) ran(sub string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, s := range g.scripts {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func fakeStream(t *testing.T, bytes int, p *importProgress) {
	t.Helper()
	saved := importStreamFunc
	t.Cleanup(func() { importStreamFunc = saved })
	importStreamFunc = func(context.Context, importDevice, importDevice, string, string, func(io.Writer, io.Reader) error) error {
		p.Write(make([]byte, bytes))
		return nil
	}
}

// ModAVD/ModAVD#271: an item whose transferred size differs from the size counted at the start is not published.
func TestCopyResourceDoesNotPublishChangedSource(t *testing.T) {
	const full = "/sdcard/Android/data/example.game/files/assets"
	for _, tc := range []struct {
		name      string
		got, want int
		publish   bool
	}{
		{"shorter than counted", 150, 200, false},
		{"longer than counted", 250, 200, false},
		{"as counted", 200, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &importProgress{done: 1000} // earlier items of the same import
			fakeStream(t, tc.got, p)
			g := &scriptedGuest{}
			dev := g.device()
			err := copyResource(context.Background(), dev, dev, full, int64(tc.want), p)
			if tc.publish {
				if err != nil {
					t.Fatal(err)
				}
			} else if errs.CodeOf(err) != errs.Internal || !strings.Contains(err.Error(), "源文件大小在导入期间发生变化") {
				t.Fatalf("err=%v", err)
			}
			if g.ran("publish()") != tc.publish {
				t.Fatalf("published=%v scripts=%q", g.ran("publish()"), g.scripts)
			}
			// Only this item's staging directory is removed, whatever happened.
			if !g.ran("rm -rf '/sdcard/Android/data/example.game/files/.modavd-import.AbCd1234'") {
				t.Fatalf("staging directory was not removed: %q", g.scripts)
			}
			if g.ran("rm -rf '" + full) {
				t.Fatalf("removed the final directory: %q", g.scripts)
			}
		})
	}
}
