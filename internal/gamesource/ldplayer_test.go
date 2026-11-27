// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package gamesource

import (
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
