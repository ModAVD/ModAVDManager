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

// tableShows makes the system listener table report port as listened on by pid (ok) or nothing at all, with state
// as the odd state of every port when not 0.
func tableShows(t *testing.T, pid uint32, ok bool, state uint32) {
	t.Helper()
	savedPID, savedState := listenerPID, oddListenerState
	t.Cleanup(func() { listenerPID, oddListenerState = savedPID, savedState })
	listenerPID = func(int) (uint32, bool, error) { return pid, ok, nil }
	oddListenerState = func(int) (uint32, bool) { return state, state != 0 }
}

// The table shows nothing on the port: an adb server answering there counts as listening (said once); nothing there,
// or something that does not answer like adb, does not.
func TestListenerTableMissAdbAnswers(t *testing.T) {
	tableShows(t, 0, false, 0)
	c, logs := hookClient(t)
	adbPort := fakeServer(t, "OKAY00040029")
	for range 2 {
		if up, err := c.serverListening(privateServer, adbPort); !up || err != nil {
			t.Fatalf("adb server missing from the table: %v %v", up, err)
		}
	}
	if n := logs.count("系统的 TCP 监听表里看不到端口 " + strconv.Itoa(adbPort) + "，但那里有 adb server 应答 host:version，按在监听处理"); n != 1 {
		t.Fatalf("%d notes: %q", n, logs.msgs)
	}

	other := fakeServer(t, "HTTP/1.1 400 Bad Request\r\n")
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	for _, port := range []int{other, closed} {
		if up, err := c.serverListening(privateServer, port); up || err != nil {
			t.Errorf("port %d: %v %v", port, up, err)
		}
	}
	if logs.count("看不到端口") != 1 {
		t.Errorf("notes: %q", logs.msgs)
	}
}

// A table row in a state other than LISTEN counts as listening, said once.
func TestListenerOddState(t *testing.T) {
	tableShows(t, uint32(os.Getpid()), true, 5)
	c, logs := hookClient(t)
	for range 2 {
		if up, err := c.listening(20391); !up || err != nil {
			t.Fatalf("%v %v", up, err)
		}
	}
	if n := logs.count("系统的 TCP 监听表里端口 20391 的状态值是 5（不是 LISTEN 的 2），照样算在监听"); n != 1 {
		t.Fatalf("%d notes: %q", n, logs.msgs)
	}
}

// The user's case: the private server runs and answers, the table never shows it. EnsureServer takes it as up and
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
