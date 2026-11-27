// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package gamesource

import (
	"bytes"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"modavd/internal/errs"
)

// mumuInt reads a number the control program writes either as a JSON number or as a string.
type mumuInt int

func (n *mumuInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	v, err := strconv.Atoi(s)
	*n = mumuInt(v)
	return err
}

type mumuRow struct {
	Index     *mumuInt `json:"index"`
	Name      *string  `json:"name"`
	Running   bool     `json:"is_process_started"`
	Android   bool     `json:"is_android_started"`
	State     string   `json:"player_state"`
	LaunchErr mumuInt  `json:"launch_err_code"`
	Port      mumuInt  `json:"adb_port"`
	Error     mumuInt  `json:"error_code"`
}

func (r mumuRow) player() (Player, bool) {
	if r.Index == nil || r.Name == nil || *r.Index < 0 {
		return Player{}, false
	}
	// Without administrator rights is_android_started stays false for a running instance.
	ready := r.Android || (r.Running && r.State == "start_finished" && r.LaunchErr == 0)
	return Player{Source: Source{"mumu", int(*r.Index)}, Name: *r.Name, Ready: ready, Running: r.Running, Port: int(r.Port), Error: int(r.Error)}, true
}

func mumuRowOf(raw json.RawMessage) (Player, bool) {
	var r mumuRow
	if json.Unmarshal(raw, &r) != nil {
		return Player{}, false
	}
	return r.player()
}

// parseMuMuPlayers reads the output of `info -v all`. Besides the usual object keyed by instance number the
// output can hold a single instance object, and tracking records or log lines around it. Every top-level JSON
// value in the output is looked at, and only the instance rows (those with an index and a name) are used.
func parseMuMuPlayers(out []byte) ([]Player, error) {
	var found []Player
	values := 0
	for pos := 0; ; {
		n := bytes.IndexByte(out[pos:], '{')
		if n < 0 {
			break
		}
		pos += n
		dec := json.NewDecoder(bytes.NewReader(out[pos:]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			pos++
			continue
		}
		pos += int(dec.InputOffset())
		values++
		if p, ok := mumuRowOf(raw); ok {
			found = append(found, p)
			continue
		}
		var keyed map[string]json.RawMessage
		if json.Unmarshal(raw, &keyed) != nil {
			continue
		}
		for _, v := range keyed {
			if p, ok := mumuRowOf(v); ok {
				found = append(found, p)
			}
		}
	}
	if values == 0 {
		return nil, errs.Newf(errs.Internal, "MuMu info：输出里没有可读的实例信息：%s", snippet(out))
	}
	sort.SliceStable(found, func(a, b int) bool { return found[a].Index < found[b].Index })
	result := make([]Player, 0, len(found))
	for _, p := range found {
		if len(result) == 0 || result[len(result)-1].Index != p.Index {
			result = append(result, p)
		}
	}
	return result, nil
}

func snippet(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
