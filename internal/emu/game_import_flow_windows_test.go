//go:build windows

// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"context"
	"slices"
	"strings"
	"testing"

	"modavd/internal/logx"
)

const (
	flowSum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	flowDF  = "Filesystem 1K-blocks Used Available Use% Mounted on\n/dev/block 100000000 1 99999999 1% /data\n"
)

// flowGuests are a source and a target answering importGame's queries. The target holds the app already when
// installed is true.
func flowGuests(t *testing.T, installed bool) (src, dst *scriptedGuest) {
	t.Helper()
	m := &Manager{Root: managerTestRoot(t, 1), Log: logx.Nop()}
	apk := "package:/data/app/~~x/example.game-1/base.apk\n"
	common := func(script string) (string, bool) {
		switch {
		case strings.Contains(script, "pm path --user 0"):
			return apk, true
		case strings.Contains(script, "sha256sum"):
			return flowSum + "  /data/app/~~x/example.game-1/base.apk\n", true
		}
		return "", false
	}
	src = &scriptedGuest{reply: func(script string) (string, error) {
		if out, ok := common(script); ok {
			return out, nil
		}
		switch {
		case strings.Contains(script, "-printf '%s"):
			if strings.Contains(script, "/sdcard/") {
				return "200", nil
			}
			return "100", nil
		}
		return "", nil
	}}
	dst = &scriptedGuest{reply: func(script string) (string, error) {
		if out, ok := common(script); ok {
			return out, nil
		}
		switch {
		case strings.Contains(script, "pm list packages -u"):
			if installed {
				return "package:example.game\n", nil
			}
			return "", nil
		case strings.Contains(script, "pm list packages -U"):
			return "package:example.game uid:10123\n", nil
		case strings.Contains(script, "df -Pk"):
			return flowDF, nil
		case strings.Contains(script, "pm install"):
			return "Success\n", nil
		}
		return "", nil
	}}
	src.m, dst.m = m, m
	return src, dst
}

func (g *scriptedGuest) index(sub string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.IndexFunc(g.scripts, func(s string) bool { return strings.Contains(s, sub) })
}

// ModAVD/ModAVD#272: the leftovers are gone before the space check, whether or not the app is installed yet, and
// the ones in the package's own directories are removed as the app's UID.
func TestImportGameRemovesStaleDirsBeforeCopying(t *testing.T) {
	for _, installed := range []bool{false, true} {
		name := "installing"
		if installed {
			name = "already installed"
		}
		t.Run(name, func(t *testing.T) {
			fakeStream(t, func(read, write string) int {
				if strings.HasPrefix(write, "cat > ") {
					return 100
				}
				return 200
			})
			src, dst := flowGuests(t, installed)
			sd, dd := src.device(), dst.device()
			g := ImportGame{Package: "example.game", Data: []string{"files/assets"}}
			res, err := importGame(context.Background(), sd, dd, g, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.ResourceBytes != 200 {
				t.Fatalf("result=%+v", res)
			}
			tmp := dst.index("-exec rm -rf")
			df := dst.index("df -Pk")
			if tmp < 0 || df < 0 || tmp > df || !strings.Contains(dst.scripts[tmp], "/data/local/tmp") || strings.Contains(dst.scripts[tmp], "su 10123") {
				t.Fatalf("/data/local/tmp cleanup at %d, space check at %d: %q", tmp, df, dst.scripts)
			}
			app := "/sdcard/Android/data/example.game/files"
			appClean := -1
			for i, s := range dst.scripts {
				if strings.Contains(s, "-exec rm -rf") && strings.Contains(s, app) && strings.Contains(s, "su 10123,10123,9997") {
					appClean = i
					break
				}
			}
			stage := dst.index(app + "/.modavd-import.XXXXXXXX") // the first staging directory in the app's directory
			if appClean < 0 || stage < 0 || appClean > stage {
				t.Fatalf("app directory cleanup at %d, staging at %d: %q", appClean, stage, dst.scripts)
			}
			if installed && appClean > df {
				t.Fatalf("an installed app's directories must be cleaned before the space check: %d > %d", appClean, df)
			}
		})
	}
}
