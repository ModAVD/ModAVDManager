package report

// The adb host protocol (adb's SERVICES.TXT / protocol.txt), spoken directly to a server that already listens. Never
// adb.exe: a client of another version kills the server and starts its own, and a client that finds no server starts
// one; neither may happen here. Never port 5037.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

var (
	// errDefaultServer: port 5037 belongs to whatever adb the user runs; the scripts never touch it, nor does modavd.
	errDefaultServer = errors.New("不连 5037 端口的 adb server")
	// errAdbClosed is report-config.ps1's message for a connection that ended early.
	errAdbClosed = errors.New("adb server 断开")
)

const (
	adbDialTimeout = 5 * time.Second
	// adbReadTimeout is report-config.ps1's TcpClient ReceiveTimeout: one read waits at most this long.
	adbReadTimeout = 15 * time.Second
)

// adbConn is one connection to the server (one request, as adb does it).
type adbConn struct {
	c    net.Conn
	stop func() bool
}

// dialTCP opens the connection (tests check which ports are dialed).
var dialTCP = func(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: adbDialTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

// dialAdb connects to the server on 127.0.0.1:port; ctx ending closes the connection.
func dialAdb(ctx context.Context, port int) (*adbConn, error) {
	if port == 5037 {
		return nil, errDefaultServer
	}
	c, err := dialTCP(ctx, "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return nil, err
	}
	return &adbConn{c: c, stop: context.AfterFunc(ctx, func() { c.Close() })}, nil
}

func (a *adbConn) Close() {
	a.stop()
	a.c.Close()
}

// send writes one request: four hex digits of its UTF-8 length, then the text.
func (a *adbConn) send(msg string) error {
	_ = a.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := io.WriteString(a.c, fmt.Sprintf("%04x", len(msg))+msg)
	return err
}

// Read reads with report-config.ps1's per-read timeout.
func (a *adbConn) Read(p []byte) (int, error) {
	_ = a.c.SetReadDeadline(time.Now().Add(adbReadTimeout))
	return a.c.Read(p)
}

// readExact is report-config.ps1's AdbReadExact.
func (a *adbConn) readExact(n int) ([]byte, error) {
	b := make([]byte, n)
	if _, err := io.ReadFull(a, b); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, errAdbClosed
		}
		return nil, err
	}
	return b, nil
}

// readHex reads a length-prefixed reply (four hex digits, then that many bytes).
func (a *adbConn) readHex() ([]byte, error) {
	h, err := a.readExact(4)
	if err != nil {
		return nil, err
	}
	n, err := strconv.ParseUint(string(h), 16, 16)
	if err != nil {
		return nil, fmt.Errorf("adb server 返回的长度不对：%q", h)
	}
	if n == 0 {
		return nil, nil
	}
	return a.readExact(int(n))
}

// adbFail is the server refusing a request (FAIL and its message).
type adbFail struct{ msg string }

func (e *adbFail) Error() string { return "adb server 返回 FAIL：" + e.msg }

// status is report-config.ps1's AdbStatus: OKAY, or FAIL with the server's message.
func (a *adbConn) status() error {
	st, err := a.readExact(4)
	if err != nil {
		return err
	}
	switch string(st) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, err := a.readHex()
		if err != nil {
			return err
		}
		return &adbFail{msg: string(msg)}
	}
	return errors.New("adb server 返回意外状态：" + string(st))
}

// request sends msg and checks the status.
func (a *adbConn) request(msg string) error {
	if err := a.send(msg); err != nil {
		return err
	}
	return a.status()
}

// adbDevices is report-config.ps1's AdbDevices: host:devices, the server's text.
func adbDevices(ctx context.Context, port int) (string, error) {
	a, err := dialAdb(ctx, port)
	if err != nil {
		return "", err
	}
	defer a.Close()
	if err := a.request("host:devices"); err != nil {
		return "", err
	}
	b, err := a.readHex()
	if err != nil {
		return "", err
	}
	return decodeText(b), nil
}

// adbShell is report-config.ps1's AdbShell: host:transport:<serial>, then shell:<cmd> (the old shell protocol, stderr
// merged into the output), read to the end, decoded as UTF-8 and trimmed.
func adbShell(ctx context.Context, port int, serial, cmd string) (string, error) {
	a, err := dialAdb(ctx, port)
	if err != nil {
		return "", err
	}
	defer a.Close()
	if err := a.request("host:transport:" + serial); err != nil {
		return "", err
	}
	if err := a.request("shell:" + cmd); err != nil {
		return "", err
	}
	b, err := io.ReadAll(a)
	if err != nil {
		return "", err
	}
	return netTrim(decodeText(b)), nil
}

