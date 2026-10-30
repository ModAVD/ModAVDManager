//go:build windows

package ui

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"modavd/internal/adb"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/root"
)

const (
	shotTimeout = 10 * time.Second
	// shotReuse: a capture this young is served again; several views of one instance (list thumbnail, detail) then
	// cost one screencap.
	shotReuse = 300 * time.Millisecond
	// statusMaxAge: how old a polled instance list may be to decide "not booted" without asking adb.
	statusMaxAge = 10 * time.Second
	inputTimeout = 10 * time.Second
	// The display the page's coordinates refer to when the instance config does not say (API.md: 1280x720).
	defaultWidth, defaultHeight = 1280, 720
)

var pngSignature = []byte("\x89PNG\r\n\x1a\n")

// shotCache holds at most one capture in flight per instance and its last result.
type shotCache struct {
	mu sync.Mutex
	by map[int]*shotEntry
}

type shotEntry struct {
	data   []byte
	at     time.Time
	flight *shotCall
}

type shotCall struct {
	done chan struct{}
	data []byte
	err  error
}

func (c *shotCache) entry(i int) *shotEntry {
	if c.by == nil {
		c.by = map[int]*shotEntry{}
	}
	e := c.by[i]
	if e == nil {
		e = &shotEntry{}
		c.by[i] = e
	}
	return e
}

func (c *shotCache) forget(i int) {
	c.mu.Lock()
	delete(c.by, i)
	c.mu.Unlock()
}

func (s *server) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	data, err := s.screenshot(r.Context(), i)
	if err != nil {
		writeError(w, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// screenshot joins the capture in flight, reuses one younger than shotReuse, or starts one. The capture runs on the
// server's context, so a viewer that goes away does not fail the others waiting for it.
func (s *server) screenshot(ctx context.Context, i int) ([]byte, error) {
	s.shots.mu.Lock()
	e := s.shots.entry(i)
	if e.data != nil && time.Since(e.at) < shotReuse {
		data := e.data
		s.shots.mu.Unlock()
		return data, nil
	}
	call := e.flight
	if call == nil {
		call = &shotCall{done: make(chan struct{})}
		e.flight = call
		go s.capture(i, e, call)
	}
	s.shots.mu.Unlock()
	select {
	case <-call.done:
		return call.data, call.err
	case <-ctx.Done():
		return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
}

func (s *server) capture(i int, e *shotEntry, call *shotCall) {
	ctx, cancel := context.WithTimeout(s.ctx, shotTimeout)
	defer cancel()
	data, err := s.captureOnce(ctx, i)
	s.shots.mu.Lock()
	e.flight = nil
	if err == nil {
		e.data, e.at = data, time.Now()
	}
	s.shots.mu.Unlock()
	call.data, call.err = data, err
	close(call.done)
}

func notBooted(i int) error {
	// 409 per API.md; busy is the code of the 409 group that fits "not now" best
	return errs.Newf(errs.Busy, "实例 %d 没有开机", i).WithAdvice("开机后再看画面")
}

func (s *server) captureOnce(ctx context.Context, i int) ([]byte, error) {
	// A recent list that says offline saves an adb call (which would also start the private adb server).
	if in, ok := s.hub.cachedInstance(i, statusMaxAge); ok && !in.Running {
		return nil, notBooted(i)
	}
	res := s.mgr.AdbRun(ctx, adb.RunOptions{Serial: root.Serial(i), Timeout: shotTimeout, Raw: true},
		"exec-out", "screencap", "-p")
	if res.Code == 0 && bytes.HasPrefix(res.Stdout, pngSignature) {
		return res.Stdout, nil
	}
	if ctx.Err() != nil {
		if s.ctx.Err() != nil {
			return nil, errs.New(errs.Canceled, "管理器正在退出")
		}
		return nil, errs.Newf(errs.Timeout, "实例 %d 截图超时（%.0f 秒）", i, shotTimeout.Seconds())
	}
	// Find out why: an instance that is not (or no longer) booted is the usual reason, and a 409 for it.
	ictx, cancel := context.WithTimeout(s.ctx, 6*time.Second)
	defer cancel()
	info, err := s.mgr.Info(ictx, i)
	if err != nil {
		return nil, err
	}
	if !info.Booted {
		return nil, notBooted(i)
	}
	if res.Code != 0 {
		return nil, errs.Newf(errs.Internal, "实例 %d 截图失败: %s", i, clip(res.Out, 300))
	}
	return nil, errs.Newf(errs.Internal, "实例 %d 截图的数据不是 PNG（%d 字节）", i, len(res.Stdout))
}

// clip keeps the first n bytes of s on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

var keyCodes = map[string]string{
	"back":    "KEYCODE_BACK",
	"home":    "KEYCODE_HOME",
	"recents": "KEYCODE_APP_SWITCH",
}

// instanceNow is instance i from a recent poll, else from emu (one getprop).
func (s *server) instanceNow(ctx context.Context, i int) (emu.InstanceInfo, error) {
	if in, ok := s.hub.cachedInstance(i, statusMaxAge); ok {
		return in, nil
	}
	in, err := s.mgr.Info(ctx, i)
	if err != nil {
		return emu.InstanceInfo{}, err
	}
	return *in, nil
}

// handleInput is POST /api/instances/{i}/input: a tap in display pixels, or back / home / recents.
func (s *server) handleInput(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	var body struct {
		Type string   `json:"type"`
		X    *float64 `json:"x"`
		Y    *float64 `json:"y"`
		Key  string   `json:"key"`
	}
	if !decode(w, r, &body) {
		return
	}
	var command string
	switch body.Type {
	case "tap":
		if body.X == nil || body.Y == nil {
			writeError(w, errs.New(errs.Usage, "点击要给 x 和 y"))
			return
		}
	case "key":
		code, ok := keyCodes[body.Key]
		if !ok {
			writeError(w, errs.Newf(errs.InvalidArgument, "key 只能是 back、home 或 recents（给的是 %q）", body.Key))
			return
		}
		command = "input keyevent " + code
	default:
		writeError(w, errs.Newf(errs.InvalidArgument, "type 只能是 tap 或 key（给的是 %q）", body.Type))
		return
	}

	in, err := s.instanceNow(r.Context(), i)
	if err != nil {
		writeError(w, err)
		return
	}
	if body.Type == "tap" {
		wd, ht := in.Config.Width, in.Config.Height
		if wd <= 0 || ht <= 0 {
			wd, ht = defaultWidth, defaultHeight
		}
		x, y := *body.X, *body.Y
		if math.IsNaN(x) || math.IsNaN(y) || x < 0 || y < 0 || x >= float64(wd) || y >= float64(ht) {
			writeError(w, errs.Newf(errs.InvalidArgument, "点击位置 (%g, %g) 不在 %d×%d 的画面里", x, y, wd, ht))
			return
		}
		command = fmt.Sprintf("input tap %d %d", int(x), int(y))
	}
	if !in.Booted {
		writeError(w, notBooted(i))
		return
	}
	res, err := s.mgr.Shell(detached(r), i, command, inputTimeout)
	if err != nil {
		writeError(w, err)
		return
	}
	if res.ExitCode != 0 {
		writeError(w, errs.Newf(errs.Internal, "实例 %d 发送 %s 失败: %s", i, command, clip(res.Output, 300)))
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}
