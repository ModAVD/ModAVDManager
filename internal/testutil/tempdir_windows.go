// Copyright 2026 Ji Mao Cai
// SPDX-License-Identifier: AGPL-3.0-or-later

package testutil

import (
	"errors"
	"syscall"
)

func retryableRemove(err error) bool {
	// Windows can still report a directory as nonempty after its children
	// were deleted. testing.TempDir retries the other two errors, but not 145.
	return errors.Is(err, syscall.ERROR_DIR_NOT_EMPTY) ||
		errors.Is(err, syscall.ERROR_ACCESS_DENIED) ||
		errors.Is(err, syscall.Errno(32)) // ERROR_SHARING_VIOLATION
}
