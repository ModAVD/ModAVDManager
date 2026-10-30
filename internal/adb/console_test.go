//go:build windows

package adb

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
)

// setToken points USERPROFILE at a fresh directory holding content as the console token file ("" = no file).
func setToken(t *testing.T, content string) {
	t.Helper()
	dir := tempDir(t)
	if content != "" {
		if err := os.WriteFile(filepath.Join(dir, consoleTokenFile), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("USERPROFILE", dir)
}

// fakeConsole serves script on the first connection to a loopback listener and returns its port. Lines the script
// expects are checked with expect.
func fakeConsole(t *testing.T, script func(c *consoleConn)) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		script(&consoleConn{t: t, conn: conn, r: bufio.NewReader(conn)})
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	return ln.Addr().(*net.TCPAddr).Port
}

type consoleConn struct {
	t    *testing.T
	conn net.Conn
	r    *bufio.Reader
}

func (c *consoleConn) write(s string) { c.conn.Write([]byte(s)) }

func (c *consoleConn) expect(line string) bool {
	got, err := c.r.ReadString('\n')
	if got != line+"\r\n" {
		c.t.Errorf("console got %q (%v), want %q", got, err, line+"\r\n")
		return false
	}
	return true
}

// drain blocks until the client closes the connection.
func (c *consoleConn) drain() {
	for {
		if _, err := c.r.ReadString('\n'); err != nil {
			return
		}
	}
}

const (
	authBanner = "Android Console: Authentication required\r\n" +
		"Android Console: type 'auth <auth_token>' to authenticate\r\n" +
		"Android Console: you can find your <auth_token> in \r\n" +
		"'C:\\Users\\<user>\\.emulator_console_auth_token'\r\nOK\r\n"
	plainBanner = "Android Console: type 'help' for a list of commands\r\nOK\r\n"
)

func TestConsoleToken(t *testing.T) {
	setToken(t, "  tok-123 \r\n")
	if got := ConsoleToken(); got != "tok-123" {
		t.Errorf("ConsoleToken = %q", got)
	}
	setToken(t, "")
	if got := ConsoleToken(); got != "" {
		t.Errorf("ConsoleToken without file = %q", got)
	}
}

func TestConsoleAuthAndCommand(t *testing.T) {
	setToken(t, "  tok-123\r\n")
	port := fakeConsole(t, func(c *consoleConn) {
		c.write(authBanner)
		if !c.expect("auth tok-123") {
			return
		}
		c.write(plainBanner)
		if !c.expect("screenrecord webrtc start") {
			return
		}
		c.write("line one\r\nline two\r\nOK\r\n")
		c.expect("quit")
	})
	out, err := Console(context.Background(), port, "screenrecord webrtc start", 5*time.Second)
	if err != nil || out != "line one\nline two" {
		t.Errorf("Console = %q, %v", out, err)
	}
}

func TestConsoleNoToken(t *testing.T) {
	setToken(t, "")
	port := fakeConsole(t, func(c *consoleConn) {
		c.write(plainBanner)
		if !c.expect("kill") { // no auth line without a token
			return
		}
		c.write("OK: killing emulator, bye bye\r\nOK\r\n")
		c.expect("quit")
	})
	out, err := Console(context.Background(), port, "kill", 5*time.Second)
	if err != nil || out != "OK: killing emulator, bye bye" {
		t.Errorf("Console = %q, %v", out, err)
	}
}

func TestConsoleKO(t *testing.T) {
	setToken(t, "")
	port := fakeConsole(t, func(c *consoleConn) {
		c.write(plainBanner)
		c.expect("bogus")
		c.write("some output\r\nKO: unknown command, try 'help'\r\n")
		c.drain()
	})
	_, err := Console(context.Background(), port, "bogus", 5*time.Second)
	want := "控制台命令 'bogus' 失败: some output\nKO: unknown command, try 'help'"
	if errs.CodeOf(err) != errs.Internal || err.Error() != want {
		t.Errorf("Console error = %v (%s), want %q", err, errs.CodeOf(err), want)
	}
}

func TestConsoleAuthFailed(t *testing.T) {
	setToken(t, "wrong")
	port := fakeConsole(t, func(c *consoleConn) {
		c.write(authBanner)
		c.expect("auth wrong")
		c.write("KO: authentication token does not match ~/.emulator_console_auth_token\r\n")
		c.drain()
	})
	_, err := Console(context.Background(), port, "kill", 5*time.Second)
	want := fmt.Sprintf("控制台 %d 鉴权失败: KO: authentication token does not match ~/.emulator_console_auth_token", port)
	if errs.CodeOf(err) != errs.Internal || err == nil || err.Error() != want {
		t.Errorf("Console error = %v, want %q", err, want)
	}
}

func TestConsoleClosedDuringBanner(t *testing.T) {
	setToken(t, "")
	port := fakeConsole(t, func(c *consoleConn) {
		c.write("Android Console: partial") // no newline, then the connection ends
	})
	_, err := Console(context.Background(), port, "kill", 5*time.Second)
	want := fmt.Sprintf("控制台 %d 没有就绪: Android Console: partial", port)
	if errs.CodeOf(err) != errs.Internal || err == nil || err.Error() != want {
		t.Errorf("Console error = %v, want %q", err, want)
	}
}

func TestConsoleClosedAfterCommand(t *testing.T) {
	setToken(t, "")
	port := fakeConsole(t, func(c *consoleConn) {
		c.write(plainBanner)
		c.expect("kill")
		c.write("OK: killing emulator, bye bye\r\n") // and gone before the final OK
	})
	_, err := Console(context.Background(), port, "kill", 5*time.Second)
	want := "控制台命令 'kill' 失败: OK: killing emulator, bye bye"
	if errs.CodeOf(err) != errs.Internal || err == nil || err.Error() != want {
		t.Errorf("Console error = %v, want %q", err, want)
	}
}

func TestConsoleTimeout(t *testing.T) {
	setToken(t, "")
	port := fakeConsole(t, func(c *consoleConn) {
		c.write("Android Console: type 'help' for a list of commands\r\n") // never says OK
		c.drain()
	})
	start := time.Now()
	_, err := Console(context.Background(), port, "kill", 300*time.Millisecond)
	elapsed := time.Since(start)
	want := fmt.Sprintf("控制台 %d 命令 'kill' 超时", port)
	if errs.CodeOf(err) != errs.Internal || err == nil || err.Error() != want {
		t.Errorf("Console error = %v, want %q", err, want)
	}
	if elapsed > 3*time.Second {
		t.Errorf("Console took %v with a 300 ms timeout", elapsed)
	}
}

func TestConsoleRefused(t *testing.T) {
	setToken(t, "")
	port := freePort(t)
	_, err := Console(context.Background(), port, "kill", 5*time.Second)
	// Windows retries a refused loopback connect for ~2 s, so either outcome is a refusal here
	if err == nil || errs.CodeOf(err) != errs.Internal ||
		!(strings.HasPrefix(err.Error(), fmt.Sprintf("连不上控制台 %d: ", port)) ||
			err.Error() == fmt.Sprintf("控制台 %d 命令 'kill' 超时", port)) {
		t.Errorf("Console error = %v", err)
	}
}

func TestConsoleCanceled(t *testing.T) {
	setToken(t, "")
	port := fakeConsole(t, func(c *consoleConn) { c.drain() }) // silent
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := Console(ctx, port, "kill", 5*time.Second)
	if errs.CodeOf(err) != errs.Canceled {
		t.Errorf("Console error = %v, want canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancel took %v", elapsed)
	}
}

func TestPyRepr(t *testing.T) {
	for in, want := range map[string]string{
		"kill":      `'kill'`,
		"it's":      `"it's"`,
		`a\b`:       `'a\\b'`,
		`a'b"c`:     `'a\'b"c'`,
		"x\ty\r\n":  `'x\ty\r\n'`,
		"\x01":      `'\x01'`,
		"screen 中文": `'screen 中文'`,
	} {
		if got := pyRepr(in); got != want {
			t.Errorf("pyRepr(%q) = %s, want %s", in, got, want)
		}
	}
}
