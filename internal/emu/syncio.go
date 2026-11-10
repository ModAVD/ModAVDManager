package emu

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modavd/internal/avd"
	"modavd/internal/root"
)

// Synchronous disk I/O under WHPX (ModAVD/ModAVD#35). A guest fsync on ext4 is three write requests in a row (data,
// journal, commit), each one a round trip to QEMU that the guest waits for. Two QEMU arguments make that round trip
// cheaper without changing what "written" means (a request still completes only once the data is in the host file):
//
//   - -global virtio-blk-pci.ioeventfd=off: WHPX has no in-kernel eventfd, QEMU emulates it in user mode, which costs
//     a request three main-loop passes and three thread hand-offs. Off, the vCPU thread handles the request itself.
//   - -acpitable <SSDT with a _CST per CPU>: the guest has no cpuidle driver and halts whenever it has nothing to run,
//     and Hyper-V needs 20-24 us to wake a halted vCPU for every completion. The table declares one C1 entered by
//     HLT with an exit latency of cstLatencyUs. The guest's own acpi_idle driver and menu governor then busy-wait
//     for up to 2 x cstLatencyUs when a task on that CPU waits for I/O (or the CPU is about to be needed again
//     anyway) and halt otherwise.
//
// Measured 11-10 on a 6-core test instance (two writers, 7 KB + fsync each, the Arknights download pattern):
// 15-16 MB/s without, 19 with the first, 19-22 with the second, 33-36 with both; single-threaded 4 KiB + fsync
// 584-638 us -> 319 us. An idle guest spent 0.1 s of 240 CPU-seconds busy-waiting. The Arknights resource download
// itself, three alternating 150 s windows each: 6.9-7.8 -> 10.4-11.8 MiB/s. With the table the busy loops of
// mas-idlepoll.sh are not started any more (setupIdlepoll).
//
// Windows only: the other hosts use another accelerator and were not measured.
const (
	// envSyncIO set to "off" starts the emulator without the two arguments (A/B runs, and the way back).
	envSyncIO = "MODAVD_SYNC_IO"
	// cstFileName is the table in the instance's own folder, rewritten when the CPU count changes.
	cstFileName  = "modavd-cst.aml"
	cstLatencyUs = 100
	// QEMU names the processor objects C000..C0FE (hw/acpi/cpu.c); one more would need another name format.
	cstMaxCPUs = 255
)

// syncIOQemuArgs is what goes after -qemu for the two changes above. The table is optional: when it cannot be
// written or named on the command line, the instance starts without it.
func (m *Manager) syncIOQemuArgs(inst *avd.Instance) []string {
	if !root.CurrentPlatform().Windows() || strings.EqualFold(strings.TrimSpace(os.Getenv(envSyncIO)), "off") {
		return nil
	}
	args := []string{"-global", "virtio-blk-pci.ioeventfd=off"}
	cpus, ok := inst.CPU()
	if !ok || cpus < 1 || cpus > cstMaxCPUs {
		m.Log.Warnf("实例 %d 读不出 CPU 核数（hw.cpu.ncore），这次开机不带空闲状态表", inst.Index)
		return args
	}
	rel, err := writeCstTable(filepath.Dir(m.Root.EmulatorExe()), inst.Dir, cpus)
	if err != nil {
		m.Log.Warnf("实例 %d 这次开机不带空闲状态表: %v", inst.Index, err)
		return args
	}
	return append(args, "-acpitable", "file="+rel)
}

// writeCstTable puts the table for cpus processors into instDir and returns its path relative to emuDir, the
// emulator's working directory. Relative because QEMU splits file= at ':' and ',' and opens the name with the ANSI
// code page: <root>\sdk\emulator -> ..\..\avd\mas_<i>.avd\modavd-cst.aml is plain ASCII wherever the root is.
func writeCstTable(emuDir, instDir string, cpus int) (string, error) {
	path := filepath.Join(instDir, cstFileName)
	rel, err := filepath.Rel(emuDir, path)
	if err != nil {
		return "", fmt.Errorf("%s 和模拟器目录 %s 不在同一个盘上", path, emuDir)
	}
	for _, c := range rel {
		if c >= 0x80 || c < 0x20 || c == ':' || c == ',' || c == '"' {
			return "", fmt.Errorf("表的相对路径 %s 里有 QEMU 命令行放不下的字符", rel)
		}
	}
	table := cstTable(cpus, cstLatencyUs)
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, table) {
		return rel, nil
	}
	if err := os.WriteFile(path, table, 0o644); err != nil {
		return "", err
	}
	return rel, nil
}

