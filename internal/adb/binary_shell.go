// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package adb

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// BinaryShell streams shell-v2 frames directly. On Windows adb.exe shell uses text-mode
// stdin/stdout even with -T, while exec-in/out do not report the remote exit status.
type BinaryShell struct {
	conn      net.Conn
	stop      func() bool
	remaining uint32
	stderr    string
	exited    bool
}

func OpenBinaryShell(ctx context.Context, port int, serial, command string) (*BinaryShell, error) {
	if port <= 0 || port == 5037 || port > 65535 {
		return nil, fmt.Errorf("invalid private adb port %d", port)
	}
	addr, service, err := net.SplitHostPort(serial)
	devicePort, portErr := strconv.ParseUint(service, 10, 16)
	if err != nil || addr != "127.0.0.1" || portErr != nil || devicePort == 0 {
		return nil, fmt.Errorf("invalid adb endpoint %q", serial)
	}
	dial := net.Dialer{Timeout: 5 * time.Second}
	c, err := dial.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return nil, err
	}
	s := &BinaryShell{conn: c, stop: context.AfterFunc(ctx, func() { c.Close() })}
	for _, service := range []string{"host:transport:" + serial, "shell,v2,raw:" + command} {
		if err := s.request(service); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *BinaryShell) request(service string) error {
	if len(service) > 65535 {
		return fmt.Errorf("adb service too long")
	}
	s.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := fmt.Fprintf(s.conn, "%04x%s", len(service), service); err != nil {
		return err
	}
	var status [4]byte
	if _, err := io.ReadFull(s.conn, status[:]); err != nil {
		return err
	}
	switch string(status[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		reason, err := readHexPrefixed(s.conn)
		if err != nil {
			return err
		}
		return fmt.Errorf("adb: %s", reason)
	}
	return fmt.Errorf("adb: %q", status)
}

func (s *BinaryShell) Close() { s.stop(); s.conn.Close() }

func (s *BinaryShell) readFull(p []byte) error {
	_, err := io.ReadFull(s.conn, p)
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (s *BinaryShell) Read(p []byte) (int, error) {
	for !s.exited {
		s.conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		if s.remaining != 0 {
			n, err := s.conn.Read(p[:min(len(p), int(s.remaining))])
			s.remaining -= uint32(n)
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return n, err
		}
		var h [5]byte
		if err := s.readFull(h[:]); err != nil {
			return 0, err
		}
		n := binary.LittleEndian.Uint32(h[1:])
		if n > 1<<20 {
			return 0, fmt.Errorf("adb shell frame: %d", n)
		}
		switch h[0] {
		case 1:
			s.remaining = n
		case 2:
			msg := make([]byte, n)
			if err := s.readFull(msg); err != nil {
				return 0, err
			}
			s.stderr += string(msg)
			if len(s.stderr) > 1024 {
				s.stderr = s.stderr[len(s.stderr)-1024:]
			}
		case 3:
			if n != 1 {
				return 0, fmt.Errorf("adb shell exit frame: %d", n)
			}
			var b [1]byte
			if err := s.readFull(b[:]); err != nil {
				return 0, err
			}
			s.exited = true
			if b[0] != 0 {
				return 0, fmt.Errorf("adb shell exit %d: %s", b[0], s.stderr)
			}
		default:
			return 0, fmt.Errorf("adb shell frame type %d", h[0])
		}
	}
	return 0, io.EOF
}

func (s *BinaryShell) frame(id byte, p []byte) error {
	var h [5]byte
	h[0] = id
	binary.LittleEndian.PutUint32(h[1:], uint32(len(p)))
	// A long upload can stay silent until extraction ends. Each write renews the read deadline too.
	s.conn.SetDeadline(time.Now().Add(90 * time.Second))
	b := net.Buffers{h[:], p}
	_, err := b.WriteTo(s.conn)
	return err
}

func (s *BinaryShell) Write(p []byte) (int, error) {
	done := 0
	for done < len(p) {
		n := min(len(p)-done, 32<<10)
		if err := s.frame(0, p[done:done+n]); err != nil {
			return done, err
		}
		done += n
	}
	return done, nil
}

// CloseInput sends EOF while the caller continues to read the command's final status.
func (s *BinaryShell) CloseInput() error { return s.frame(4, nil) }
