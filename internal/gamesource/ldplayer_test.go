// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package gamesource

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLDList2(t *testing.T) {
	rows, err := parseLDPlayers([]byte("2,Game, test,8,9,1,101,202,1920,1080,240\r\n0,Offline,0,0,0,-1,-1\r\n"))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].Index != 0 || rows[0].Running || rows[0].Ready {
		t.Fatal(rows[0])
	}
	r := rows[1]
	if r.Source != (Source{"ldplayer", 2}) || r.Name != "Game, test" || !r.Ready || !r.Running || r.enginePID != 202 {
		t.Fatal(r)
	}
	starting, err := parseLDPlayers([]byte("1,Starting,16124758,2558758,2,57424,-1,0,0,0"))
	if err != nil || len(starting) != 1 || !starting[0].Running || starting[0].Ready || starting[0].enginePID != 0 {
		t.Fatalf("starting %v: %v", starting, err)
	}
	if _, err := parseLDPlayers([]byte("1,broken")); err == nil {
		t.Fatal("accepted incomplete row")
	}
}
func TestLDRuntimeForwarding(t *testing.T) {
	log := `00:00 [/Devices/nic/PortForwarding/0/] (level 7)
00:00   HostPort <integer> = 0x00000123
00:00   Name <string> = "other"
00:00 [/Devices/nic/PortForwarding/1/] (level 7)
00:00   GuestPort <integer> = 0x000015b3
00:00   HostPort <integer> = 0x0000476d (18 285)
00:00   Name <string> = "adb"
00:00 [/Devices/nic/PortForwarding/2/] (level 7)`
	if got := ldForwardedPort(strings.NewReader(log)); got != 18285 {
		t.Fatalf("port %d", got)
	}
	if got := ldForwardedPort(strings.NewReader(strings.ReplaceAll(log, `"adb"`, `"other"`))); got != 0 {
		t.Fatalf("unrelated port %d", got)
	}
}

// The default folder on the system drive is a guess for an install without registry records, never an alternative
// to a registered one.
func TestLDFallbackOnlyWithoutRegistryRecords(t *testing.T) {
	drive, dir14, dir9 := t.TempDir(), t.TempDir(), t.TempDir()
	exes := func(list []installation) []string {
		var out []string
		for _, i := range list {
			out = append(out, i.exe)
		}
		return out
	}
	none := []ldRecord{{version: "14"}, {version: "9"}}
	got := ldInstallationsOf(none, drive)
	if len(got) != 2 || !strings.Contains(got[0].exe, "LDPlayer14") || !strings.Contains(got[1].exe, "LDPlayer9") || !strings.HasPrefix(got[0].exe, drive) {
		t.Fatalf("no record: %v", exes(got))
	}
	for name, records := range map[string][]ldRecord{
		"both":     {{version: "14", dir: dir14, data: filepath.Join(dir14, "data")}, {version: "9", dir: dir9}},
		"only 9":   {{version: "14"}, {version: "9", dir: dir9}},
		"relative": {{version: "14", dir: `relative\LDPlayer14`}, {version: "9"}},
	} {
		for _, i := range ldInstallationsOf(records, drive) {
			if strings.HasPrefix(i.exe, drive) {
				t.Fatalf("%s: used the fallback next to a record: %v", name, i.exe)
			}
		}
	}
	got = ldInstallationsOf([]ldRecord{{version: "14", dir: dir14, data: filepath.Join(dir14, "data")}, {version: "9", dir: dir9}}, drive)
	if len(got) != 2 || got[0].exe != filepath.Join(dir14, "ldconsole.exe") || got[0].data != filepath.Join(dir14, "data") ||
		got[1].exe != filepath.Join(dir9, "ldconsole.exe") || got[1].data != filepath.Join(dir9, "vms") {
		t.Fatalf("registered: %+v", got)
	}
	if got := ldInstallationsOf(none, ""); len(got) != 0 {
		t.Fatalf("no system drive: %v", exes(got))
	}
}
