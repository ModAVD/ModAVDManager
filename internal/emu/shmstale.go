package emu

import (
	"fmt"
	"strings"

	"modavd/internal/host"
	"modavd/internal/root"
)

// warnStaleScreenshotShm runs before an instance's emulator is launched. Its screenshot region
// (SHM_videmulator<console port>) only exists while an emulator shares frames, so one that is already there was left by
// the last emulator on this port and is kept alive by a program still holding it. The new emulator then gets that old
// region back, at the old size, and cannot share frames when the resolution went up (ModAVD/ModAVD#210; emulators
// before that fix even hung). Only a warning: the boot goes on, screenshots fall back to the slower path.
func (m *Manager) warnStaleScreenshotShm(i int) {
	if !root.CurrentPlatform().Windows() {
		return
	}
	name := shmName(m.Root.Ports(), i)
	if !host.SharedMemoryExists(name) {
		return
	}
	holders, err := host.SharedMemoryHolders(name)
	m.Log.Warnf("%s", staleShmMessage(i, name, holders, err))
}

// staleShmMessage is the warning of warnStaleScreenshotShm.
func staleShmMessage(i int, name string, holders []host.Process, err error) string {
	var who string
	switch {
	case len(holders) > 0:
		names := make([]string, 0, len(holders))
		for _, p := range holders {
			names = append(names, fmt.Sprintf("%s（pid %d）", p.Name, p.PID))
		}
		who = "拿着它的程序：" + strings.Join(names, "、")
	case err != nil:
		who = fmt.Sprintf("查不出是哪个程序（%v），多半是 MAA 这类用 ModAVD 截图的工具", err)
	default:
		who = "没认出是哪个程序，多半是 MAA 这类用 ModAVD 截图的工具"
	}
	return fmt.Sprintf("实例 %d 的截图共享内存 %s 还被上次开机后没退出的程序拿着（%s）。"+
		"这次开机截图共享可能开不起来，用它截图的工具会变慢；把这些程序退出后再开机就好", i, name, who)
}
