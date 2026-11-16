package emu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"modavd/internal/guest"
	"modavd/internal/root"
)

// The Vulkan display mode (guest.VulkanMode, ModAVD/ModAVD#222). Honkai: Star Rail picks OpenGL ES at its first start,
// and on gfxstream's GLES it closes about 15 s later; in Vulkan mode it runs. For the listed games that have not
// chosen a mode (in the game's settings, or by an earlier pass), a guest watcher writes the Vulkan mode before the game
// starts: installed before the boot or after it. A mode chosen in the game is never overwritten. The script's header
// has the files and what was measured.
//
// On by default, without a switch. Windows only so far: that is where it was measured.

// vulkanModeTimeout covers the script's wait for the shared storage folder (20 s) and one pass.
const vulkanModeTimeout = 60 * time.Second

// setupVulkanMode starts the watcher, which makes one pass before it returns: a game already installed is set before
// the boot is reported as done and a script starts it.
func (m *Manager) setupVulkanMode(ctx context.Context, i int) {
	if !root.CurrentPlatform().Windows() {
		return
	}
	remote, ok := m.pushGuestScript(ctx, i, guest.VulkanMode)
	if !ok {
		return
	}
	started := m.shell(ctx, i, "su 0 sh "+remote+" daemon", vulkanModeTimeout).Out
	status := m.shell(ctx, i, "su 0 sh "+remote+" status", 0).Out
	if warn, msg := vulkanModeReport(i, started, status); warn {
		m.Log.Warnf("%s", msg)
	} else {
		m.Log.Infof("%s", msg)
	}
}

// vulkanModeReport judges `modavd-vkmode.sh daemon` + `status`: running when a watcher pid is listed. The message
// names the listed games that are installed and what the script last said about each; a game it could not set makes
// it a warning.
func vulkanModeReport(i int, started, status string) (warn bool, msg string) {
	lines := parseStatusLines(status)
	if lines["watchers"] == "" {
		return true, fmt.Sprintf("实例 %d 游戏显示模式预设没起来: %s", i, tail(strings.TrimSpace(started)+" / "+status, 300))
	}
	var games []string
	for _, line := range outputLines(status) {
		pkg, state, ok := strings.Cut(line, ":")
		state = strings.TrimSpace(state)
		if !ok || pkg == "watchers" || state == "" || state == "not installed" {
			continue
		}
		if strings.HasPrefix(state, "cannot ") {
			warn = true
		}
		games = append(games, pkg+"（"+vulkanModeState(state)+"）")
	}
	if len(games) == 0 {
		return false, fmt.Sprintf("实例 %d 游戏显示模式预设: 守护已起（pid %s），名单里的游戏都没装", i, lines["watchers"])
	}
	return warn, fmt.Sprintf("实例 %d 游戏显示模式预设: 守护已起（pid %s）；%s", i, lines["watchers"], strings.Join(games, "，"))
}

// vulkanModeState puts the script's notes into the log's language; one it does not know stays as it is.
func vulkanModeState(state string) string {
	switch {
	case state == "set to Vulkan":
		return "已预设为 Vulkan"
	case state == "display mode chosen already":
		return "已选过显示模式，不动"
	case state == "running, set once it exits":
		return "游戏开着，等它退出后再写"
	case state == "nothing done yet":
		return "还没处理"
	case state == "cannot write the display mode files":
		return "显示模式文件写不进去"
	case strings.HasPrefix(state, "cannot make "):
		return "建不了 " + strings.TrimPrefix(state, "cannot make ")
	}
	return state
}
