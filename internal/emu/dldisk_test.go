package emu

import "testing"

func TestDownloadDiskReport(t *testing.T) {
	// the watcher runs, one listed game is installed and on its disk
	status := "watchers: 5481 \r\ncom.hypergryph.arknights: on its disk\r\ncom.hypergryph.arknights.bilibili: not installed\r\n"
	warn, msg := downloadDiskReport(3, "modavd-dldisk: com.hypergryph.arknights: disk mounted (kept image)\n", status)
	if warn || msg != "实例 3 下载盘: 守护已起（pid 5481）；com.hypergryph.arknights（下载目录已在下载盘上）" {
		t.Errorf("on its disk: %v %q", warn, msg)
	}
	// none of them installed
	warn, msg = downloadDiskReport(3, "", "watchers: 5481\ncom.hypergryph.arknights: not installed\ncom.hypergryph.arknights.bilibili: not installed\n")
	if warn || msg != "实例 3 下载盘: 守护已起（pid 5481），名单里的游戏都没装" {
		t.Errorf("none installed: %v %q", warn, msg)
	}
	// every state the script reports, and one this build does not know
	for state, want := range map[string]string{
		"nothing done yet": "游戏开着，等它退出后再挂",
		"a download is under way in the plain folder, the disk comes when it is done": "有一轮下载是在普通目录里开始的，等它下完再挂",
		"less than 2 GB free on /data, no disk":                                       "数据盘剩余不到 2 GB，没挂",
		"cannot make the image":                                                       "cannot make the image",
	} {
		warn, msg = downloadDiskReport(1, "", "watchers: 7\ncom.hypergryph.arknights: "+state+"\n")
		if warn || msg != "实例 1 下载盘: 守护已起（pid 7）；com.hypergryph.arknights（"+want+"）" {
			t.Errorf("%s: %v %q", state, warn, msg)
		}
	}
	// no watcher: a warning with what the script said
	started := "modavd-dldisk: something went wrong"
	warn, msg = downloadDiskReport(2, started+"\n", "watchers: \ncom.hypergryph.arknights: not installed\n")
	if !warn || msg != "实例 2 下载盘没起来: "+started+" / watchers: \ncom.hypergryph.arknights: not installed" {
		t.Errorf("no watcher: %v %q", warn, msg)
	}
	if warn, _ = downloadDiskReport(2, "", ""); !warn {
		t.Error("empty status: no warning")
	}
}

func TestDownloadDiskNotes(t *testing.T) {
	out := "modavd-dldisk: com.hypergryph.arknights: disk removed\r\nsomething else\nmodavd-dldisk: com.x: running, its disk stays until the next boot\n"
	if got := downloadDiskNotes(out); got != "（com.hypergryph.arknights: disk removed；com.x: running, its disk stays until the next boot）" {
		t.Errorf("notes %q", got)
	}
	if got := downloadDiskNotes("\n"); got != "" {
		t.Errorf("no notes: %q", got)
	}
}
