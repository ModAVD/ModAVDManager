//go:build !windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Windows alone uses the manager hand-off gate.
var updatingNotice = func() error { return nil }
