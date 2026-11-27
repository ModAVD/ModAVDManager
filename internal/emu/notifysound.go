package emu

// The guest's notification sound off by default (ModAVD/ModAVD#280): with sound on, the system plays the default
// notification tone for every notification, e.g. the Safety Center's "no screen lock" one about 18 s after a boot, and
// nobody is at an unattended instance to want it.
//
// What the default tone is: the channels' sound is the URI content://settings/system/notification_sound, which the
// player resolves through the system setting of that name. The setting unset (`settings delete`, `settings get` then
// prints "null") resolves to nothing, so no track is played at all. `settings put system notification_sound ''` does
// not do it (the empty value is ignored and the old one stays). This needs no sound card and leaves the media volume,
// the touch sounds and the ringtone alone; the notifications themselves still show.
//
// Once per instance, recorded in mas-avd.json: an instance set up before this existed gets it at its next boot too,
// and a tone the user chose afterwards is not overwritten at every boot. Not during the first boot of a new instance
// (before the first-boot setup has recorded initialized): the system sets its default tones when its first media scan
// finishes, which can be after this step and then puts the default back (measured 11-27: a create that muted at its
// first boot read the default again at the next start). The next boot is the first one nobody can have heard the tone
// at: the Safety Center posts its notification only 12 hours after it first saw the issue.

import (
	"context"
	"fmt"
	"strings"

	"modavd/internal/adb"
	"modavd/internal/jsonord"
)

// notificationSoundMutedKey is instances.<i>.notificationSoundMuted in mas-avd.json: true once the notification sound
// was turned off.
const notificationSoundMutedKey = "notificationSoundMuted"

// muteNotificationSoundCommand unsets the setting and reads it back (an unset one reads "null").
const muteNotificationSoundCommand = "settings delete system notification_sound ; settings get system notification_sound"

// notificationSoundUnset judges the output of muteNotificationSoundCommand: the read-back (its last line) is "null".
func notificationSoundUnset(out string) bool {
	return lastNonEmptyLine(out) == "null"
}

// notificationSoundDone: instances.<i>.notificationSoundMuted is true.
func (m *Manager) notificationSoundDone(i int) bool {
	meta := m.Root.InstanceMeta(i)
	return meta != nil && jsonTruthy(meta.Get(notificationSoundMutedKey))
}

// setupNotificationSound is the first step of afterBoot: it runs before the guest can post anything the user would
// hear. Only warnings: a failure leaves the marker unset and the next boot tries again.
func (m *Manager) setupNotificationSound(ctx context.Context, i int) {
	m.muteNotificationSound(ctx, i, func(command string) adb.Result { return m.shell(ctx, i, command, 0) })
}

// muteNotificationSound is setupNotificationSound over run, the guest shell.
func (m *Manager) muteNotificationSound(ctx context.Context, i int, run func(command string) adb.Result) {
	if !m.initialized(i) || m.notificationSoundDone(i) {
		return
	}
	res := run(muteNotificationSoundCommand)
	if ctx.Err() != nil {
		return
	}
	if res.Code != 0 || !notificationSoundUnset(res.Out) {
		m.Log.Warnf("实例 %d 没关掉通知提示音（下次开机重试）: %s", i,
			orText(tail(strings.Join(strings.Fields(res.Out), " "), 200), fmt.Sprintf("退出码 %d", res.Code)))
		return
	}
	err := m.Root.EditInstanceMeta(i, func(meta *jsonord.Obj) error {
		meta.Set(notificationSoundMutedKey, jsonord.NewBool(true))
		return nil
	})
	if err != nil {
		m.Log.Warnf("实例 %d 通知提示音已关，但结果没写进 mas-avd.json，下次开机再关一次: %v", i, err)
		return
	}
	m.Log.Infof("实例 %d 已关掉通知提示音（通知照常显示）", i)
}
