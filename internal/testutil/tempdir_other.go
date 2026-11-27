//go:build !windows

// Copyright 2026 Ji Mao Cai
// SPDX-License-Identifier: AGPL-3.0-or-later

package testutil

func retryableRemove(error) bool { return false }
