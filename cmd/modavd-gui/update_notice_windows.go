// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"modavd/internal/update"
	"modavd/internal/win"
)

// A message box in this process would keep ModAVD.exe mapped until the user
// dismisses it and prevent replacement. win.SessionNotice has the session's own
// system process show the fixed notice (no child process, nothing of ours left
// mapped) and returns at once; this manager then exits before the CLI's final
// scan and image gate. The box closes by itself after 15 seconds.
var updatingNotice = func() error {
	return win.SessionNotice("ModAVD", update.UpdatingMessage, 15)
}
