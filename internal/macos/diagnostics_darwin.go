//go:build darwin && cgo

package macos

/*
#include <sys/types.h>
#include <sys/sysctl.h>
#include <stdint.h>
#include <stdlib.h>
#include <errno.h>
#include <fcntl.h>

static int diagnostic_openat(int fd, const char *name, int flags) {
    return openat(fd, name, flags);
}

static int diagnostic_number(const char *name, uint64_t *value) {
    uint64_t raw = 0;
    size_t size = sizeof(raw);
    if (sysctlbyname(name, &raw, &size, NULL, 0) != 0) return errno;
    if (size != sizeof(uint32_t) && size != sizeof(uint64_t)) return EINVAL;
    *value = raw;
    return 0;
}
*/
import "C"

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// OpenDiagnostic walks from an already selected root directory, rejecting links
// at every child component. Parent directory descriptors pin the walk even if a
// path is renamed concurrently. O_NONBLOCK prevents a FIFO from hanging a read.
func OpenDiagnostic(rootDir, relative string, directory bool) (*os.File, error) {
	if !filepath.IsLocal(relative) {
		return nil, errors.New("诊断路径不在所选目录内")
	}
	fd, err := syscall.Open(rootDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	parts := strings.Split(filepath.Clean(relative), string(filepath.Separator))
	for n, part := range parts {
		flags := C.int(C.O_RDONLY | C.O_NOFOLLOW | C.O_NONBLOCK | C.O_CLOEXEC)
		if n < len(parts)-1 || directory {
			flags |= C.O_DIRECTORY
		}
		name := C.CString(part)
		next, openErr := C.diagnostic_openat(C.int(fd), name, flags)
		C.free(unsafe.Pointer(name))
		syscall.Close(fd)
		if next < 0 {
			return nil, &fs.PathError{Op: "open", Path: relative, Err: openErr}
		}
		fd = int(next)
	}
	file := os.NewFile(uintptr(fd), relative)
	info, err := file.Stat()
	if err != nil || (!directory && !info.Mode().IsRegular()) || (directory && !info.IsDir()) {
		file.Close()
		return nil, errors.New("诊断源不是预期的常规文件或目录")
	}
	return file, nil
}

// SystemNumber reads one non-identifying numeric sysctl. Callers select fixed
// names for CPU counts, physical memory and Hypervisor.framework support.
func SystemNumber(name string) (uint64, error) {
	key := C.CString(name)
	defer C.free(unsafe.Pointer(key))
	var value C.uint64_t
	if code := C.diagnostic_number(key, &value); code != 0 {
		return 0, fmt.Errorf("读取 %s: %w", name, syscall.Errno(code))
	}
	return uint64(value), nil
}

// SystemString is used only for the CPU model and Darwin build, never the host
// name, hardware serial number, UUID or network configuration.
func SystemString(name string) (string, error) { return syscall.Sysctl(name) }

// DiagnosticResult is the bounded output of a read-only diagnostic command.
type DiagnosticResult struct {
	Stdout, Stderr []byte
	ExitCode       int
	TimedOut       bool
}

type diagnosticBuffer struct{ bytes.Buffer }

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	const limit = 1 << 20
	if len(p) > limit-b.Len() {
		return 0, errors.New("诊断命令输出超过 1 MiB")
	}
	return b.Buffer.Write(p)
}

// RunDiagnostic never invokes a shell. Output and process lifetime are bounded;
// a child retaining the output pipes cannot hold a report open indefinitely.
func RunDiagnostic(ctx context.Context, exe string, args []string, timeout time.Duration) (DiagnosticResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, exe, args...)
	cmd.Dir = filepath.Dir(exe)
	cmd.WaitDelay = time.Second
	var stdout, stderr diagnosticBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := DiagnosticResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if cctx.Err() != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.TimedOut = true
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, err
}

// CopyClipboard sends UTF-8 through stdin, so neither the text nor its secrets
// can appear in a process argument list. It is called only on user request.
func CopyClipboard(text string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/pbcopy")
	cmd.Stdin = strings.NewReader(text)
	cmd.Env = append(os.Environ(), "LANG=en_US.UTF-8", "LC_CTYPE=UTF-8")
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("复制到剪贴板失败: %w", err)
	}
	return nil
}

// RevealInFinder selects one completed diagnostic archive without a shell.
func RevealInFinder(path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("问题包不存在或不是常规文件")
	}
	result, err := RunDiagnostic(context.Background(), "/usr/bin/open", []string{"-R", path}, 5*time.Second)
	if err == nil && (result.TimedOut || result.ExitCode != 0) {
		err = errors.New("Finder 未能显示问题包")
	}
	return err
}
