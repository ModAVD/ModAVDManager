// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"context"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"modavd/internal/errs"
)

// The functions below take text the source emulator produced, or build commands from it. Nothing here starts adb.

var hostile = []string{
	"plain", "it's", `a"b`, "$(touch /sdcard/x)", "`id`", "a;rm -rf /", "a\nb", "a\r\nb", "a b", "'", "''", `\`, "a\\'b", "${IFS}x", "*", "中文",
}

func TestImportQuote(t *testing.T) {
	for _, s := range hostile {
		q := importQuote(s)
		if !strings.HasPrefix(q, "'") || !strings.HasSuffix(q, "'") {
			t.Fatalf("%q quoted as %s", s, q)
		}
		// Every single quote of the input is closed, double-quoted and reopened; nothing else is touched.
		if got := strings.ReplaceAll(q[1:len(q)-1], `'"'"'`, "'"); got != s || strings.Contains(strings.ReplaceAll(q[1:len(q)-1], `'"'"'`, ""), "'") {
			t.Fatalf("%q quoted as %s", s, q)
		}
	}
}

func TestImportQuoteWithRealShell(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	for _, s := range hostile {
		if strings.Contains(s, "\r") {
			continue // Git for Windows' sh drops a carriage return from its arguments
		}
		out, err := exec.Command(sh, "-c", "printf %s "+importQuote(s)).Output()
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != s {
			t.Fatalf("shell read %q for %q", out, s)
		}
	}
}

func TestResourcePath(t *testing.T) {
	for _, s := range []string{"files", "files/a/b.bin", "a b/c", "it's", "$(x)", "中文/文件", ".hidden", "a..b", "a.b/..c"} {
		if !resourcePath(s) {
			t.Errorf("rejected %q", s)
		}
	}
	for _, s := range []string{"", ".", "..", "../x", "a/../b", "a/..", "/abs", "a//b", "a/", "./a", `a\b`, `..\x`, "a\nb", "a\rb", "a\x00b", "a\tb", "a\x7fb"} {
		if resourcePath(s) {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestValidPackageName(t *testing.T) {
	for _, s := range []string{"a", "com.example.game", "com.example.game_2", "Com.Example2", "a.b1"} {
		if !validPackageName(s) {
			t.Errorf("rejected %q", s)
		}
	}
	for _, s := range []string{"", ".a", "1a", "_a", "a b", "a/b", "a-b", "a;b", "a$(b)", "a'b", "a\nb", "包名", "a:b"} {
		// "a.." is only rejected by the later path checks, so it is not asserted here.
		if s == "a.." {
			continue
		}
		if validPackageName(s) {
			t.Errorf("accepted %q", s)
		}
	}
}

func TestImportPublicCDPinsThePhysicalDirectory(t *testing.T) {
	got := importPublicCD("/sdcard/Android/data/example.game/files")
	want := `cd -P /sdcard && base=$PWD && cd -P './Android/data/example.game/files' && [ "$PWD" = "$base"/'Android/data/example.game/files' ]`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	// A name with quotes and substitutions only ever appears quoted.
	rel := `Android/data/example.game/it's $(id) "x"`
	got = importPublicCD("/sdcard/" + rel)
	if !strings.Contains(got, "cd -P "+importQuote("./"+rel)) || !strings.HasSuffix(got, "/"+importQuote(rel)+" ]") {
		t.Fatal(got)
	}
	if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(got, importQuote("./"+rel), ""), importQuote(rel), ""), "$(id)") {
		t.Fatalf("unquoted use of the name: %s", got)
	}
}

func TestApkPaths(t *testing.T) {
	const base, split = "package:/data/app/~~x/example-1/base.apk", "package:/data/app/~~x/example-1/split_config.arm64_v8a.apk"
	for _, tc := range []struct {
		name, out string
		want      []string // nil: error
	}{
		{"base only", base + "\n", []string{"/data/app/~~x/example-1/base.apk"}},
		{"splits are sorted by file name", split + "\r\n" + base + "\r\n", []string{"/data/app/~~x/example-1/base.apk", "/data/app/~~x/example-1/split_config.arm64_v8a.apk"}},
		{"empty", "", nil},
		{"no base", split + "\n", nil},
		{"error text instead of paths", "Failure [not installed for 0]\n", nil},
		{"outside /data/app", "package:/system/app/Example/base.apk\n", nil},
		{"parent directory", "package:/data/app/../../etc/base.apk\n", nil},
		{"not an apk", "package:/data/app/~~x/base.apk.sh\n", nil},
		{"same name twice", base + "\npackage:/data/app/~~y/example-2/base.apk\n", nil},
		{"without the prefix", "/data/app/~~x/example-1/base.apk\n", nil},
		{"backslash", `package:/data/app/~~x/ex\ample/base.apk` + "\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &scriptedGuest{reply: func(string) (string, error) { return tc.out, nil }}
			got, err := apkPaths(context.Background(), g.device(), "example.game")
			if tc.want == nil {
				if err == nil {
					t.Fatalf("accepted %q", got)
				}
				return
			}
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, err %v", got, err)
			}
		})
	}
}

