// Copyright 2026 jinghero
package emu

import (
	"context"
	"strconv"

	"modavd/internal/guest"
	"modavd/internal/jsonord"
)

const appKeepaliveKey = "appKeepalive"

// Keep background apps if an existing setting cannot be interpreted. An absent
// setting is the supported legacy configuration and uses the default (off).
func keepBackgroundApps(meta *jsonord.Obj) bool {
	field := meta.Get(appKeepaliveKey)
	on, valid := field.BoolVal()
	return on || (field != nil && !valid)
}

func (m *Manager) setupAppKeepalive(ctx context.Context, i int) {
	meta, err := m.Root.ReadMetadata()
	if err != nil {
		m.Log.Warnf("实例 %d 无法读取应用保活设置，跳过后台应用清理: %v", i, err)
		return
	}
	if keepBackgroundApps(meta.Obj().Get("instances").Obj().Get(strconv.Itoa(i)).Obj()) {
		return
	}
	if script, ok := m.pushGuestScript(ctx, i, guest.AppKeepalive); ok {
		if result := m.shell(ctx, i, "sh "+script+" daemon", 0); result.Code != 0 {
			m.Log.Warnf("实例 %d 后台应用清理未启动: %s", i, tail(result.Out, 300))
		}
	}
}
