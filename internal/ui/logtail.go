//go:build windows

package ui

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/root"
)

const (
	defaultLogLines = 300
	maxLogLines     = 10000
	// maxLogTail: a logcat grows by megabytes a minute; the page only shows the end.
	maxLogTail = 1 << 20
)

// handleLog is GET /api/instances/{i}/log?kind=emulator|logcat&lines=N: the end of the newest log of that kind.
func (s *server) handleLog(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind == "" {
		kind = "emulator"
	}
	if kind != "emulator" && kind != "logcat" {
		writeError(w, errs.Newf(errs.InvalidArgument, "kind 只能是 emulator 或 logcat（给的是 %q）", kind))
		return
	}
	lines := defaultLogLines
	if v := q.Get("lines"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLogLines {
			writeError(w, errs.Newf(errs.InvalidArgument, "lines 要是 1–%d 的整数（给的是 %q）", maxLogLines, v))
			return
		}
		lines = n
	}
	path, err := newestLog(s.root.LogsDir(), i, kind)
	if err != nil {
		writeError(w, err)
		return
	}
	text, err := tailLines(path, lines, maxLogTail)
	if err != nil {
		writeError(w, errs.Wrap(errs.Internal, err, "读不了日志 "+path+": "+err.Error()))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, text)
}

// newestLog is the newest (by modification time, then name) mas_<i>-*.log or logcat-mas_<i>-*.txt in dir; the names
// emu gives them.
func newestLog(dir string, i int, kind string) (string, error) {
	prefix, suffix, what := root.AvdName(i)+"-", ".log", "模拟器日志"
	if kind == "logcat" {
		prefix, suffix, what = "logcat-"+root.AvdName(i)+"-", ".txt", "客体日志（logcat）"
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", errs.Wrap(errs.Internal, err, "读不了日志文件夹 "+dir+": "+err.Error())
	}
	var best string
	var bestInfo fs.FileInfo
	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if bestInfo == nil || info.ModTime().After(bestInfo.ModTime()) ||
			(info.ModTime().Equal(bestInfo.ModTime()) && name > best) {
			best, bestInfo = name, info
		}
	}
	if best == "" {
		return "", errs.Newf(errs.NotFound, "实例 %d 还没有%s", i, what)
	}
	return filepath.Join(dir, best), nil
}

// tailLines returns the last n lines of the file, reading at most maxBytes from its end. A cut through a line drops
// that partial line; the text is made valid UTF-8 (the emulator may print in the ANSI code page).
func tailLines(path string, n int, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()
	start := max(size-maxBytes, 0)
	buf := make([]byte, size-start)
	m, err := f.ReadAt(buf, start)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	buf = buf[:m]
	if start > 0 {
		if k := bytes.IndexByte(buf, '\n'); k >= 0 {
			buf = buf[k+1:]
		} else {
			buf = nil
		}
	}
	// count n line ends back from the end, ignoring the end of the last line
	end := len(buf)
	if end > 0 && buf[end-1] == '\n' {
		end--
	}
	cut := 0
	for k, seen := end-1, 0; k >= 0; k-- {
		if buf[k] == '\n' {
			if seen++; seen == n {
				cut = k + 1
				break
			}
		}
	}
	return root.DecodeUTF8Replace(buf[cut:]), nil
}
