//go:build darwin && cgo

package macos

/*
#include <errno.h>
#include <libproc.h>
#include <signal.h>
#include <sys/proc.h>
#include <sys/sysctl.h>

static int bsd_info(int pid, struct proc_bsdinfo *info) {
    errno = 0;
    int n = proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, info, sizeof(*info));
    return n == sizeof(*info) ? 0 : (errno ? errno : EIO);
}

static int task_time(int pid, uint64_t *ns) {
    struct proc_taskinfo info;
    errno = 0;
    int n = proc_pidinfo(pid, PROC_PIDTASKINFO, 0, &info, sizeof(info));
    if (n != sizeof(info)) return errno ? errno : EIO;
    *ns = info.pti_total_user + info.pti_total_system;
    return 0;
}

static int argument_limit(int *limit) {
    size_t size = sizeof(*limit);
    int mib[] = { CTL_KERN, KERN_ARGMAX };
    return sysctl(mib, 2, limit, &size, NULL, 0) == 0 ? 0 : errno;
}

static int process_args(int pid, void *buf, size_t *size) {
    int mib[] = { CTL_KERN, KERN_PROCARGS2, pid };
    return sysctl(mib, 3, buf, size, NULL, 0) == 0 ? 0 : errno;
}
*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sort"
	"syscall"
	"time"
	"unsafe"
)

func bsdInfo(pid uint32) (C.struct_proc_bsdinfo, error) {
	var info C.struct_proc_bsdinfo
	if pid == 0 || pid > 1<<31-1 {
		return info, os.ErrProcessDone
	}
	if code := C.bsd_info(C.int(pid), &info); code != 0 {
		if code == C.ESRCH {
			return info, os.ErrProcessDone
		}
		return info, fmt.Errorf("读取进程 %d: %w", pid, syscall.Errno(code))
	}
	if info.pbi_pid != C.uint32_t(pid) || info.pbi_status == C.SZOMB {
		return info, os.ErrProcessDone
	}
	return info, nil
}

func processFromInfo(info C.struct_proc_bsdinfo) Process {
	nameString := func(buf []byte) string {
		if end := bytes.IndexByte(buf, 0); end >= 0 {
			buf = buf[:end]
		}
		return string(buf)
	}
	name := nameString(C.GoBytes(unsafe.Pointer(&info.pbi_name[0]), C.int(len(info.pbi_name))))
	if name == "" {
		name = nameString(C.GoBytes(unsafe.Pointer(&info.pbi_comm[0]), C.int(len(info.pbi_comm))))
	}
	return Process{
		PID: uint32(info.pbi_pid), PPID: uint32(info.pbi_ppid), Name: name,
		Created: uint64(info.pbi_start_tvsec)*1_000_000 + uint64(info.pbi_start_tvusec),
	}
}

// A child that has not been waited on still owns its PID even after exit.
// Capture its identity before starting the reaper, including short-lived zombies.
func startedProcess(pid uint32) (Process, bool) {
	var info C.struct_proc_bsdinfo
	if pid == 0 || pid > 1<<31-1 || C.bsd_info(C.int(pid), &info) != 0 || info.pbi_pid != C.uint32_t(pid) {
		return Process{}, false
	}
	p := processFromInfo(info)
	return p, p.Created != 0
}

// ListProcesses takes a libproc snapshot. Processes that exit or cannot be read
// during the snapshot are omitted; every returned process has a start identity.
func ListProcesses() ([]Process, error) {
	n, err := C.proc_listallpids(nil, 0)
	if n <= 0 {
		return nil, fmt.Errorf("读不到进程列表: %v", err)
	}
	for range 8 {
		ids := make([]C.int, int(n)+128)
		count, err := C.proc_listallpids(unsafe.Pointer(&ids[0]), C.int(len(ids)*4))
		if count <= 0 {
			return nil, fmt.Errorf("读不到进程列表: %v", err)
		}
		if int(count) >= len(ids) {
			n = count
			continue
		}
		out := make([]Process, 0, int(count))
		for _, pid := range ids[:int(count)] {
			if p, ok := FindProcess(uint32(pid)); ok {
				out = append(out, p)
			}
		}
		return out, nil
	}
	return nil, errors.New("进程列表持续增长，无法读取完整快照")
}

// FindProcess returns a live process with its creation time, never a zombie.
func FindProcess(pid uint32) (Process, bool) {
	info, err := bsdInfo(pid)
	if err != nil {
		return Process{}, false
	}
	p := processFromInfo(info)
	return p, p.Created != 0
}

func ProcessName(pid uint32) string {
	p, _ := FindProcess(pid)
	return p.Name
}

// checkedInfo refuses incomplete and stale identities. Query methods check both
// sides of a PID-based read, so a reused PID cannot supply another process's data.
func (p Process) checkedInfo() (C.struct_proc_bsdinfo, error) {
	if p.Created == 0 {
		return C.struct_proc_bsdinfo{}, errors.New("进程缺少创建时间，无法验证身份")
	}
	info, err := bsdInfo(p.PID)
	if err != nil {
		return info, err
	}
	if processFromInfo(info).Created != p.Created {
		return info, os.ErrProcessDone
	}
	return info, nil
}

func (p Process) StartedAt() time.Time {
	if p.Created == 0 {
		return time.Time{}
	}
	return time.UnixMicro(int64(p.Created))
}

func (p Process) Alive() bool {
	if p.Created == 0 {
		return false
	}
	_, err := p.checkedInfo()
	// A permission or transient query failure is not evidence of exit.
	return !errors.Is(err, os.ErrProcessDone)
}

func (p Process) CommandLine() ([]string, error) {
	args, _, err := p.processArguments()
	return args, err
}

// processArguments shares the native process record between argument and
// environment queries, including creation checks around the PID-based read.
func (p Process) processArguments() ([]string, []byte, error) {
	if _, err := p.checkedInfo(); err != nil {
		return nil, nil, err
	}
	var limit C.int
	if code := C.argument_limit(&limit); code != 0 {
		return nil, nil, fmt.Errorf("读不到 kern.argmax: %w", syscall.Errno(code))
	}
	if limit < 4 || limit > 64<<20 {
		return nil, nil, fmt.Errorf("无效的 kern.argmax: %d", limit)
	}
	buf := make([]byte, int(limit))
	size := C.size_t(len(buf))
	code := C.process_args(C.int(p.PID), unsafe.Pointer(&buf[0]), &size)
	if _, err := p.checkedInfo(); err != nil {
		return nil, nil, err
	}
	if code != 0 {
		return nil, nil, fmt.Errorf("读不到进程 %d 的命令行: %w", p.PID, syscall.Errno(code))
	}
	if size < 4 || uint64(size) > uint64(len(buf)) {
		return nil, nil, errors.New("进程命令行长度无效")
	}
	buf = buf[:int(size)]
	argc := int(binary.NativeEndian.Uint32(buf[:4]))
	buf = buf[4:]
	if argc < 1 || argc > len(buf) {
		return nil, nil, errors.New("进程参数数量无效")
	}
	// KERN_PROCARGS2: argc, executable path, NUL padding, then argc C strings.
	end := bytes.IndexByte(buf, 0)
	if end < 0 {
		return nil, nil, errors.New("进程命令行缺少程序路径")
	}
	buf = bytes.TrimLeft(buf[end:], "\x00")
	args := make([]string, 0, argc)
	for range argc {
		end = bytes.IndexByte(buf, 0)
		if end < 0 {
			return nil, nil, errors.New("进程命令行不完整")
		}
		args = append(args, string(buf[:end]))
		buf = buf[end+1:]
	}
	return args, buf, nil
}

func (p Process) ImagePath() (string, error) {
	if _, err := p.checkedInfo(); err != nil {
		return "", err
	}
	buf := make([]byte, C.PROC_PIDPATHINFO_MAXSIZE)
	n, queryErr := C.proc_pidpath(C.int(p.PID), unsafe.Pointer(&buf[0]), C.uint32_t(len(buf)))
	if _, err := p.checkedInfo(); err != nil {
		return "", err
	}
	if n <= 0 || int(n) > len(buf) {
		return "", fmt.Errorf("读不到进程 %d 的程序路径: %v", p.PID, queryErr)
	}
	return string(bytes.TrimRight(buf[:int(n)], "\x00")), nil
}

// Wait uses the reaper for our children and polls identity for unrelated processes.
func (p Process) Wait(timeout time.Duration) bool {
	if done, known := childWait(p, timeout); known {
		return done
	}
	deadline := time.Now().Add(timeout)
	for p.Alive() {
		left := time.Until(deadline)
		if left <= 0 {
			return false
		}
		time.Sleep(min(20*time.Millisecond, left))
	}
	return true
}

// ExitCode is known only for direct children started in this process. Darwin
// cannot recover the historical exit status of an unrelated or orphan process.
func (p Process) ExitCode() (uint32, bool) { return childExit(p) }

func (p Process) CPUTime() (time.Duration, bool) {
	if _, err := p.checkedInfo(); err != nil {
		return 0, false
	}
	var ns C.uint64_t
	code := C.task_time(C.int(p.PID), &ns)
	if _, err := p.checkedInfo(); err != nil || code != 0 || uint64(ns) > 1<<63-1 {
		return 0, false
	}
	return time.Duration(ns), true
}

// Kill checks the creation time immediately before SIGKILL. Darwin's signal API
// takes a PID, unlike a Windows process handle; it cannot pin an identity across
// the check and signal. Missing identities and PID 0/1 are never signalled.
func (p Process) Kill() error {
	if p.PID <= 1 {
		return errors.New("拒绝结束 PID 0 或 1")
	}
	if _, err := p.checkedInfo(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	if err := syscall.Kill(int(p.PID), syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("结束进程 %d: %w", p.PID, err)
	}
	return nil
}

// KillTree uses one snapshot, verifies creation order and kills deepest first.
// Each signal checks identity again; an already gone root owns no new tree.
func KillTree(root Process) error {
	if root.PID <= 1 {
		return errors.New("拒绝结束 PID 0 或 1 的进程树")
	}
	if _, err := root.checkedInfo(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	all, err := ListProcesses()
	if err != nil {
		return err
	}
	children := make(map[uint32][]Process)
	for _, p := range all {
		children[p.PPID] = append(children[p.PPID], p)
	}
	type node struct {
		p         Process
		ancestors []Process
	}
	queue := []node{{p: root}}
	seen := map[uint32]bool{root.PID: true}
	var found []node
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		for _, p := range children[n.p.PID] {
			if seen[p.PID] || p.Created == 0 || p.Created < n.p.Created {
				continue
			}
			seen[p.PID] = true
			ancestors := append(append([]Process(nil), n.ancestors...), n.p)
			child := node{p, ancestors}
			found = append(found, child)
			queue = append(queue, child)
		}
	}
	if _, err := root.checkedInfo(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	sort.SliceStable(found, func(i, j int) bool { return len(found[i].ancestors) > len(found[j].ancestors) })
	var first error
	for _, n := range found {
		// Snapshot rows are read at different times. An intermediate parent's
		// PID may already belong to an unrelated process; verify the entire
		// ancestry before acting on an otherwise valid descendant identity.
		chain := append(n.ancestors, n.p)
		member := true
		for i, p := range chain {
			info, err := p.checkedInfo()
			if err != nil {
				if !errors.Is(err, os.ErrProcessDone) && first == nil {
					first = err
				}
				member = false
				break
			}
			if i > 0 && uint32(info.pbi_ppid) != chain[i-1].PID {
				member = false
				break
			}
		}
		if !member {
			continue
		}
		if err := n.p.Kill(); err != nil && first == nil {
			first = err
		}
	}
	if err := root.Kill(); err != nil {
		return err
	}
	return first
}
