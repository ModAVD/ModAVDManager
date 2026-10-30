//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"modavd/internal/logx"
)

const (
	logFileName = "modavd-ui.log"
	// logMaxBytes: past this the log starts over, keeping the previous one as .1 (one backup, at most ~10 MB in all).
	logMaxBytes = 5 << 20
)

// logFile is <root>\logs\modavd-ui.log, appended to. Write failures are dropped: there is nowhere else to report
// them in a GUI process, and the manager works without its log.
type logFile struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openLogFile(dir string) *logFile {
	l := &logFile{path: filepath.Join(dir, logFileName)}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return l
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open()
	return l
}

// open (with mu held) rotates first when the file is already past the limit.
func (l *logFile) open() {
	if st, err := os.Stat(l.path); err == nil && st.Size() >= logMaxBytes {
		// fails while another manager process (a second launch) holds the file open: keep appending then
		os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		l.f = nil
		return
	}
	l.f = f
	if st, err := f.Stat(); err == nil {
		l.size = st.Size()
	}
}

func (l *logFile) write(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.size >= logMaxBytes {
		l.f.Close()
		l.open()
		if l.f == nil {
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}

// sink writes "2026-10-28 15:04:05.000 [信息] message" lines (multi-line messages indented).
func (l *logFile) sink(min logx.Level) logx.Sink {
	return func(level logx.Level, msg string) {
		if level < min {
			return
		}
		msg = strings.ReplaceAll(strings.TrimRight(msg, "\r\n"), "\n", "\n    ")
		l.write(fmt.Sprintf("%s [%s] %s\n", time.Now().Format("2006-01-02 15:04:05.000"), levelLabel(level), msg))
	}
}

func levelLabel(l logx.Level) string {
	switch l {
	case logx.Debug:
		return "调试"
	case logx.Info:
		return "信息"
	case logx.Warn:
		return "警告"
	default:
		return "错误"
	}
}

func (l *logFile) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