func TestPackageList(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []string
		fail      bool
	}{
		{"sorted", "package:com.b\r\npackage:com.a\n", []string{"com.a", "com.b"}, false},
		{"none", "", []string{}, false},
		{"error text", "Error: Could not access the Package Manager.\n", nil, true},
		{"substitution in a name", "package:com.a$(id)\n", nil, true},
		{"a second field", "package:com.a installer:x\n", nil, true},
		{"empty name", "package:\n", nil, true},
		{"path as a name", "package:../x\n", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &scriptedGuest{reply: func(string) (string, error) { return tc.out, nil }}
			got, err := packageList(context.Background(), g.device(), "-3")
			if (err != nil) != tc.fail || (!tc.fail && !slices.Equal(got, tc.want)) {
				t.Fatalf("got %q, err %v", got, err)
			}
		})
	}
}

func TestListResourceDirKeepsOnlyPlainNames(t *testing.T) {
	out := "dassets\x00fb.bin\x00fa.bin\x00d..\x00d\x00f\x00d\x00dwith\nline\x00fback\\slash\x00d.hidden\x00"
	g := &scriptedGuest{reply: func(string) (string, error) { return out, nil }}
	got, err := listResourceDir(context.Background(), g.device(), "data/example.game", "files")
	if err != nil {
		t.Fatal(err)
	}
	want := []ResourceEntry{{".hidden", true}, {"a.bin", false}, {"assets", true}, {"b.bin", false}}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if len(g.scripts) != 1 || !strings.Contains(g.scripts[0], "'data/example.game/files'") || !strings.Contains(g.scripts[0], "-type d -printf") {
		t.Fatalf("script %q", g.scripts)
	}
}

func TestListResourceDirRejectsBadRelativePaths(t *testing.T) {
	for _, rel := range []string{"..", "../x", "a/../../b", "/etc", "a\nb", `a\b`, "a/", "./a"} {
		g := &scriptedGuest{reply: func(string) (string, error) { t.Errorf("asked the guest about %q", rel); return "", nil }}
		if _, err := listResourceDir(context.Background(), g.device(), "data/example.game", rel); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%q: err=%v", rel, err)
		}
	}
	// A name with a quote reaches the guest only inside the quoting.
	g := &scriptedGuest{}
	if _, err := listResourceDir(context.Background(), g.device(), "data/example.game", `it's $(id)`); err != nil {
		t.Fatal(err)
	}
	if len(g.scripts) != 1 || !strings.Contains(g.scripts[0], importQuote("data/example.game/it's $(id)")) {
		t.Fatalf("script %q", g.scripts)
	}
}

