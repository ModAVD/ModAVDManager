// Package logx is the progress log of a modavd command. Messages are Chinese, one line each. The CLI writes them to
// stderr (text, or NDJSON with --json); the MCP server turns them into notifications. Results never go here.
package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

func (l Level) String() string {
	switch l {
	case Debug:
		return "debug"
	case Info:
		return "info"
	case Warn:
		return "warn"
	default:
		return "error"
	}
}

func (l Level) label() string {
	switch l {
	case Debug:
		return "调试"
	case Info:
		return "信息"
	case Warn:
		return "警告"
	default:
		return "错误"
	}
}

// Sink receives every message. It must be safe for concurrent use.
type Sink func(level Level, msg string)

// Logger is nil-safe: methods on a nil *Logger do nothing.
type Logger struct {
	sinks []Sink
}

func New(sinks ...Sink) *Logger { return &Logger{sinks: sinks} }

// Nop discards everything.
func Nop() *Logger { return &Logger{} }

// Tee returns a logger that also feeds sink.
func (l *Logger) Tee(sink Sink) *Logger {
	if l == nil {
		return New(sink)
	}
	sinks := append(append([]Sink(nil), l.sinks...), sink)
	return &Logger{sinks: sinks}
}

func (l *Logger) Log(level Level, msg string) {
	if l == nil {
		return
	}
	for _, s := range l.sinks {
		s(level, msg)
	}
}

func (l *Logger) Debugf(format string, args ...any) { l.Log(Debug, fmt.Sprintf(format, args...)) }
func (l *Logger) Infof(format string, args ...any)  { l.Log(Info, fmt.Sprintf(format, args...)) }
func (l *Logger) Warnf(format string, args ...any)  { l.Log(Warn, fmt.Sprintf(format, args...)) }
func (l *Logger) Errorf(format string, args ...any) { l.Log(Error, fmt.Sprintf(format, args...)) }

// TextSink writes "15:04:05 [信息] message" lines.
func TextSink(w io.Writer, min Level) Sink {
	var mu sync.Mutex
	return func(level Level, msg string) {
		if level < min {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		for _, line := range strings.Split(strings.TrimRight(msg, "\r\n"), "\n") {
			fmt.Fprintf(w, "%s [%s] %s\n", time.Now().Format("15:04:05"), level.label(), line)
		}
	}
}

// JSONSink writes one JSON object per line: {"time":"2026-10-28T15:04:05.123+01:00","level":"info","msg":"..."}.
func JSONSink(w io.Writer, min Level) Sink {
	var mu sync.Mutex
	return func(level Level, msg string) {
		if level < min {
			return
		}
		line, _ := json.Marshal(struct {
			Time  string `json:"time"`
			Level string `json:"level"`
			Msg   string `json:"msg"`
		}{time.Now().Format("2006-01-02T15:04:05.000Z07:00"), level.String(), msg})
		mu.Lock()
		defer mu.Unlock()
		w.Write(append(line, '\n'))
	}
}

// Collector keeps the warnings and errors of one operation, for the "warnings" field of a result.
type Collector struct {
	mu   sync.Mutex
	msgs []string
}

func (c *Collector) Sink() Sink {
	return func(level Level, msg string) {
		if level < Warn {
			return
		}
		c.mu.Lock()
		c.msgs = append(c.msgs, msg)
		c.mu.Unlock()
	}
}

// Messages returns a copy, never nil (so it marshals as []).
func (c *Collector) Messages() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string{}, c.msgs...)
}
