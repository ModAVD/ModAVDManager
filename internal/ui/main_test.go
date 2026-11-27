package ui

import (
	"os"
	"testing"

	"modavd/internal/testutil"
)

// No test here touches the real per-user directories (component download source, update settings, running emulators).
func TestMain(m *testing.M) { os.Exit(testutil.RunWithTempUserDirs(m)) }
