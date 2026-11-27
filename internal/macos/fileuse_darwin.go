//go:build darwin && arm64 && cgo

package macos

/*
#include <errno.h>
#include <libproc.h>
#include <stdlib.h>
#include <unistd.h>
static int daily_file_open(int pid, uint32_t dev, uint64_t ino, int *found) {
    *found = 0;
    for (int attempt = 0; attempt < 4; attempt++) {
        int size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
        if (size < 0) return errno ? errno : EIO;
        if (size == 0) return 0;
        if (size > 8 * 1024 * 1024) return EOVERFLOW;
        size += 64 * sizeof(struct proc_fdinfo);
        struct proc_fdinfo *fds = malloc(size);
        if (!fds) return ENOMEM;
        int used = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, size);
        if (used <= 0) { int e = errno; free(fds); return e ? e : EIO; }
        if (used >= size) { free(fds); continue; }
        int count = used / sizeof(struct proc_fdinfo);
        for (int i = 0; i < count; i++) {
            if (fds[i].proc_fdtype != PROX_FDTYPE_VNODE) continue;
            struct vnode_fdinfo info;
            errno = 0;
            int n = proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDVNODEINFO, &info, sizeof(info));
            if (n != sizeof(info)) {
                int e = errno;
                if (e == EBADF || e == ENOENT || e == EINVAL) continue;
                free(fds); return e ? e : EIO;
            }
            if (info.pvi.vi_stat.vst_dev == dev && info.pvi.vi_stat.vst_ino == ino) {
                *found = 1; free(fds); return 0;
            }
        }
        free(fds); return 0;
    }
    return EAGAIN;
}
*/
import "C"

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// FileInUse protects actual open vnodes, including files renamed by another
// process. Every process read is bracketed by its PID/start identity. Unknown
// descriptors of a possible managed writer conservatively prevent deletion.
// Protected macOS services outside the package can deny descriptor inspection;
// their denial does not turn every closed manager log into an active one.
func FileInUse(path string) (busy, vanished bool) {
	return fileInUse(path, path)
}

func fileInUse(path, writerPath string) (busy, vanished bool) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, true
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return true, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true, false
	}
	processes, err := ListProcesses()
	if err != nil {
		return true, false
	}
	for _, p := range processes {
		before, err := p.checkedInfo()
		if errors.Is(err, os.ErrProcessDone) {
			continue
		}
		if err != nil {
			return true, false
		}
		if uint32(before.pbi_uid) != uint32(os.Geteuid()) {
			continue
		}
		var found C.int
		code := C.daily_file_open(C.int(p.PID), C.uint32_t(stat.Dev), C.uint64_t(stat.Ino), &found)
		if _, err := p.checkedInfo(); errors.Is(err, os.ErrProcessDone) {
			continue
		} else if err != nil {
			return true, false
		}
		if code != 0 {
			if possibleLogWriter(p, writerPath) {
				return true, false
			}
			continue
		}
		if found != 0 {
			return true, false
		}
	}
	return false, false
}

func possibleLogWriter(p Process, path string) bool {
	name := strings.ToLower(p.Name)
	if p.PID == uint32(os.Getpid()) || p.PPID == uint32(os.Getpid()) || strings.HasPrefix(name, "qemu") ||
		strings.HasPrefix(name, "emulator") || strings.HasPrefix(name, "adb") || strings.Contains(name, "modavd") {
		return true
	}
	image, err := p.ImagePath()
	if err != nil {
		return false
	}
	rootDir := filepath.Dir(filepath.Dir(path))
	if real, err := filepath.EvalSymlinks(rootDir); err == nil {
		rootDir = real
	}
	rel, err := filepath.Rel(rootDir, image)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
