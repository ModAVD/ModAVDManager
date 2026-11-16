package emu

import "testing"

func TestVulkanModeReport(t *testing.T) {
	status := "watchers: 5481 \r\ncom.miHoYo.hkrpg: set to Vulkan\r\ncom.miHoYo.hkrpg.bilibili: not installed\r\n"
	warn, msg := vulkanModeReport(3, "modavd-vkmode: com.miHoYo.hkrpg: set to Vulkan\n", status)
	if warn || msg != "实例 3 游戏显示模式预设: 守护已起（pid 5481）；com.miHoYo.hkrpg（已预设为 Vulkan）" {
		t.Errorf("set: %v %q", warn, msg)
	}
	warn, msg = vulkanModeReport(3, "", "watchers: 5481\ncom.miHoYo.hkrpg: not installed\ncom.miHoYo.hkrpg.bilibili: not installed\n")
	if warn || msg != "实例 3 游戏显示模式预设: 守护已起（pid 5481），名单里的游戏都没装" {
		t.Errorf("none installed: %v %q", warn, msg)
	}
	for state, want := range map[string]struct {
		text string
		warn bool
	}{
		"display mode chosen already":                                   {"已选过显示模式，不动", false},
		"running, set once it exits":                                    {"游戏开着，等它退出后再写", false},
		"nothing done yet":                                              {"还没处理", false},
		"cannot write the display mode files":                           {"显示模式文件写不进去", true},
		"cannot make /data/media/0/Android/data/com.miHoYo.hkrpg/files": {"建不了 /data/media/0/Android/data/com.miHoYo.hkrpg/files", true},
		"something this build does not know":                            {"something this build does not know", false},
	} {
		warn, msg = vulkanModeReport(1, "", "watchers: 7\ncom.miHoYo.hkrpg: "+state+"\n")
		if warn != want.warn || msg != "实例 1 游戏显示模式预设: 守护已起（pid 7）；com.miHoYo.hkrpg（"+want.text+"）" {
			t.Errorf("%s: %v %q", state, warn, msg)
		}
	}
	// both listed games installed
	warn, msg = vulkanModeReport(2, "", "watchers: 9\ncom.miHoYo.hkrpg: display mode chosen already\ncom.miHoYo.hkrpg.bilibili: set to Vulkan\n")
	if warn || msg != "实例 2 游戏显示模式预设: 守护已起（pid 9）；com.miHoYo.hkrpg（已选过显示模式，不动），com.miHoYo.hkrpg.bilibili（已预设为 Vulkan）" {
		t.Errorf("both: %v %q", warn, msg)
	}
	// no watcher: a warning with what the script said
	warn, msg = vulkanModeReport(2, "modavd-vkmode: oops\n", "watchers: \ncom.miHoYo.hkrpg: not installed\n")
	if !warn || msg != "实例 2 游戏显示模式预设没起来: modavd-vkmode: oops / watchers: \ncom.miHoYo.hkrpg: not installed" {
		t.Errorf("no watcher: %v %q", warn, msg)
	}
	if warn, _ = vulkanModeReport(2, "", ""); !warn {
		t.Error("empty status: no warning")
	}
	// what the two files hold, as status prints it
	warn, msg = vulkanModeReport(4, "", "watchers: 11 \r\ncom.miHoYo.hkrpg: set to Vulkan [custom=01 caps=01]\r\n"+
		"com.miHoYo.hkrpg.bilibili: display mode chosen already [custom=00 caps=-]\r\n")
	if warn || msg != "实例 4 游戏显示模式预设: 守护已起（pid 11）；com.miHoYo.hkrpg（已预设为 Vulkan，custom=01 caps=01），"+
		"com.miHoYo.hkrpg.bilibili（已选过显示模式，不动，custom=00 caps=-）" {
		t.Errorf("files: %v %q", warn, msg)
	}
	warn, msg = vulkanModeReport(4, "", "watchers: 11\ncom.miHoYo.hkrpg: cannot make /data/media/0/Android/data/com.miHoYo.hkrpg/files [custom=- caps=-]\n")
	if !warn || msg != "实例 4 游戏显示模式预设: 守护已起（pid 11）；com.miHoYo.hkrpg（建不了 /data/media/0/Android/data/com.miHoYo.hkrpg/files，custom=- caps=-）" {
		t.Errorf("cannot, with files: %v %q", warn, msg)
	}
	warn, msg = vulkanModeReport(4, "", "watchers: 11\ncom.miHoYo.hkrpg: nothing done yet [custom=empty caps=03]\n")
	if warn || msg != "实例 4 游戏显示模式预设: 守护已起（pid 11）；com.miHoYo.hkrpg（还没处理，custom=empty caps=03）" {
		t.Errorf("empty file: %v %q", warn, msg)
	}
}
