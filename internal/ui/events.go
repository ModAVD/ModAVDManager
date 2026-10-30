//go:build windows

package ui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
)

// clientBuffer: events queued per event stream. A client that falls this far behind is dropped; EventSource
// reconnects by itself and gets a fresh instance list on connect.
const clientBuffer = 256

type event struct {
	name string
	data []byte
}

// hub fans events out to the connected event streams and caches the latest instance list.
type hub struct {
	mu      sync.Mutex
	clients map[chan event]struct{}
	changed time.Time // last connect / disconnect; server start before the first one
	closed  bool

	list    *emu.ListResult
	listRaw []byte
	listAt  time.Time
	listErr string // last poll error, logged once per distinct message

	refresh chan struct{} // a pending "list again now" request (capacity 1: requests coalesce)
}

func newHub() *hub {
	return &hub{clients: map[chan event]struct{}{}, changed: time.Now(), refresh: make(chan struct{}, 1)}
}

func (h *hub) add() chan event {
	ch := make(chan event, clientBuffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch
	}
	h.clients[ch] = struct{}{}
	h.changed = time.Now()
	return ch
}

func (h *hub) remove(ch chan event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[ch]; ok {
		delete(h.clients, ch)
		close(ch)
		h.changed = time.Now()
	}
}

// idle is the number of connected streams and, when none, how long there has been none.
func (h *hub) idle() (int, time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if n := len(h.clients); n > 0 {
		return n, 0
	}
	return 0, time.Since(h.changed)
}

func (h *hub) count() int {
	n, _ := h.idle()
	return n
}

// send queues data for every stream; never blocks (a full stream is dropped).
func (h *hub) send(name string, data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- event{name, data}:
		default:
			delete(h.clients, ch)
			close(ch)
			h.changed = time.Now()
		}
	}
}

func (h *hub) broadcast(name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.send(name, data)
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for ch := range h.clients {
		delete(h.clients, ch)
		close(ch)
	}
}

// requestRefresh asks the poller to list the instances now (job steps, create / delete ...).
func (h *hub) requestRefresh() {
	select {
	case h.refresh <- struct{}{}:
	default:
	}
}

// storeList caches a fresh list and returns its JSON.
func (h *hub) storeList(res *emu.ListResult) []byte {
	data, err := json.Marshal(res)
	if err != nil {
		return nil
	}
	h.mu.Lock()
	h.list, h.listRaw, h.listAt, h.listErr = res, data, time.Now(), ""
	h.mu.Unlock()
	return data
}

// cachedListJSON is the cached list when younger than maxAge.
func (h *hub) cachedListJSON(maxAge time.Duration) ([]byte, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listRaw == nil || time.Since(h.listAt) > maxAge {
		return nil, false
	}
	return h.listRaw, true
}

// cachedInstance is instance i from a list younger than maxAge.
func (h *hub) cachedInstance(i int, maxAge time.Duration) (emu.InstanceInfo, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.list == nil || time.Since(h.listAt) > maxAge {
		return emu.InstanceInfo{}, false
	}
	for _, in := range h.list.Instances {
		if in.Index == i {
			return in, true
		}
	}
	return emu.InstanceInfo{}, false
}

// dropCached forgets the cached list (after a create / delete it is wrong until the next poll).
func (h *hub) dropCached() {
	h.mu.Lock()
	h.list, h.listRaw = nil, nil
	h.mu.Unlock()
}

// pollErr records a poll failure; true when its message differs from the previous one.
func (h *hub) pollErr(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listErr == msg {
		return false
	}
	h.listErr = msg
	return true
}

// pollLoop lists the instances every pollEvery and on request, but only while some event stream is connected: a
// list costs one adb call per running instance.
func (s *server) pollLoop() {
	t := time.NewTicker(s.pollEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.hub.refresh:
		}
		if s.hub.count() == 0 {
			continue
		}
		s.refreshInstances()
	}
}

func (s *server) refreshInstances() {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	res, err := s.poll.List(ctx)
	if err != nil {
		if s.ctx.Err() != nil {
			return
		}
		msg := errs.As(err).Message
		if s.hub.pollErr(msg) {
			s.log.Warnf("刷新实例列表失败: %s", msg)
			s.emitLog(logx.Warn, "刷新实例列表失败: "+msg, "")
		}
		return
	}
	if data := s.hub.storeList(res); data != nil {
		s.hub.send("instances", data)
	}
}

type logEvent struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
	JobID string `json:"job_id"`
}

// emitLog sends one log line to the event streams (info and above).
func (s *server) emitLog(level logx.Level, msg, jobID string) {
	if level < logx.Info {
		return
	}
	s.hub.broadcast("log", logEvent{
		Time:  time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
		Level: level.String(),
		Msg:   msg,
		JobID: jobID,
	})
}

func (s *server) eventLogSink(jobID string) logx.Sink {
	return func(level logx.Level, msg string) { s.emitLog(level, msg, jobID) }
}

// handleEvents is GET /api/events: text/event-stream with instances / job / log / ping (API.md).
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		writeError(w, errs.New(errs.Internal, "这个连接不支持事件流"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	ch := s.hub.add()
	defer s.hub.remove(ch)
	w.WriteHeader(http.StatusOK)

	write := func(name string, data []byte) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	// Reconnect quickly after a drop (the default is about 3 s).
	fmt.Fprint(w, "retry: 1500\n\n")
	if data, ok := s.hub.cachedListJSON(s.pollEvery); ok {
		if !write("instances", data) {
			return
		}
	} else {
		fl.Flush()
		s.hub.requestRefresh() // the poller sends it to every stream, this one included
	}

	ping := time.NewTicker(s.pingEvery)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok || !write(ev.name, ev.data) {
				return
			}
		case <-ping.C:
			if !write("ping", []byte("{}")) {
				return
			}
		}
	}
}
