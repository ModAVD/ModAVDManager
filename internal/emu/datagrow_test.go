//go:build windows

package emu

import "testing"

func TestParseDataSize(t *testing.T) {
	// what dataSizeScript printed on a 64 GB instance raised to 96 GB, before resize2fs
	out := "dev: /dev/block/dm-40\r\ndevice: 103079215104\r\nBlock count:              16777216\r\nBlock size:               4096\r\n"
	got, ok := parseDataSize(out)
	if !ok || got != (dataSize{Dev: "/dev/block/dm-40", Device: 103079215104, FSSize: 68719476736}) {
		t.Fatalf("got %+v %v", got, ok)
	}
	if got.Device-got.FSSize < dataGrowMin {
		t.Fatal("32 GB of slack not seen as growable")
	}
	same, _ := parseDataSize("dev: /dev/block/dm-40\ndevice: 68719476736\nBlock count: 16777216\nBlock size: 4096\n")
	if same.Device-same.FSSize >= dataGrowMin {
		t.Fatal("no slack seen as growable")
	}
	for _, bad := range []string{
		"",
		"dev: \ndevice: \n",                  // /data not found
		"dev: /dev/block/dm-40\ndevice: 1\n", // tune2fs failed
		"dev: /dev/block/dm-40; reboot\ndevice: 103079215104\nBlock count: 1\nBlock size: 4096\n",
		"dev: /dev/block/dm-40\ndevice: x\nBlock count: 1\nBlock size: 4096\n",
	} {
		if _, ok := parseDataSize(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestDataOldLayout(t *testing.T) {
	line := "[   35.992040] EXT4-fs warning (device dm-40): reserve_backup_gdb:1052: reserved block 1024 not at offset 1023"
	if !dataOldLayout(line) {
		t.Fatal("kernel line not recognised")
	}
	if dataOldLayout("") || dataOldLayout("EXT4-fs (dm-40): resized filesystem to 25165824") {
		t.Fatal("false positive")
	}
}
