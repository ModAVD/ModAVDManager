//go:build windows

package emu

// Growing the data partition's file system after `set --data-gb` raised it. The emulator (sdk-mas36 on) grows the
// overlay userdata-qemu.img.qcow2 at launch when disk.dataPartition.size is larger than it; vold then sets up the
// metadata encryption mapping over the whole, larger disk, and the ext4 inside is grown here, online (resize2fs on the
// mounted /data, 0.3 s for 64 -> 96 GB on 10-29).
//
// Data partitions of 16 GB or less created before sdk-mas36 carry a resize inode the kernel's online resize cannot
// fully use (the emulator's ext4_utils put the last reserved GDT block in the wrong slot): growing them stops at the
// first new block group holding a superblock backup (16 GB -> 30.4 GB instead of 32 GB). That is reported, not
// repaired: rewriting the metadata of a mounted /data (errors=panic) is not worth the risk.

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// dataSizeScript prints /data's block device, the device's size in bytes and the ext4 block count and size (su, so
// no single quotes; mksh arithmetic is 32-bit, so the numbers are compared on the host).
const dataSizeScript = `d=$(grep " /data " /proc/mounts | head -n 1 | cut -d" " -f1); ` +
	`echo "dev: $d"; echo "device: $(blockdev --getsize64 $d)"; tune2fs -l $d | grep -E "^Block (count|size):"`

const (
	// dataGrowMin: less slack than one block group (32768 blocks of 4 KiB) is not worth a resize2fs; a partition
	// whose size is not a whole number of groups always keeps a little.
	dataGrowMin = 128 << 20
	// dataGrowTimeout: online resize writes the new groups' bitmaps and tables; 64 -> 512 GB is about 3600 groups.
	dataGrowTimeout = 3 * time.Minute
)

var dataDevRe = regexp.MustCompile(`^/dev/block/[A-Za-z0-9._-]+$`)

type dataSize struct {
	Dev            string
	Device, FSSize int64 // bytes
}

// parseDataSize reads dataSizeScript's output; ok only when all four values are there and the device looks like a
// block device path (it goes into the resize2fs command line).
func parseDataSize(out string) (dataSize, bool) {
	l := parseStatusLines(out)
	device, err1 := strconv.ParseInt(l["device"], 10, 64)
	count, err2 := strconv.ParseInt(l["Block count"], 10, 64)
	bs, err3 := strconv.ParseInt(l["Block size"], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || device <= 0 || count <= 0 || bs <= 0 || !dataDevRe.MatchString(l["dev"]) {
		return dataSize{}, false
	}
	return dataSize{Dev: l["dev"], Device: device, FSSize: count * bs}, true
}

func gib(b int64) string { return fmt.Sprintf("%.1f GB", float64(b)/(1<<30)) }

// growData grows /data's file system to its device when the device is at least dataGrowMin larger (after the data
// partition was raised). Only warnings; nothing to do on almost every boot.
func (m *Manager) growData(ctx context.Context, i int) {
	res := m.su(ctx, i, dataSizeScript, 0)
	before, ok := parseDataSize(res.Out)
	if !ok {
		m.Log.Warnf("实例 %d 读不到数据盘大小，没检查要不要扩: %s", i, orText(tail(res.Out, 200), fmt.Sprintf("退出码 %d", res.Code)))
		return
	}
	if before.Device-before.FSSize < dataGrowMin {
		return
	}
	m.Log.Infof("实例 %d 数据盘: 磁盘 %s，文件系统 %s，在线扩大", i, gib(before.Device), gib(before.FSSize))
	started := time.Now()
	grow := m.su(ctx, i, "resize2fs "+before.Dev+" 2>&1", dataGrowTimeout)
	took := time.Since(started).Seconds()
	after, ok := parseDataSize(m.su(ctx, i, dataSizeScript, 0).Out)
	if !ok {
		m.Log.Warnf("实例 %d 扩完读不到数据盘大小（resize2fs 退出码 %d）: %s", i, grow.Code, tail(grow.Out, 300))
		return
	}
	if after.Device-after.FSSize < dataGrowMin {
		m.Log.Infof("实例 %d 数据盘已扩到 %s（%.1f 秒）", i, gib(after.FSSize), took)
		return
	}
	// The old resize inode layout (see the header): the kernel names the misplaced block. It stays that way, so every
	// later boot tries again, gets nowhere and says the same.
	if dataOldLayout(m.su(ctx, i, "dmesg | grep -m 1 reserve_backup_gdb", 0).Out) {
		m.Log.Warnf("实例 %d 数据盘只能扩到 %s（磁盘 %s）：这台实例的数据盘是 0.36 之前按 16 GB 及以下建的，"+
			"当时的建盘工具留下的结构让在线扩容过不去，可用空间就到这里。要更大只能新建一台实例再搬游戏", i,
			gib(after.FSSize), gib(after.Device))
		return
	}
	m.Log.Warnf("实例 %d 数据盘没扩满（磁盘 %s，文件系统 %s，原来 %s；resize2fs 退出码 %d）: %s", i, gib(after.Device),
		gib(after.FSSize), gib(before.FSSize), grow.Code, tail(grow.Out, 300))
}

// dataOldLayout: the kernel log line of an online resize that hit the misplaced reserved GDT block
// ("reserve_backup_gdb:1052: reserved block 1024 not at offset 1023").
func dataOldLayout(dmesg string) bool {
	return strings.Contains(dmesg, "reserve_backup_gdb") && strings.Contains(dmesg, "not at offset")
}
