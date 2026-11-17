package emu

import (
	"os"
	"path/filepath"
)

// screenshotFolder is the folder under the user's Pictures that the emulator's screenshot button saves to
// (ModAVD/ModAVD#231).
const screenshotFolder = "ModAVD"

// picturesDir is the current user's Pictures folder ("" and the reason when there is none): hostPicturesDir, a
// variable so tests point it at a temporary directory instead of the real one.
var picturesDir = hostPicturesDir

// screenshotSavePath is the -save-path of a boot: <Pictures>\ModAVD, created first. "" when the Pictures folder is
// unknown or the folder cannot be made; the emulator then saves where it always did. Never stops a start. -save-path
// is the emulator's own option (stock emulators have it too), and it is not remembered past that run.
func (m *Manager) screenshotSavePath() string {
	base, why := picturesDir()
	if base == "" {
		m.Log.Warnf("找不到当前用户的「图片」文件夹（%s），截图照模拟器默认的位置保存", why)
		return ""
	}
	dir := filepath.Join(base, screenshotFolder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.Log.Warnf("建截图文件夹 %s 失败（%v），截图照模拟器默认的位置保存", dir, err)
		return ""
	}
	return dir
}
