package adb

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"modavd/internal/errs"
)

// fakeServer listens on 127.0.0.1 and answers whatever comes with reply; returns its port.
func fakeServer(t *testing.T, reply string) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				buf := make([]byte, 16)
				if _, err := io.ReadFull(conn, buf); err == nil {
					_, _ = io.WriteString(conn, reply)
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// closedPort is a port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

// tableShows makes the system listener table report every port as listened on by pid (ok) or nothing at all, with
// state as the odd state of every port when not 0.
func tableShows(t *testing.T, pid uint32, ok bool, state uint32) {
	t.Helper()
	savedPID, savedState := listenerPID, oddListenerState
	t.Cleanup(func() { listenerPID, oddListenerState = savedPID, savedState })
	listenerPID = func(int) (uint32, bool, error) { return pid, ok, nil }
	oddListenerState = func(int) (uint32, bool) { return state, state != 0 }
}

// An adb server answering on the port listens, whatever the table says (what it says wrong is said once); nothing
// there does not; something there that does not answer like adb, and that the table does not name, is a conflict.
func TestServerListeningAsksThePort(t *testing.T) {
	tableShows(t, 0, false, 0)
	c, logs := hookClient(t)
	adbPort := fakeServer(t, "OKAY00040029")
	for range 2 {
		if up, err := c.serverListening(privateServer, adbPort); !up || err != nil {
			t.Fatalf("adb server missing from the table: %v %v", up, err)
		}
		if up, err := c.listening(adbPort); !up || err != nil {
			t.Fatalf("listening: %v %v", up, err)
		}
	}
	if n := logs.count("端口 " + strconv.Itoa(adbPort) + " 上的 adb server 在应答，系统的 TCP 监听表里却看不到它（这台电脑的监听表不可靠，不影响使用）"); n != 1 {
		t.Fatalf("%d notes: %q", n, logs.msgs)
	}

	if up, err := c.serverListening(privateServer, closedPort(t)); up || err != nil {
		t.Errorf("closed port: %v %v", up, err)
	}
	other := fakeServer(t, "HTTP/1.1 400 Bad Request\r\n")
	if up, err := c.serverListening(privateServer, other); up || errs.CodeOf(err) != errs.PortConflict {
		t.Errorf("not adb: %v %v", up, err)
	}
	if up, err := c.listening(other); up || err != nil {
		t.Errorf("listening on not adb, no table row: %v %v", up, err)
	}
	if logs.count("看不到它") != 1 {
		t.Errorf("notes: %q", logs.msgs)
	}
}

// The table no longer decides: a row on a port where nothing accepts connections is no server.
func TestServerListeningTableAloneIsNotEnough(t *testing.T) {
	tableShows(t, uint32(os.Getpid()), true, 0)
	c, logs := hookClient(t)
	port := closedPort(t)
	if up, err := c.serverListening(privateServer, port); up || err != nil {
		t.Errorf("serverListening: %v %v", up, err)
	}
	if up, err := c.listening(port); up || err != nil {
		t.Errorf("listening: %v %v", up, err)
	}
	if len(logs.msgs) != 0 {
		t.Errorf("notes: %q", logs.msgs)
	}
}

// The table's row for an answering server in a state other than LISTEN is said once.
func TestListenerOddState(t *testing.T) {
	tableShows(t, uint32(os.Getpid()), true, 5)
	c, logs := hookClient(t)
	port := fakeServer(t, "OKAY00040029")
	for range 2 {
		if up, err := c.serverListening(privateServer, port); !up || err != nil {
			t.Fatalf("%v %v", up, err)
		}
	}
	if n := logs.count("系统的 TCP 监听表里端口 " + strconv.Itoa(port) + " 的状态值是 5（不是 LISTEN 的 2），照样算在监听"); n != 1 || logs.count("看不到它") != 0 {
		t.Fatalf("%d notes: %q", n, logs.msgs)
	}
}

// The users' case: the private server runs and answers, the table never shows it. EnsureServer takes it as up and
// starts nothing (it used to start a server every 20 s and run no adb command at all).
func TestEnsureServerTableMiss(t *testing.T) {
	tableShows(t, 0, false, 0)
	c, logs := hookClient(t)
	port := fakeServer(t, "OKAY00040029")
	meta := `{"adbServerPort": ` + strconv.Itoa(port) + `}`
	if err := os.WriteFile(filepath.Join(c.Root.Dir, "mas-avd.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	up, err := c.EnsureServer(context.Background(), 2*time.Second)
	if !up || err != nil {
		t.Fatalf("EnsureServer: %v %v", up, err)
	}
	if logs.count("启动魔改 AVD") != 0 {
		t.Fatalf("started a server: %q", logs.msgs)
	}
}
