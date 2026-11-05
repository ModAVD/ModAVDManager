//go:build windows

package emu

import (
	"os"
	"syscall"

	"modavd/internal/errs"
)

// checkCompactOverlayLinks inspects only the overlay's filesystem entry. Replacing
// a link would detach this name from its target or from the other hardlink names.
func checkCompactOverlayLinks(path string, st os.FileInfo) error {
	reparseError := func() error {
		return errs.Newf(errs.Unsupported, "数据盘覆盖层是 Windows 重解析点（如符号链接），无法整理磁盘: %s", path).
			WithAdvice("请先备份数据，再将此路径的覆盖层改为独立普通文件后整理")
	}
	if st.Sys().(*syscall.Win32FileAttributeData).FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return reparseError()
	}
	if !st.Mode().IsRegular() {
		return nil // DiskCompact reports a missing overlay for directories and other non-files.
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return errs.Wrap(errs.Internal, err, "读取数据盘覆盖层链接信息失败: "+path+": "+err.Error())
	}
	// Query metadata only, with every share mode. OPEN_REPARSE_POINT prevents
	// following a link if the entry changed since Lstat.
	h, err := syscall.CreateFile(p, 0, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return errs.Wrap(errs.Internal, err, "读取数据盘覆盖层链接信息失败: "+path+": "+err.Error())
	}
	defer syscall.CloseHandle(h)
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &info); err != nil {
		return errs.Wrap(errs.Internal, err, "读取数据盘覆盖层链接信息失败: "+path+": "+err.Error())
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return reparseError()
	}
	if info.NumberOfLinks > 1 {
		return errs.Newf(errs.Unsupported, "数据盘覆盖层有 %d 个硬链接名称，无法整理磁盘: %s", info.NumberOfLinks, path).
			WithAdvice("请先备份数据，再将此路径的覆盖层复制为独立普通文件后整理")
	}
	return nil
}
