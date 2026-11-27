// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package gamesource

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
)

const mumuDict = `{"0":{"adb_port":16384,"error_code":0,"index":"0","is_android_started":true,"is_process_started":true,"name":"MuMu","player_state":"start_finished"},` +
	`"1":{"index":"1","is_android_started":false,"is_process_started":false,"name":"MuMu-1"}}`

func TestParseMuMuCleanDict(t *testing.T) {
	rows, err := parseMuMuPlayers([]byte(mumuDict))
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if r := rows[0]; r.Source != (Source{"mumu", 0}) || r.Name != "MuMu" || !r.Ready || !r.Running || r.Port != 16384 {
		t.Fatal(r)
	}
	if r := rows[1]; r.Index != 1 || r.Running || r.Ready || r.Port != 0 {
		t.Fatal(r)
	}
	if rows, err := parseMuMuPlayers([]byte("{}")); err != nil || rows == nil || len(rows) != 0 {
		t.Fatalf("empty: %v %v", rows, err)
	}
}

// Tracking records and log lines around the JSON must not hide the instances.
func TestParseMuMuMixedWithTrackingAndLogs(t *testing.T) {
	out := "[2026-11-27 12:00:01] MuMuManager start\r\n" +
		`add record:{"event":"info","count":1,"nested":{"index":"7"}}` + "\r\n" +
		mumuDict + "\r\n" +
		"[WARN] unfinished {not json\r\n" +
		`add record:{"event":"exit"}` + "\r\n"
	rows, err := parseMuMuPlayers([]byte(out))
	if err != nil || len(rows) != 2 || rows[0].Index != 0 || rows[1].Index != 1 || rows[0].Name != "MuMu" || !rows[0].Ready {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
}

func TestParseMuMuSingleInstanceObject(t *testing.T) {
	out := `add record:{"event":"info"}` + "\n" +
		`{"adb_port":16416,"index":"2","is_android_started":true,"is_process_started":true,"name":"MuMu-2","player_state":"start_finished"}`
	rows, err := parseMuMuPlayers([]byte(out))
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if r := rows[0]; r.Source != (Source{"mumu", 2}) || r.Name != "MuMu-2" || !r.Ready || r.Port != 16416 {
		t.Fatal(r)
	}
	// The index can also be a JSON number.
	rows, err = parseMuMuPlayers([]byte(`{"index":3,"name":"n"}`))
	if err != nil || len(rows) != 1 || rows[0].Index != 3 {
		t.Fatalf("numeric index: %v %v", rows, err)
	}
}

// Without administrator rights is_android_started stays false for a running instance.
func TestParseMuMuReadyWithoutAdministrator(t *testing.T) {
	row := func(extra string) string {
		return `{"index":"0","name":"MuMu","adb_port":16384,"is_android_started":false,"is_process_started":true` + extra + `}`
	}
	for _, tc := range []struct {
		name, out string
		ready     bool
	}{
		{"start_finished", row(`,"player_state":"start_finished"`), true},
		{"start_finished with zero launch error", row(`,"player_state":"start_finished","launch_err_code":0`), true},
		{"launch error", row(`,"player_state":"start_finished","launch_err_code":-5`), false},
		{"still starting", row(`,"player_state":"starting"`), false},
		{"no state", row(``), false},
		{"process gone", strings.Replace(row(`,"player_state":"start_finished"`), `"is_process_started":true`, `"is_process_started":false`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := parseMuMuPlayers([]byte(tc.out))
			if err != nil || len(rows) != 1 || rows[0].Ready != tc.ready {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
		})
	}
}

// Output without a single instance row is an error row in the list, unless it is the empty object of a MuMu that has no
// instance.
func TestParseMuMuOutputWithoutInstanceRows(t *testing.T) {
	for _, tc := range []struct{ name, out string }{
		{"only tracking", `add record:{"event":"info","count":1}` + "\n"},
		{"error reply", `{"errcode":-1,"errmsg":"no such command"}`},
		{"tracking and logs", "[INFO] start\n" + `add record:{"event":"info"}` + "\n" + `add record:{"event":"exit"}` + "\n"},
		{"dict without names", `{"0":{"index":"0"},"1":{"index":"1"}}`},
	} {
		if rows, err := parseMuMuPlayers([]byte(tc.out)); err == nil {
			t.Errorf("%s: read as %v", tc.name, rows)
		}
	}
	for _, out := range []string{"{}", "{}\r\n", `add record:{}` + "\n{}"} {
		if rows, err := parseMuMuPlayers([]byte(out)); err != nil || len(rows) != 0 {
			t.Errorf("%q: rows=%v err=%v", out, rows, err)
		}
	}
}

func TestParseMuMuRejectsOutputWithoutJSON(t *testing.T) {
	if _, err := parseMuMuPlayers([]byte("access denied")); err == nil {
		t.Fatal("accepted output without JSON")
	}
}

// The wait for a running instance ends at once with the fallback; it does not wait for is_android_started.
func TestReadyUsesPlayerStateFallback(t *testing.T) {
	calls := 0
	i := installation{kind: "mumu", run: func(_ context.Context, args ...string) ([]byte, error) {
		calls++
		if args[0] != "info" {
			t.Errorf("unexpected command %v", args)
		}
		return []byte(`add record:{"event":"info"}` + "\n" + `{"0":{"index":"0","name":"MuMu","adb_port":16384,"is_android_started":false,"is_process_started":true,"player_state":"start_finished"}}`), nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := i.ready(ctx, Source{"mumu", 0})
	if err != nil || p.Port != 16384 || !p.Ready || calls != 1 {
		t.Fatalf("player=%+v err=%v calls=%d", p, err, calls)
	}
}

func TestMuMuExeFindsOldShellDirectory(t *testing.T) {
	root := t.TempDir()
	touch := func(parts ...string) string {
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	oldOnly, both := filepath.Join(root, "old"), filepath.Join(root, "both")
	wantOld := touch("old", "shell", "MuMuManager.exe")
	touch("both", "shell", "MuMuManager.exe")
	wantNew := touch("both", "nx_main", "MuMuManager.exe")
	if got := mumuExe([]string{"relative", filepath.Join(root, "none"), oldOnly}); got != wantOld {
		t.Fatalf("old layout: %q", got)
	}
	if got := mumuExe([]string{both}); got != wantNew {
		t.Fatalf("nx_main must win: %q", got)
	}
	if got := mumuExe([]string{"", "relative", filepath.Join(root, "none")}); got != "" {
		t.Fatalf("found %q", got)
	}
}

// One player that cannot be read is marked on its own row; the other players are still listed.
func TestListKeepsOtherSourcesWhenOneFails(t *testing.T) {
	mumu := installation{kind: "mumu", run: func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("MuMuManager.exe info: exit status 1")
	}}
	ld := installation{kind: "ldplayer", run: func(_ context.Context, args ...string) ([]byte, error) {
		if args[0] != "list2" {
			t.Errorf("unexpected command %v", args)
		}
		return []byte("0,Offline,0,0,0,-1,-1\r\n"), nil
	}}
	rows, err := listAll(context.Background(), []installation{mumu, ld})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if r := rows[0]; r.Kind != "mumu" || r.Index != -1 || r.Error == 0 || !strings.Contains(r.Message, "exit status 1") || r.Valid() {
		t.Fatal(r)
	}
	if r := rows[1]; r.Source != (Source{"ldplayer", 0}) || r.Name != "Offline" || r.Error != 0 {
		t.Fatal(r)
	}
}

func TestListStopsWhenCanceled(t *testing.T) {
	mumu := installation{kind: "mumu", run: func(context.Context, ...string) ([]byte, error) {
		return nil, errs.New(errs.Canceled, "已取消")
	}}
	if _, err := listAll(context.Background(), []installation{mumu}); errs.CodeOf(err) != errs.Canceled {
		t.Fatalf("err=%v", err)
	}
}
