// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"os"
	"path/filepath"

	"modavd/internal/host"
	"modavd/internal/update"
)

// A message box in this process would keep ModAVD.exe mapped until the user
// dismisses it and prevent replacement. Let the Windows system host show the
// fixed notice, then exit this manager before the CLI's final scan and image gate.
var updatingNotice = func() error {
	exe := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	script := "$shell = New-Object -ComObject WScript.Shell; [void]$shell.Popup('" + update.UpdatingMessage + "', 15, 'ModAVD', 64)"
	_, err := host.StartDetached(exe, []string{"-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script}, host.StartOptions{})
	return err
}
