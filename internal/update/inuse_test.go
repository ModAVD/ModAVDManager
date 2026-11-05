//go:build windows

package update

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
)

func TestShutdownHelperOwnershipAndArguments(t *testing.T) {
	u := &Updater{l: layout{root: t.TempDir()}}
	other := t.TempDir()
	emulator := filepath.Join(u.l.emulator(), "emulator.exe")
	cli := u.l.at("tools/modavd.exe")
	watchdog := []string{emulator, "-kill", "123", "-sleep", "20"}
	watch := []string{cli, "__bootwatch", u.l.root, "0", "123", "456"}
	change := func(args []string, i int, s string) []string {
		out := append([]string{}, args...)
		out[i] = s
		return out
	}
	cases := []struct {
		name, image string
		args        []string
		want        bool
	}{
		{"watchdog", emulator, watchdog, true},
		{"launcher omits exe suffix in argv0", emulator, change(watchdog, 0, strings.TrimSuffix(emulator, ".exe")), true},
		{"bootwatch", cli, watch, true},
		{"gui bootwatch", u.l.at("ModAVD.exe"), change(watch, 0, u.l.at("ModAVD.exe")), true},
		{"case insensitive root", cli, change(watch, 2, strings.ToUpper(u.l.root)), true},
		{"different SDK same name", filepath.Join(other, "sdk", "emulator", "emulator.exe"), watchdog, false},
		{"different manager same name", filepath.Join(other, "tools", "modavd.exe"), watch, false},
		{"different bootwatch root", cli, change(watch, 2, other), false},
		{"neighbor root prefix", cli, change(watch, 2, u.l.root+"-other"), false},
		{"relative bootwatch root", cli, change(watch, 2, "."), false},
		{"nested emulator copy", filepath.Join(u.l.emulator(), "other", "emulator.exe"), watchdog, false},
		{"netsimd", filepath.Join(u.l.emulator(), "netsimd.exe"), watchdog, false},
		{"unreadable argv", emulator, nil, false},
		{"normal emulator", emulator, []string{emulator, "-avd", "mas_0"}, false},
		{"extra watchdog args", emulator, append(append([]string{}, watchdog...), "-avd", "mas_0"), false},
		{"wrong watchdog option", emulator, change(watchdog, 1, "-avd"), false},
		{"wrong sleep option", emulator, change(watchdog, 3, "-avd"), false},
		{"missing sleep", emulator, watchdog[:3], false},
		{"zero target PID", emulator, change(watchdog, 2, "0"), false},
		{"overflow target PID", emulator, change(watchdog, 2, "4294967296"), false},
		{"zero sleep", emulator, change(watchdog, 4, "0"), false},
		{"negative sleep", emulator, change(watchdog, 4, "-1"), false},
		{"nonnumeric sleep", emulator, change(watchdog, 4, "forever"), false},
		{"normal manager", cli, []string{cli, "mcp"}, false},
		{"other internal helper", cli, change(watch, 1, "__orphan"), false},
		{"missing watch arg", cli, watch[:5], false},
		{"extra watch arg", cli, append(append([]string{}, watch...), "extra"), false},
		{"reserved instance", cli, change(watch, 3, "5"), false},
		{"out of range instance", cli, change(watch, 3, "10"), false},
		{"negative instance", cli, change(watch, 3, "-1"), false},
		{"zero watch PID", cli, change(watch, 4, "0"), false},
		{"invalid watch PID", cli, change(watch, 4, "bad"), false},
		{"zero creation time", cli, change(watch, 5, "0"), false},
		{"invalid creation time", cli, change(watch, 5, "18446744073709551616"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := u.shutdownHelper(c.image, c.args); got != c.want {
				t.Fatalf("shutdownHelper(%q, %q) = %v, want %v", c.image, c.args, got, c.want)
			}
		})
	}
}