// cstTable is an ACPI SSDT that adds to each of QEMU's processor objects \_SB.CPUS.C000 .. (one per CPU)
//
//	Name (_CST, Package () { 1, Package () { ResourceTemplate () { Register (FFixedHW, 0, 0, 0) }, 1, latency, 1000 } })
//
// i.e. one C-state of type C1. Bit offset 0 is not the MWAIT encoding, so Linux enters it with HLT ("ACPI HLT").
// A table naming a processor QEMU did not create fails to load in the guest and changes nothing.
func cstTable(cpus, latencyUs int) []byte {
	// Generic Register Descriptor (0x82, 12 bytes: FFixedHW, width 0, offset 0, access 0, address 0) + End Tag
	res := append([]byte{0x82, 0x0c, 0x00, 0x7f, 0x00, 0x00, 0x00}, make([]byte, 8)...)
	res = append(res, 0x79, 0x00)
	buffer := amlPackage(0x11, append(amlInteger(len(res)), res...))
	state := []byte{0x04}
	state = append(state, buffer...)
	state = append(state, amlInteger(1)...)         // type C1
	state = append(state, amlInteger(latencyUs)...) // worst-case exit latency, microseconds
	state = append(state, amlInteger(1000)...)      // power, milliwatts (unused by Linux)
	cst := append([]byte{0x02}, amlInteger(1)...)   // two elements: the count, then the one state
	cst = append(cst, amlPackage(0x12, state)...)
	term := append([]byte{0x08, '_', 'C', 'S', 'T'}, amlPackage(0x12, cst)...)

	var body []byte
	for i := 0; i < cpus; i++ {
		name := fmt.Sprintf("\\/\x03_SB_CPUSC%03X", i) // root, MultiNamePrefix, 3 segments
		body = append(body, amlPackage(0x10, append([]byte(name), term...))...)
	}
	table := make([]byte, 36, 36+len(body))
	copy(table[0:], "SSDT")
	binary.LittleEndian.PutUint32(table[4:], uint32(36+len(body)))
	table[8] = 1 // revision
	copy(table[10:], "MODAVD")
	copy(table[16:], "CPUCST  ")
	binary.LittleEndian.PutUint32(table[24:], 1)
	copy(table[28:], "MAVD")
	binary.LittleEndian.PutUint32(table[32:], 1)
	table = append(table, body...)
	var sum byte
	for _, b := range table {
		sum += b
	}
	table[9] = -sum
	return table
}

// amlPackage is op, PkgLength, content. PkgLength counts its own bytes: one byte up to 63, else the low nibble in the
// lead byte (with the number of following bytes in its top two bits) and the rest in up to three more.
func amlPackage(op byte, content []byte) []byte {
	out := []byte{op}
	for n := 1; n <= 4; n++ {
		total := len(content) + n
		if n == 1 {
			if total <= 0x3f {
				out = append(out, byte(total))
				break
			}
			continue
		}
		if total < 1<<(4+8*(n-1)) {
			out = append(out, byte(n-1)<<6|byte(total&0x0f))
			for k, rest := 1, total>>4; k < n; k, rest = k+1, rest>>8 {
				out = append(out, byte(rest))
			}
			break
		}
	}
	return append(out, content...)
}

// amlInteger is the shortest AML integer constant for v (0 <= v < 2^32).
func amlInteger(v int) []byte {
	switch {
	case v == 0:
		return []byte{0x00}
	case v == 1:
		return []byte{0x01}
	case v <= 0xff:
		return []byte{0x0a, byte(v)}
	case v <= 0xffff:
		return []byte{0x0b, byte(v), byte(v >> 8)}
	default:
		return []byte{0x0c, byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)}
	}
}
