//go:build windows

package win

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestWaitBoundCancelWithRetainedJobHandle(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer null.Close()
			p, job, err := startBound(testExe(t), []string{modeChild, "60"}, [3]*os.File{null, null, null})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { p.Kill(); p.Release() })
			self, err := syscall.GetCurrentProcess()
			if err != nil {
				syscall.CloseHandle(job)
				t.Fatal(err)
			}
			var retained syscall.Handle
			if err := syscall.DuplicateHandle(self, job, self, &retained, 0, false, syscall.DUPLICATE_SAME_ACCESS); err != nil {
				syscall.CloseHandle(job)
				t.Fatal(err)
			}
			defer func() {
				if retained != 0 {
					syscall.CloseHandle(retained)
				}
			}()
			// This duplicate remains open throughout cancellation, like an observer
			// holding a job handle. Closing waitBound's handle alone cannot kill it.
			ctx, cancel := context.WithCancel(context.Background())
			if mode == "timeout" {
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			done := make(chan struct{})
			var state *os.ProcessState
			var waitErr error
			go func() { state, waitErr = waitBound(ctx, p, job); close(done) }()
			if mode == "cancel" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				// Release the duplicate so a missing TerminateJobObject cannot leak
				// the helper or its waiter while reporting the failed assertion.
				syscall.CloseHandle(retained)
				retained = 0
				p.Kill()
				<-done
				t.Fatal("child survived cancellation with another job handle open")
			}
			if waitErr != nil || state == nil || state.ExitCode() != 1 {
				t.Fatalf("state=%v error=%v", state, waitErr)
			}
			if ctx.Err() == nil {
				t.Fatal("context was not canceled or expired")
			}
		})
	}
}
