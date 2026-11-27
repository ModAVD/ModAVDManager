// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package gamesource

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"modavd/internal/adb"
	"modavd/internal/host"
)

// ldRecord is what the registry holds for one LDPlayer version (empty when it holds nothing).
type ldRecord struct{ version, dir, data string }

// ldInstallationsOf lists the registered installations in the order of records. The default folder on the system
// drive is only a guess for an install that left no registry record: any local user may create it, so it is used
// when no record exists at all, and never next to one.
func ldInstallationsOf(records []ldRecord, systemDrive string) []installation {
	var result []installation
	registered := false
	for _, r := range records {
		if r.dir == "" {
			continue
		}
		registered = true
		if !filepath.IsAbs(r.dir) {
			continue
		}
		vms := filepath.Join(r.dir, "vms")
		if filepath.IsAbs(r.data) {
			vms = r.data
		}
		result = append(result, installation{kind: "ldplayer", exe: filepath.Join(r.dir, "ldconsole.exe"), data: vms})
	}
	if registered {
		return result
	}
	for _, r := range records {
		base := filepath.Join(systemDrive+`\`, "leidian", "LDPlayer"+r.version)
		if filepath.IsAbs(base) {
			result = append(result, installation{kind: "ldplayer", exe: filepath.Join(base, "ldconsole.exe"), data: filepath.Join(base, "vms")})
		}
	}
	return result
}

// list2 is ANSI on Windows. Names can contain commas; the optional final fields are width, height and DPI.
var ldRow = regexp.MustCompile(`^(\d+),(.+?),-?\d+,-?\d+,([0-9]),(-?\d+),(-?\d+)(?:,\d+,\d+,\d+)?$`)

func parseLDPlayers(out []byte) ([]Player, error) {
	var list []Player
	for line := range strings.SplitSeq(strings.TrimSpace(adb.DecodeOutput(out)), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		f := ldRow.FindStringSubmatch(line)
		if f == nil {
			return nil, fmt.Errorf("ldconsole list2: %q", line)
		}
		index, err := strconv.Atoi(f[1])
		if err != nil {
			return nil, err
		}
		pid, err := strconv.ParseInt(f[5], 10, 32)
		if err != nil {
			return nil, err
		}
		list = append(list, Player{
			Source: Source{Kind: "ldplayer", Index: index}, Name: f[2],
			Running: pid > 0 || (f[4] != "-1" && f[4] != "0"), Ready: f[3] == "1",
			enginePID: uint32(max(pid, 0)),
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Index < list[j].Index })
	return list, nil
}

// VBox.log records the active NAT rule, including overrides. Check the listener
// belongs to this instance's VM before exposing the port to adb.
func ldPort(data string, p Player) int {
	if p.enginePID == 0 {
		return 0
	}
	candidate := 0
	f, err := os.Open(filepath.Join(data, "leidian"+strconv.Itoa(p.Index), "Logs", "VBox.log"))
	if err == nil {
		candidate = ldForwardedPort(f)
		f.Close()
	}
	if candidate == 0 {
		candidate = 5555 + 2*p.Index
	}
	if candidate <= 0 || candidate > 65535 {
		return 0
	}
	pid, ok, err := host.ListenerPID(candidate)
	if err != nil || !ok || pid != p.enginePID {
		return 0
	}
	return candidate
}

func ldForwardedPort(r io.Reader) int {
	scan := bufio.NewScanner(io.LimitReader(r, 4<<20))
	inRule, named := false, false
	port := 0
	for scan.Scan() {
		line := scan.Text()
		if strings.Contains(line, "[/Devices/") {
			if inRule && named && port > 0 {
				return port
			}
			inRule = strings.Contains(line, "/PortForwarding/")
			named = false
			port = 0
			continue
		}
		if !inRule {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 5 || f[3] != "=" {
			continue
		}
		switch f[1] {
		case "HostPort":
			n, e := strconv.ParseUint(f[4], 0, 16)
			if e == nil {
				port = int(n)
			}
		case "Name":
			named = f[4] == `"adb"`
		}
	}
	if inRule && named {
		return port
	}
	return 0
}
