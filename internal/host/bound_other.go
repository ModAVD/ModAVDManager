//go:build !windows && (!darwin || !arm64 || !cgo)

package host

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"time"
)

// RunBound retains the command behavior for non-Windows builds. Disk operations
// themselves require Windows; kill-on-close jobs are a Windows host feature.
func RunBound(ctx context.Context, exe string, args ...string) ([]byte, []byte, *os.ProcessState, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	return so.Bytes(), se.Bytes(), cmd.ProcessState, err
}
