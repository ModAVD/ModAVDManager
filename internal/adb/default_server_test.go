package adb

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// fakeDefaultServer is a stand-in for the default adb server on a free port of 127.0.0.1: it answers host:devices with
// devices and records every other request. DefaultServerPort points at it for the test.
func fakeDefaultServer(t *testing.T, devices string) <-chan string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	old := DefaultServerPort
	DefaultServerPort = ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { DefaultServerPort = old })
	if DefaultServerPort == 5037 {
		t.Fatal("测试端口不能是 5037")
	}
	got := make(chan string, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				msg, err := readHexPrefixed(bufio.NewReader(c))
				if err != nil {
					return
				}
				if msg == "host:devices" {
					fmt.Fprintf(c, "OKAY%04x%s", len(devices), devices)
					return
				}
				got <- msg
				io.Copy(io.Discard, c)
			}(conn)
		}
	}()
	return got
}

func TestShareWithDefaultServerRegistersMissingInstance(t *testing.T) {
	got := fakeDefaultServer(t, "emulator-5554\tdevice\n127.0.0.1:16384\toffline\n")
	registered, err := ShareWithDefaultServer(context.Background(), 31020)
	if err != nil || !registered {
		t.Fatalf("ShareWithDefaultServer = %v, %v，应登记", registered, err)
	}
	select {
	case msg := <-got:
		if msg != "host:emulator:31021" {
			t.Fatalf("登记发的是 %q，应是 host:emulator:31021（adb 端口 = 控制台 + 1）", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("假 server 没收到登记")
	}
}

func TestShareWithDefaultServerSkipsKnownInstance(t *testing.T) {
	for _, state := range []string{"device", "offline", "unauthorized"} {
		got := fakeDefaultServer(t, "emulator-31020\t"+state+"\n")
		registered, err := ShareWithDefaultServer(context.Background(), 31020)
		if err != nil || registered {
			t.Fatalf("%s: ShareWithDefaultServer = %v, %v，已经列着的不该再登记", state, registered, err)
		}
		select {
		case msg := <-got:
			t.Fatalf("%s: 不该再发 %q", state, msg)
		case <-time.After(300 * time.Millisecond):
		}
	}
}

func TestShareWithDefaultServerWithoutServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens there now
	old := DefaultServerPort
	DefaultServerPort = port
	t.Cleanup(func() { DefaultServerPort = old })
	registered, err := ShareWithDefaultServer(context.Background(), 31020)
	if err != nil || registered {
		t.Fatalf("没有 server 时 ShareWithDefaultServer = %v, %v，应什么都不做、不报错", registered, err)
	}
}

func TestServerDevicesRefusal(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		readHexPrefixed(bufio.NewReader(c))
		const why = "unknown host service"
		fmt.Fprintf(c, "FAIL%04x%s", len(why), why)
	}()
	port := ln.Addr().(*net.TCPAddr).Port
	if _, err := serverDevices(context.Background(), port, 2*time.Second); err == nil {
		t.Fatal("FAIL 回复应报错")
	}
}