func TestWaitUntilUnused(t *testing.T) {
	helper := processUse{PID: 111, Name: "emulator.exe", ShutdownHelper: true}
	watch := processUse{PID: 222, Name: "modavd.exe", ShutdownHelper: true}
	qemu := processUse{PID: 333, Name: "qemu-system-x86_64.exe"}
	cases := []struct {
		name     string
		states   [][]processUse
		errScan  int
		wantCode string
		wantPID  string
		wantWait time.Duration
	}{
		{name: "unused", states: [][]processUse{nil}},
		{name: "helpers exit separately", states: [][]processUse{{helper, watch}, {watch}, nil}, wantWait: 500 * time.Millisecond},
		{name: "timeout reports remaining helper", states: [][]processUse{{helper, watch}, {watch}}, wantCode: errs.Busy, wantPID: "222", wantWait: shutdownHelperWait},
		{name: "new helper shares deadline", states: [][]processUse{{helper}, {watch}}, wantCode: errs.Busy, wantPID: "222", wantWait: shutdownHelperWait},
		{name: "live instance", states: [][]processUse{{helper, qemu}}, wantCode: errs.Busy, wantPID: "333"},
		{name: "netsimd", states: [][]processUse{{{PID: 444, Name: "netsimd.exe"}}}, wantCode: errs.Busy, wantPID: "444"},
		{name: "manager", states: [][]processUse{{{PID: 555, Name: "modavd.exe"}}}, wantCode: errs.Busy, wantPID: "555"},
		{name: "unknown ownership", states: [][]processUse{{{PID: 666, Name: "emulator.exe", Unknown: true, ShutdownHelper: true}}}, wantCode: errs.Busy, wantPID: "666"},
		{name: "full recheck finds new instance", states: [][]processUse{{helper}, {qemu}}, wantCode: errs.Busy, wantPID: "333", wantWait: 250 * time.Millisecond},
		{name: "scan fails initially", errScan: 1, wantCode: errs.Internal},
		{name: "scan fails after wait", states: [][]processUse{{helper}}, errScan: 2, wantCode: errs.Internal, wantWait: 250 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start := time.Unix(1, 0)
			clock := start
			scans := 0
			err := waitUntilUnused("装", func(programs bool) ([]processUse, error) {
				if !programs {
					t.Fatal("recheck omitted manager processes")
				}
				scans++
				if scans == c.errScan {
					return nil, errors.New("scan unavailable")
				}
				return c.states[min(scans-1, len(c.states)-1)], nil
			}, func() time.Time { return clock }, func(d time.Duration) {
				if d <= 0 || d > 250*time.Millisecond || clock.Sub(start)+d > shutdownHelperWait {
					t.Fatalf("invalid wait %v at %v", d, clock.Sub(start))
				}
				clock = clock.Add(d)
			})
			if errs.CodeOf(err) != c.wantCode || clock.Sub(start) != c.wantWait {
				t.Fatalf("wait = %v, err = %v; want wait %v, code %q", clock.Sub(start), err, c.wantWait, c.wantCode)
			}
			if c.wantPID != "" && !strings.Contains(err.Error(), "pid "+c.wantPID) {
				t.Fatalf("remaining occupant missing from %v", err)
			}
			if c.wantPID == "222" && strings.Contains(err.Error(), "pid 111") {
				t.Fatalf("reported exited process: %v", err)
			}
		})
	}
}

func TestInstallRechecksAfterShutdownHelper(t *testing.T) {
	for _, newOccupant := range []bool{false, true} {
		t.Run(map[bool]string{false: "helper exits", true: "new occupant blocks"}[newOccupant], func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			r := writeRoot(t, 36, nil)
			f := newSources(t, 37, zipOf(t, 37, pkgFiles(37, nil), false))
			u := f.updater(r)
			scans := 0
			u.scan = func(programs bool) ([]processUse, error) {
				if !programs || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-36" {
					t.Fatal("swap started before full occupancy check completed")
				}
				scans++
				if scans == 1 {
					return []processUse{{PID: 111, Name: "emulator.exe", ShutdownHelper: true}}, nil
				}
				if newOccupant {
					return []processUse{{PID: 222, Name: "qemu-system-x86_64.exe"}}, nil
				}
				return nil, nil
			}
			st, err := u.Install(context.Background())
			if newOccupant {
				if errs.CodeOf(err) != errs.Busy || st.Updated || st.Phase != phaseStaged || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-36" {
					t.Fatalf("new occupant did not preserve old package and stage: %+v %v", st, err)
				}
			} else if err != nil || !st.Updated || read(t, r, "sdk/emulator/emulator.exe") != "emulator mas-37" {
				t.Fatalf("install after shutdown: %+v %v", st, err)
			}
		})
	}
}
