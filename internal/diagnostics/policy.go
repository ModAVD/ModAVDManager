// Copyright 2026 Ji Mao Cai
// Package diagnostics holds the per-instance diagnostic policy shared by log retention and problem collection.
package diagnostics

import (
	"fmt"
	"strconv"
	"time"

	"modavd/internal/jsonord"
	"modavd/internal/root"
)

const (
	MaxBytes         int64  = 4 << 30
	CollectLogBytes  int64  = 1 << 30
	CollectBootBytes int64  = 300 << 20
	MinFreeBytes     uint64 = 20 << 30
	MaxAge                  = 30 * 24 * time.Hour
	BootKeep                = 20
	CollectKeep             = 30
)

type State struct {
	Enabled   bool
	Since     string
	CreatedAt string
}

func Of(o *jsonord.Obj, i int) State {
	m := o.Get("instances").Obj().Get(strconv.Itoa(i)).Obj()
	on, _ := m.Get("diagnostics").BoolVal()
	return State{Enabled: on, Since: text(m.Get("diagnosticsSince")), CreatedAt: text(m.Get("createdAt"))}
}

func text(v *jsonord.Value) string { s, _ := v.Str(); return s }

// Read is strict: corrupt metadata must never silently turn an enabled instance's deletion policy off.
func Read(r *root.Root) (map[int]State, error) {
	v, err := r.ReadMetadata()
	if err != nil {
		return nil, err
	}
	return Parse(v.Obj())
}

func Parse(o *jsonord.Obj) (map[int]State, error) {
	states := map[int]State{}
	instances := o.Get("instances").Obj()
	for _, key := range instances.Keys() {
		i, err := strconv.Atoi(key)
		if err != nil || !root.ValidIndex(i) {
			continue
		}
		m := instances.Get(key).Obj()
		if m.Has("diagnostics") {
			if _, ok := m.Get("diagnostics").BoolVal(); !ok {
				return nil, fmt.Errorf("实例 %d diagnostics 不是布尔值，已跳过日志删除", i)
			}
		}
		s := Of(o, i)
		if s.Since != "" {
			if _, err := time.Parse(time.RFC3339Nano, s.Since); err != nil {
				return nil, fmt.Errorf("实例 %d diagnosticsSince 无效，已跳过日志删除", i)
			}
		}
		states[i] = s
	}
	return states, nil
}

// Set preserves unknown instance keys and the original enable time on repeated on requests.
// The caller holds the instance config lock, so deletion cannot recreate this metadata entry midway.
func Set(r *root.Root, i int, enabled bool, now time.Time) error {
	return r.EditInstanceMeta(i, func(m *jsonord.Obj) error {
		old, _ := m.Get("diagnostics").BoolVal()
		m.Set("diagnostics", jsonord.NewBool(enabled))
		if enabled {
			if !old || text(m.Get("diagnosticsSince")) == "" {
				m.Set("diagnosticsSince", jsonord.NewString(now.UTC().Format(time.RFC3339Nano)))
			}
		} else {
			m.Delete("diagnosticsSince")
		}
		return nil
	})
}
