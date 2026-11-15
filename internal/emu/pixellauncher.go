package emu

import (
	"context"
	"fmt"
	"strings"
	"time"

	"modavd/internal/adb"
)

// pixelLauncherRemoveScript removes the stock launcher for user 0 (run as root) and waits until the package manager
// has written that to disk: it only schedules the write of package-restrictions.xml (10 s later), so a framework
// restart or a power cut before then brings the launcher back at the next boot. 11-15: the first-boot setup removed
// it 2.6 s before the zygote restart of the guest library fixes, and every instance created since kept the stock
// launcher, which draws its taskbar over every home app once the display counts as a tablet (1080p, 280 dpi).
// Prints SAVED once the file's modification time changed, UNSAVED after 20 s without; exits 1 when pm fails. The time
// is taken after pm returns: a write that starts later has the removal in it, one that ran during pm may not have.
const pixelLauncherRemoveScript = `f=/data/system/users/0/package-restrictions.xml; ` +
	`pm uninstall --user 0 ` + pixelLauncherPackage + ` || exit 1; b=$(stat -c %y $f 2>/dev/null); n=0; ` +
	`while [ $n -lt 40 ]; do sleep 0.5; [ "$(stat -c %y $f 2>/dev/null)" != "$b" ] && { echo SAVED; exit 0; }; ` +
	`n=$((n+1)); done; echo UNSAVED`

// pixelLauncherRemoveTimeout covers the script's 20 s wait and pm itself.
const pixelLauncherRemoveTimeout = 45 * time.Second

// pixelLauncherListed: `pm list packages --user 0 <pkg>` lists the stock launcher itself (the filter also matches
// longer names).
func pixelLauncherListed(out string) bool {
	for _, line := range outputLines(out) {
		if strings.TrimSpace(line) == "package:"+pixelLauncherPackage {
			return true
		}
	}
	return false
}

// removePixelLauncher runs pixelLauncherRemoveScript. ok: pm removed it; saved: the removal is on disk. out is the
// script's output for the messages.
func (m *Manager) removePixelLauncher(ctx context.Context, i int) (ok, saved bool, out string) {
	res := m.su(ctx, i, pixelLauncherRemoveScript, pixelLauncherRemoveTimeout)
	ok, saved = pixelLauncherRemoveResult(res)
	return ok, saved, res.Out
}

// pixelLauncherRemoveResult reads pixelLauncherRemoveScript's result: ok when it exited 0 with SAVED or UNSAVED last.
func pixelLauncherRemoveResult(res adb.Result) (ok, saved bool) {
	last := lastNonEmptyLine(res.Out)
	ok = res.Code == 0 && (last == "SAVED" || last == "UNSAVED")
	return ok, ok && last == "SAVED"
}

// pixelLauncherRemoveReport is the log line of removePixelLauncher's result (what: what was done, for the message).
func pixelLauncherRemoveReport(i int, what string, ok, saved bool, out string) report {
	switch {
	case !ok:
		return report{true, fmt.Sprintf("实例 %d %s失败，下次开机再试: %s", i, what, orText(tail(strings.TrimSpace(out), 200), "无输出"))}
	case !saved:
		return report{true, fmt.Sprintf("实例 %d %s：已卸载，但 20 秒内没见系统写盘，下次开机会再检查", i, what)}
	}
	return report{false, fmt.Sprintf("实例 %d %s：已卸载并写盘", i, what)}
}

// ensurePixelLauncherRemoved removes the stock launcher again when an instance set up with the light launcher still
// has it for user 0 (see pixelLauncherRemoveScript for how that happened). Instances whose first-boot setup kept the
// stock launcher (meta launcher "pixel") are left alone. Every boot after the first; failures are only logged.
func (m *Manager) ensurePixelLauncherRemoved(ctx context.Context, i int) {
	meta := m.Root.InstanceMeta(i)
	if meta == nil {
		return
	}
	if launcher, _ := meta.Get("launcher").Str(); launcher != lightLauncherPackage {
		return
	}
	res := m.shell(ctx, i, "pm list packages --user 0 "+pixelLauncherPackage, 0)
	if res.Code != 0 {
		m.Log.Debugf("实例 %d 读不出原生桌面在不在: %s", i, tail(res.Out, 200))
		return
	}
	if !pixelLauncherListed(res.Out) {
		return
	}
	ok, saved, out := m.removePixelLauncher(ctx, i)
	m.logReport(pixelLauncherRemoveReport(i, "原生 Pixel 桌面又出现了（之前的卸载没写进磁盘），重新卸载", ok, saved, out))
}
