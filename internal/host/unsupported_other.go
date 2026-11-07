//go:build !windows

package host

import (
	"errors"
	"os"
	"os/exec"

	"modavd/internal/errs"
)

var errUnsupported = errs.New(errs.Unsupported, "当前平台暂不支持此操作")

const HideConsole = 0

// L3Group is the cache topology exposed by the host interface.
type L3Group struct {
	Size uint32
	Mask uint64
}

// HideConsoleCmd is a no-op on hosts without a Windows console.
func HideConsoleCmd(*exec.Cmd) {}

// SetAboveNormalPriority calls SetPriorityClass with ABOVE_NORMAL_PRIORITY_CLASS.
func SetAboveNormalPriority(Process) error { return errUnsupported }

// SetAffinity calls SetProcessAffinityMask on Windows.
func SetAffinity(Process, uint64) error { return errUnsupported }

// GetAffinity calls GetProcessAffinityMask on Windows.
func GetAffinity(Process) (uint64, error) { return 0, errUnsupported }

// L3Groups lists the L3 caches of the host.
func L3Groups() []L3Group { return nil }

// SharedMemoryExists calls OpenFileMappingW for the Windows named mapping.
func SharedMemoryExists(string) bool { return false }

// GetCompressedFileSize calls GetCompressedFileSizeW for the Windows file.
func GetCompressedFileSize(string) (uint64, error) { return 0, errUnsupported }

// MoveFileReplaceThrough calls MoveFileExW (REPLACE_EXISTING | WRITE_THROUGH).
func MoveFileReplaceThrough(string, string) error { return errUnsupported }

// Protect calls CryptProtectData (DPAPI) for the current Windows user.
func Protect([]byte, []byte) ([]byte, error) { return nil, errUnsupported }

// Unprotect calls CryptUnprotectData (DPAPI) for the current Windows user.
func Unprotect([]byte, []byte) ([]byte, error) { return nil, errUnsupported }

// SetEmulatorWindowsVisible finds the emulator window and calls ShowWindow on Windows.
func SetEmulatorWindowsVisible(uint32, bool) (int, error) { return 0, errUnsupported }

// GetConsoleWindow calls the Windows GetConsoleWindow API.
func GetConsoleWindow() uintptr { return 0 }

// MessageBox calls MessageBoxW from Windows user32.
func MessageBox(string) error { return errors.New("当前平台暂不支持消息框") }

// OpenStableRead requires Windows file-sharing semantics.
func OpenStableRead(string) (*os.File, error) { return nil, errUnsupported }

// FileChangeStamp disables caching without a supported file identity API.
func FileChangeStamp(*os.File) string { return "" }

func InfoBox(string) error { return errors.New("当前平台暂不支持消息框") }