// adbResult is what collect-info.ps1's Invoke-Native reports of one adb.exe run.
type adbResult struct {
	TimedOut bool
	ExitCode int
	Stdout   []byte
	Stderr   string
}

// adbLive runs one of the live commands collect-info.ps1 runs with adb.exe, the way adb.exe does it, within timeout:
//
//	exec-out <cmd>      exec:<cmd> (raw bytes, exit code 0)
//	shell <cmd>         shell,v2,raw:<cmd> (stdout, stderr and the exit code apart), shell:<cmd> without shell_v2
//	logcat <args>       shell,v2:export ANDROID_LOG_TAGS=''; exec logcat '<arg>'... (adb's logcat())
//
// Stdout goes to w when it is set (the screenshot), else into the result. A refused request is exit code 1 with the
// server's message as stderr, as adb.exe prints it; a server that went away is exit code 1 too (adb.exe would start
// a new one).
func adbLive(ctx context.Context, port int, serial string, timeout time.Duration, w io.Writer, kind string, args ...string) adbResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out bytes.Buffer
	if w == nil {
		w = &out
	}
	res, err := adbLiveRun(ctx, port, serial, w, kind, args)
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	} else if err != nil {
		res.ExitCode = 1
		var f *adbFail
		if errors.As(err, &f) {
			res.Stderr = "adb: " + f.msg + "\n"
		} else {
			res.Stderr = "adb: " + err.Error() + "\n"
		}
	}
	res.Stdout = out.Bytes()
	return res
}

func adbLiveRun(ctx context.Context, port int, serial string, w io.Writer, kind string, args []string) (adbResult, error) {
	var res adbResult
	service := ""
	v2 := false
	switch kind {
	case "exec-out":
		service = "exec:" + strings.Join(args, " ")
	case "shell", "logcat":
		features, err := adbFeatures(ctx, port, serial)
		if err != nil {
			return res, err
		}
		v2 = hasFeature(features, "shell_v2")
		cmd := strings.Join(args, " ")
		typ := ",raw"
		if kind == "logcat" {
			cmd = `export ANDROID_LOG_TAGS=''; exec logcat`
			for _, a := range args {
				cmd += " " + shellQuote(a)
			}
			typ = ""
		}
		if v2 {
			service = "shell,v2" + typ + ":" + cmd
		} else {
			service = "shell:" + cmd
		}
	default:
		return res, fmt.Errorf("不认识的 adb 命令 %s", kind)
	}
	a, err := dialAdb(ctx, port)
	if err != nil {
		return res, err
	}
	defer a.Close()
	if err := a.request("host:transport:" + serial); err != nil {
		return res, err
	}
	if err := a.request(service); err != nil {
		return res, err
	}
	if !v2 {
		_, err := io.Copy(w, a)
		return res, err
	}
	var stderr bytes.Buffer
	for {
		hdr, err := a.readExact(5)
		if err != nil {
			if errors.Is(err, errAdbClosed) {
				// the stream ended without an exit packet: adb.exe reports what it got
				res.Stderr = stderr.String()
				return res, nil
			}
			return res, err
		}
		n := binary.LittleEndian.Uint32(hdr[1:])
		if n > 64<<20 {
			return res, fmt.Errorf("shell 协议的数据包太大（%d 字节）", n)
		}
		var data []byte
		if n > 0 {
			if data, err = a.readExact(int(n)); err != nil {
				return res, err
			}
		}
		switch hdr[0] {
		case 1: // stdout
			if _, err := w.Write(data); err != nil {
				return res, err
			}
		case 2: // stderr
			stderr.Write(data)
		case 3: // exit
			if len(data) > 0 {
				res.ExitCode = int(data[0])
			}
			res.Stderr = stderr.String()
			return res, nil
		}
	}
}

// adbFeatures is host-serial:<serial>:features (what adb.exe asks before a shell).
func adbFeatures(ctx context.Context, port int, serial string) (string, error) {
	a, err := dialAdb(ctx, port)
	if err != nil {
		return "", err
	}
	defer a.Close()
	if err := a.request("host-serial:" + serial + ":features"); err != nil {
		return "", err
	}
	b, err := a.readHex()
	return string(b), err
}

func hasFeature(list, name string) bool {
	for _, f := range strings.Split(list, ",") {
		if strings.TrimSpace(f) == name {
			return true
		}
	}
	return false
}

// shellQuote is adb's escape_arg: the argument in single quotes, each single quote in it closed, escaped and reopened.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