func TestCheckGuestSpaceReadsDf(t *testing.T) {
	const header = "Filesystem     1024-blocks    Used Available Capacity Mounted on\n"
	row := func(avail string) string {
		return header + "/dev/block/dm-9   1000000  400000 " + avail + "      40% /data\n"
	}
	const free = int64(600000) * 1024
	for _, tc := range []struct {
		name string
		out  string
		need int64
		want string // "": fits, else the expected error code
	}{
		{"fits", row("600000"), free - 1, ""},
		{"exactly the free space", row("600000"), free, ""},
		{"one byte more", row("600000"), free + 1, errs.PrecheckFailed},
		{"nothing needed", row("0"), 0, ""},
		{"CRLF", strings.ReplaceAll(row("600000"), "\n", "\r\n"), free, ""},
		{"no trailing newline", strings.TrimSuffix(row("600000"), "\n"), free, ""},
		{"warning before the table", "df: /storage/x: Permission denied\n" + row("600000"), free, ""},
		{"negative free space", row("-5"), 1, errs.PrecheckFailed},
		{"free space is not a number", row("many"), 1, errs.Internal},
		{"too few columns", header + "/dev/x 1 2\n", 1, errs.Internal},
		{"no output", "", 1, errs.Internal},
		{"only an error", "df: /nonexistent: No such file or directory\n", 1, errs.Internal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &scriptedGuest{reply: func(string) (string, error) { return tc.out, nil }}
			err := checkGuestSpace(context.Background(), g.device(), "/data", tc.need)
			switch {
			case tc.want == "" && err != nil:
				t.Fatal(err)
			case tc.want == errs.PrecheckFailed && errs.CodeOf(err) != errs.PrecheckFailed:
				t.Fatalf("err=%v", err)
			case tc.want == errs.Internal && (err == nil || errs.CodeOf(err) == errs.PrecheckFailed):
				t.Fatalf("err=%v", err)
			}
		})
	}
	g := &scriptedGuest{reply: func(string) (string, error) { return row("1"), nil }}
	err := checkGuestSpace(context.Background(), g.device(), "/data", 1<<20+1)
	if !strings.Contains(err.Error(), "需要 2 MiB") {
		t.Fatalf("rounding: %v", err)
	}
	if err := checkGuestSpace(context.Background(), g.device(), `/sdcard/it's $(id)`, 0); err != nil && errs.CodeOf(err) == errs.PrecheckFailed {
		t.Fatal(err)
	}
	if !strings.Contains(g.scripts[len(g.scripts)-1], "p="+importQuote(`/sdcard/it's $(id)`)+";") {
		t.Fatalf("target not quoted: %q", g.scripts)
	}
}

func TestImportSizeParsesTheTotal(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want int64
		fail bool
	}{
		{"12345", 12345, false}, {"0\n", 0, false}, {"4294967296\r\n", 4 << 30, false},
		{"", 0, true}, {"12 junk", 0, true}, {"find: x: No such file", 0, true}, {"-1x", 0, true},
	} {
		g := &scriptedGuest{reply: func(string) (string, error) { return tc.out, nil }}
		got, err := importSize(context.Background(), g.device(), []string{"/sdcard/Android/obb/example.game/it's.obb"})
		if (err != nil) != tc.fail || got != tc.want {
			t.Errorf("%q: got %d, err %v", tc.out, got, err)
		}
		if !strings.Contains(g.scripts[0], importQuote("/sdcard/Android/obb/example.game/it's.obb")) {
			t.Errorf("name not quoted: %q", g.scripts)
		}
	}
	g := &scriptedGuest{}
	if n, err := importSize(context.Background(), g.device(), nil); n != 0 || err != nil || len(g.scripts) != 0 {
		t.Fatalf("no files: %d %v %q", n, err, g.scripts)
	}
}

func TestAppIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"app uid", "package:example.game uid:10123\n", "10123,10123,9997"},
		{"CRLF", "package:example.game uid:10123\r\n", "10123,10123,9997"},
		{"among others", "package:example.game.extra uid:10200\npackage:example.game uid:10123\n", "10123,10123,9997"},
		{"root", "package:example.game uid:0\n", ""},
		{"system", "package:example.game uid:1000\n", ""},
		{"below the app range", "package:example.game uid:9999\n", ""},
		{"above the app range", "package:example.game uid:100000\n", ""},
		{"not a number", "package:example.game uid:10123,0\n", ""},
		{"only another package", "package:example.game.extra uid:10200\n", ""},
		{"nothing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &scriptedGuest{reply: func(string) (string, error) { return tc.out, nil }}
			got, err := appIdentity(context.Background(), g.device(), "example.game")
			if (err != nil) != (tc.want == "") || got != tc.want {
				t.Fatalf("got %q, err %v", got, err)
			}
		})
	}
}

func TestImportPortConflict(t *testing.T) {
	instances := map[int]bool{20000: true, 20001: true, 20002: true, 20010: true}
	for _, tc := range []struct {
		port int
		want bool
	}{
		{20000, true}, {20002, true}, {20010, true}, {5037, true}, {20049, true}, {20050, true},
		{20003, false}, {16384, false}, {5555, false}, {5554, false}, {0, false},
	} {
		if got := importPortConflict(tc.port, instances, 20049, 20050); got != tc.want {
			t.Errorf("port %d: %v", tc.port, got)
		}
	}
	if importPortConflict(7555, nil) || !importPortConflict(5037, nil) {
		t.Fatal("without instances or reserved ports")
	}
}
