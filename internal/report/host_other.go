//go:build !windows && (!darwin || !arm64 || !cgo)

package report

import (
	"io/fs"

	"modavd/internal/errs"
)

// Hosts without a native diagnostics implementation remain unsupported.

const reportsAvailable = false

func unsupported(what string) error {
	return errs.New(errs.Unsupported, what+"目前只支持 Windows")
}

// newHost: on Windows the WMI classes, the registry, the process and TCP listener tables of this computer.
func newHost() (Host, error) { return nil, unsupported("读取电脑配置") }

// desktopDir: on Windows [Environment]::GetFolderPath('Desktop').
func desktopDir() (string, error) { return "", unsupported("找桌面文件夹") }

// copyClipboard: on Windows the text as CF_UNICODETEXT (Set-Clipboard).
func copyClipboard(string) error { return unsupported("复制到剪贴板") }

// openExplorer: on Windows explorer.exe /select,"<path>".
func openExplorer(string) error { return unsupported("打开资源管理器") }

// isHidden: on Windows FILE_ATTRIBUTE_HIDDEN; dot files are not hidden for Get-ChildItem either.
func isHidden(fs.FileInfo) bool { return false }

// longPath: on Windows the path with its 8.3 short names expanded ([IO.Path]::GetFullPath); there are none here.
func longPath(p string) string { return p }
