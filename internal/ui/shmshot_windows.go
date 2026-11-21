//go:build windows

package ui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

// The frame comes from the emulator's shared memory through extras\modavd_extras.dll (ModAVDExtras), the same library
// MAA and MaaFramework use, so no format is parsed here. Nothing stays mapped between thumbnails: the library is
// loaded, one frame is copied, the handle is closed and the library is freed again. A manager that kept either would
// block a restarting emulator from creating a new frame mapping (ModAVD/ModAVD#210) and the root's own update from
// replacing the DLL.

const (
	mxFmtBGRA     = 1
	mxOpenNoStart = 1 << 0 // never turn frame sharing on: a thumbnail must not change the instance
	mxOK          = 0
)

type mxOpenParams struct {
	Size        uint32
	_           uint32
	AdbAddress  *byte
	ConsolePort int32
	_           int32
	ClientName  *byte
	Flags       uint32
	TimeoutMs   int32
}

type mxFrameInfo struct {
	Size        uint32
	Width       int32
	Height      int32
	Stride      int32
	Format      uint32
	FrameNumber uint32
	TimestampUs uint64
}

// shmMu: one capture at a time, so a library one capture frees is never in use by another.
var shmMu sync.Mutex

func shmFrame(rootDir, serial string) ([]byte, error) {
	dll := filepath.Join(rootDir, "extras", "modavd_extras.dll")
	if _, err := os.Stat(dll); err != nil {
		return nil, err
	}
	shmMu.Lock()
	defer shmMu.Unlock()
	lib, err := syscall.LoadLibrary(dll)
	if err != nil {
		return nil, err
	}
	defer syscall.FreeLibrary(lib)
	proc := func(name string) (uintptr, error) { return syscall.GetProcAddress(lib, name) }
	open, err1 := proc("modavd_open")
	closeH, err2 := proc("modavd_close")
	capture, err3 := proc("modavd_capture")
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, err
	}

	addr, err := syscall.BytePtrFromString(serial)
	if err != nil {
		return nil, err
	}
	name, _ := syscall.BytePtrFromString("ModAVD manager thumbnail")
	p := mxOpenParams{AdbAddress: addr, ClientName: name, Flags: mxOpenNoStart}
	p.Size = uint32(unsafe.Sizeof(p))
	var h uintptr
	if r, _, _ := syscall.SyscallN(open, uintptr(unsafe.Pointer(&p)), uintptr(unsafe.Pointer(&h))); int32(r) != mxOK {
		return nil, fmt.Errorf("modavd_open: %d", int32(r))
	}
	defer syscall.SyscallN(closeH, h)

	call := func(buf []byte, info *mxFrameInfo) int32 {
		var b, n uintptr
		if len(buf) > 0 {
			b, n = uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf))
		}
		r, _, _ := syscall.SyscallN(capture, h, 0, mxFmtBGRA, 0, b, n, uintptr(unsafe.Pointer(info)))
		return int32(r)
	}
	var info mxFrameInfo
	info.Size = uint32(unsafe.Sizeof(info))
	for try := 0; try < 2; try++ { // the screen may change size between the probe and the copy
		if r := call(nil, &info); r != mxOK {
			return nil, fmt.Errorf("modavd_capture (probe): %d", r)
		}
		if info.Width <= 0 || info.Height <= 0 || info.Stride < info.Width*4 || int64(info.Stride)*int64(info.Height) > 1<<28 {
			return nil, errors.New("shared frame has a bad size")
		}
		buf := make([]byte, int(info.Stride)*int(info.Height))
		switch r := call(buf, &info); r {
		case mxOK:
			return bgraPNG(buf, int(info.Width), int(info.Height), int(info.Stride))
		case -5: // MODAVD_E_BUFFER: grew in between, probe again
			continue
		default:
			return nil, fmt.Errorf("modavd_capture: %d", r)
		}
	}
	return nil, errors.New("shared frame kept changing size")
}
