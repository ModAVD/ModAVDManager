//go:build windows

package adb

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"modavd/internal/errs"
)

// defaultConsoleTimeout is AUTO-MAS console_command's default.
const defaultConsoleTimeout = 10 * time.Second

const consoleTokenFile = ".emulator_console_auth_token"

// ConsoleToken is the console auth token the emulator writes at first start: %USERPROFILE%\.emulator_console_auth_token,
// trimmed; "" when missing (then no auth line is sent).
func ConsoleToken() string {
	home := os.Getenv("USERPROFILE")
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, consoleTokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Console sends one command to the emulator console on 127.0.0.1:port (the same as `adb emu`, but independent of the
// adb server) and returns its output without the final OK line. Protocol: read until a line "OK" (banner), send
// "auth <token>\r\n" when there is a token and read until OK, send "<command>\r\n", read until "OK" (success) or a
// line starting "KO" (failure, the KO line is part of the error), then "quit\r\n" best effort. The whole exchange is
// bounded by timeout. Errors are errs.Internal with "控制台 ..." messages like AUTO-MAS console_command.
//
// The console matters because the private server forgets the emulator when it restarts (it registers only once),
// and then `adb emu` cannot reach it any more; the console stays. timeout <= 0 means 10 s. A canceled ctx gives
// errs.Canceled.
func Console(ctx context.Context, port int, command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultConsoleTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	timedOut := func(err error) error {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("控制台 %d 命令 %s 超时", port, pyRepr(command)))
	}
	ioFailed := func(err error) error {
		if ctx.Err() != nil {
			return canceled(ctx)
		}
		if tctx.Err() != nil || isTimeout(err) {
			return timedOut(err)
		}
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("连不上控制台 %d: %v", port, err))
	}

	var d net.Dialer
	conn, err := d.DialContext(tctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return "", ioFailed(err)
	}
	defer conn.Close()
	deadline, _ := tctx.Deadline()
	conn.SetDeadline(deadline)
	// wake a blocked read or write at once when the caller cancels
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	s := consoleSession{w: conn, r: bufio.NewReader(conn)}
	ok, text, err := s.readUntilStatus()
	if err != nil {
		return "", ioFailed(err)
	}
	if !ok {
		return "", errs.Newf(errs.Internal, "控制台 %d 没有就绪: %s", port, text)
	}
	if token := ConsoleToken(); token != "" {
		if err := s.send("auth " + token); err != nil {
			return "", ioFailed(err)
		}
		if ok, text, err = s.readUntilStatus(); err != nil {
			return "", ioFailed(err)
		}
		if !ok {
			return "", errs.Newf(errs.Internal, "控制台 %d 鉴权失败: %s", port, text)
		}
	}
	if err := s.send(command); err != nil {
		return "", ioFailed(err)
	}
	if ok, text, err = s.readUntilStatus(); err != nil {
		return "", ioFailed(err)
	}
	if !ok {
		return "", errs.Newf(errs.Internal, "控制台命令 %s 失败: %s", pyRepr(command), text)
	}
	s.send("quit") // best effort; the console closes the connection on it
	return text, nil
}

type consoleSession struct {
	w io.Writer
	r *bufio.Reader
}

func (s consoleSession) send(line string) error {
	_, err := io.WriteString(s.w, line+"\r\n")
	return err
}

// readUntilStatus reads lines until "OK" (ok) or a line starting "KO" (kept in text) or the end of the connection
// (not ok). err is set only for other read errors (deadline, reset), like AUTO-MAS where those raise.
func (s consoleSession) readUntilStatus() (ok bool, text string, err error) {
	var lines []string
	for {
		raw, rerr := s.r.ReadString('\n')
		if raw != "" {
			line := strings.TrimRight(utf8Replace(raw), "\r\n")
			switch {
			case line == "OK":
				return true, strings.Join(lines, "\n"), nil
			case strings.HasPrefix(line, "KO"):
				lines = append(lines, line)
				return false, strings.Join(lines, "\n"), nil
			}
			lines = append(lines, line)
		}
		if rerr == io.EOF {
			return false, strings.Join(lines, "\n"), nil
		}
		if rerr != nil {
			return false, strings.Join(lines, "\n"), rerr
		}
	}
}

// pyRepr quotes s like Python repr() of a str, so messages read like AUTO-MAS's f"{command!r}" ('kill').
func pyRepr(s string) string {
	q := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		q = '"'
	}
	var b strings.Builder
	b.WriteRune(q)
	for _, r := range s {
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case r == q:
			b.WriteRune('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteRune(q)
	return b.String()
}
