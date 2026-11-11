//go:build !windows

package ui

import "modavd/internal/errs"

// realNewTray: the tray icon and the boss keys are Windows only.
func realNewTray(string, trayEvents) (trayHost, error) {
	return nil, errs.New(errs.Unsupported, "当前平台没有托盘图标和老板键")
}
