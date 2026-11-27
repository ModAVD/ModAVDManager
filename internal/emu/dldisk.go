package emu

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"modavd/internal/guest"
	"modavd/internal/root"
)

// The download disk (guest.DownloadDisk, ModAVD/ModAVD#35). Some games write their resource packages a few KB at a
// time and fsync after every piece; Arknights does about 8 MiB/s that way, whatever the network. Their download
// folder goes onto a disk of its own where fsync returns at once and the folder as a whole is committed every 3 s.
// After a hard kill it is back at its last commit and the game resumes its downloads; what it has unpacked, accounts
// and settings are on /data as before. The script's header has the layers and the measurements.
//
// On by default: nobody finds a switch for this, and a download ten times slower is what people notice.
// Windows only so far: that is where it was measured (API 34 guest, GKI 6.1).

// envDownloadDisk set to "off" removes the disks at the next boot (the downloads in flight go back into the plain
// folders) and starts no watcher.
const envDownloadDisk = "MODAVD_DL_DISK"

// downloadDiskTimeout covers the script's wait for the shared storage (20 s), making an image (about a second) and
// the journal replay of one that was in use at a hard kill.
const downloadDiskTimeout = 90 * time.Second

// setupDownloadDisk starts the watcher, which makes one pass before it returns: the disks are in place before the
// boot is reported as done and a script starts the game. It has to come after the guest library fixes: their
// framework restart remounts the shared storage and takes everything mounted on it along.
func (m *Manager) setupDownloadDisk(ctx context.Context, i int) {
	if !root.CurrentPlatform().Windows() {
		return
	}
	remote, ok := m.pushGuestScript(ctx, i, guest.DownloadDisk)
	if !ok {
		return
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv(envDownloadDisk)), "off") {
		res := m.shell(ctx, i, "su 0 sh "+remote+" off", downloadDiskTimeout)
		m.Log.Infof("实例 %d 下载盘: 已按 %s=off 关闭%s", i, envDownloadDisk, downloadDiskNotes(res.Out))
		return
	}
	started := m.shell(ctx, i, "su 0 sh "+remote+" daemon", downloadDiskTimeout).Out
	status := m.shell(ctx, i, "su 0 sh "+remote+" status", 0).Out
	if warn, msg := downloadDiskReport(i, started, status); warn {
		m.Log.Warnf("%s", msg)
	} else {
		m.Log.Infof("%s", msg)
	}
}

// downloadDiskReport judges `modavd-dldisk.sh daemon` + `status`: running when a watcher pid is listed. The message
// names the listed games that are installed and what the script last said about each.
func downloadDiskReport(i int, started, status string) (warn bool, msg string) {
	lines := parseStatusLines(status)
	if lines["watchers"] == "" {
		return true, fmt.Sprintf("实例 %d 下载盘没起来: %s", i, tail(strings.TrimSpace(started)+" / "+status, 300))
	}
	var games []string
	for _, line := range outputLines(status) {
		pkg, state, ok := strings.Cut(line, ":")
		state = strings.TrimSpace(state)
		if !ok || pkg == "watchers" || state == "" || state == "not installed" {
			continue
		}
		games = append(games, pkg+"（"+downloadDiskState(state)+"）")
	}
	if len(games) == 0 {
		return false, fmt.Sprintf("实例 %d 下载盘: 守护已起（pid %s），名单里的游戏都没装", i, lines["watchers"])
	}
	return false, fmt.Sprintf("实例 %d 下载盘: 守护已起（pid %s）；%s", i, lines["watchers"], strings.Join(games, "，"))
}

// downloadDiskState puts the script's notes into the log's language; one it does not know stays as it is.
func downloadDiskState(state string) string {
	switch {
	case state == "on its disk":
		return "下载目录已在下载盘上"
	case state == "nothing done yet":
		return "游戏开着，等它退出后再挂"
	case strings.HasPrefix(state, "a download is under way"):
		return "有一轮下载是在普通目录里开始的，等它下完再挂"
	case strings.HasPrefix(state, "less than 2 GB free"):
		return "数据盘剩余不到 2 GB，没挂"
	}
	return state
}

// downloadDiskNotes is the script's own output as a suffix for a log line ("" when it said nothing).
func downloadDiskNotes(out string) string {
	var notes []string
	for _, line := range outputLines(out) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "modavd-dldisk: "); ok {
			notes = append(notes, rest)
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "（" + strings.Join(notes, "；") + "）"
}
