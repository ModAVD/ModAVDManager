package emu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/win"
)

// LogcatCaptureArg is routed by the root's console executable before normal command parsing.
const LogcatCaptureArg = "__logcat"

// Rotate at the first newline at or beyond 256 MiB, so a record never spans two files.
const logcatSegmentBytes int64 = 256 << 20

// logcatWriter owns the only host output handle. adb keeps writing to the same pipe during every rotation.
// The next file is opened before the previous one is closed; pruning therefore always sees the active file busy.
type logcatWriter struct {
	dir       string
	i         int
	limit     int64
	file      *os.File
	size      int64
	lineStart bool
	nextStamp time.Time
	prune     func()
}

func newLogcatWriter(dir string, i int, limit int64, prune func()) (*logcatWriter, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("logcat segment size must be positive")
	}
	w := &logcatWriter{dir: dir, i: i, limit: limit, nextStamp: time.Now().Truncate(time.Second), prune: prune}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := "logcat-" + root.AvdName(i) + "-"
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		stamp, err := time.ParseInLocation("20060102-150405", strings.TrimSuffix(strings.TrimPrefix(e.Name(), prefix), ".txt"), time.Local)
		if err == nil && !stamp.Before(w.nextStamp) {
			w.nextStamp = stamp.Add(time.Second)
		}
	}
	if err := w.rotate(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *logcatWriter) rotate() error {
	// Keep the existing timestamp-only naming contract, including collect-info's readers. Exclusive creation
	// prevents truncation on a same-second restart or fast rotation; occupied seconds advance by one.
	stamp := time.Now().Truncate(time.Second)
	if stamp.Before(w.nextStamp) {
		stamp = w.nextStamp
	}
	var next *os.File
	for {
		path := filepath.Join(w.dir, "logcat-"+root.AvdName(w.i)+"-"+stamp.Format("20060102-150405")+".txt")
		var err error
		next, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if errors.Is(err, os.ErrExist) {
			stamp = stamp.Add(time.Second)
			continue
		}
		if err != nil {
			return err
		}
		break
	}
	old := w.file
	w.file, w.size, w.lineStart, w.nextStamp = next, 0, true, stamp.Add(time.Second)
	if old != nil {
		if err := old.Close(); err != nil {
			return err
		}
	}
	if w.prune != nil {
		w.prune()
	}
	return nil
}

func (w *logcatWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.size >= w.limit && w.lineStart {
			if err := w.rotate(); err != nil {
				return written, err
			}
		}
		n := len(p)
		if room := w.limit - w.size; room > 0 {
			if int64(n) > room {
				n = int(room)
			}
		} else if end := bytes.IndexByte(p, '\n'); end >= 0 {
			n = end + 1
		}
		got, err := w.file.Write(p[:n])
		w.size += int64(got)
		written += got
		if got > 0 {
			w.lineStart = p[got-1] == '\n'
		}
		p = p[got:]
		if err != nil {
			return written, err
		}
		if got != n {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (w *logcatWriter) Close() error { return w.file.Close() }

// RunLogcatCapture is an orphaned host writer, not a guest logcat -f (which would write inside Android).
// A per-instance lifetime lock prevents duplicate helpers while adb starts or a previous helper drains its pipe.
// It also watches the qemu identity, so even a stale adb client is ended when this boot's qemu exits.
func RunLogcatCapture(args []string, stderr io.Writer) int {
	return runLogcatCapture(args, stderr, logcatSegmentBytes)
}

func runLogcatCapture(args []string, stderr io.Writer, limit int64) int {
	if len(args) != 4 {
		return 2
	}
	i, err := strconv.Atoi(args[1])
	pid, pidErr := strconv.ParseUint(args[2], 10, 32)
	created, createdErr := strconv.ParseUint(args[3], 10, 64)
	if errors.Join(err, pidErr, createdErr) != nil || !root.ValidIndex(i) || pid == 0 || created == 0 {
		return 2
	}
	r, err := root.New(args[0])
	if err != nil {
		return 1
	}
	m := New(r, logx.New(logx.JSONSink(stderr, logx.Info)))
	l, free, err := win.Lock(m.lockPath(i, "logcat"), 5*time.Second)
	if err != nil || !free {
		return 1
	}
	defer l.Unlock()
	q := win.Process{PID: uint32(pid), Created: created}
	if !q.Alive() {
		return 0
	}
	// A legacy capture, or a helper started concurrently, may already have an adb client.
	if running, err := m.findLogcatProcesses(r.Ports().Serial(i)); err != nil || len(running) > 0 {
		return 1
	}
	w, err := newLogcatWriter(r.LogsDir(), i, limit, func() { m.pruneLogs(i) })
	if err != nil {
		return 1
	}
	defer w.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for !q.Wait(100 * time.Millisecond) {
			if ctx.Err() != nil {
				return
			}
		}
		cancel()
	}()
	if err := captureLogcat(ctx, r.AdbExe(), logcatArgs(r.AdbServerPort(), r.Ports().Serial(i)), m.ADB.Env(), w); err != nil && ctx.Err() == nil {
		m.Log.Warnf("实例 %d 客体日志采集已结束: %v", i, err)
		return 1
	}
	return 0
}

func captureLogcat(ctx context.Context, exe string, args, env []string, output io.Writer) error {
	r, pipe, err := os.Pipe()
	if err != nil {
		return err
	}
	defer r.Close()
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = filepath.Dir(exe), env, pipe, pipe
	win.HideConsoleCmd(cmd)
	if err := cmd.Start(); err != nil {
		pipe.Close()
		return err
	}
	pipe.Close()
	_, copyErr := io.Copy(output, r)
	if copyErr != nil {
		// Do not leave a live adb capture after a disk error: ensureLogcat must be able to start it again.
		cmd.Process.Kill()
	}
	return errors.Join(copyErr, cmd.Wait())
}
