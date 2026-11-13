package adb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// DefaultServerPort is adb's own default server port, the one other tools use when they run `adb devices`
// (ModAVD/ModAVD#196). Our instances register with the root's private server only; ShareWithDefaultServer also
// registers them here, so that tools which find devices through the default server see them. A variable so that tests
// use a fake server instead: no test may reach the real one.
var DefaultServerPort = 5037

// defaultServerTimeout bounds one look at the default server (connect, request, reply).
const defaultServerTimeout = 3 * time.Second

// ShareWithDefaultServer registers instance console (its adb port is console + 1) with the adb server on
// DefaultServerPort when that server runs and does not list emulator-<console> yet. It never starts that server: the
// tool that starts it gets our instance at the next call (the boot watch calls this every bootWatchShareInterval).
// Returns whether it registered the instance now; nothing listening is not an error.
func ShareWithDefaultServer(ctx context.Context, console int) (bool, error) {
	serials, err := serverDevices(ctx, DefaultServerPort, defaultServerTimeout)
	if err != nil {
		if errors.Is(err, errNoServer) {
			return false, nil
		}
		return false, err
	}
	serial := "emulator-" + strconv.Itoa(console)
	for _, s := range serials {
		if s == serial {
			return false, nil
		}
	}
	if err := Register(ctx, DefaultServerPort, console+1, defaultServerTimeout); err != nil {
		return false, err
	}
	return true, nil
}

// errNoServer: nothing accepts connections on the port.
var errNoServer = errors.New("no adb server")

// serverDevices sends host:devices to the adb server on port and returns the serials it lists (any state).
func serverDevices(ctx context.Context, port int, timeout time.Duration) ([]string, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(dctx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		var op *net.OpError
		if errors.As(err, &op) && op.Op == "dial" && dctx.Err() == nil {
			return nil, errNoServer // refused: nothing listens
		}
		return nil, fmt.Errorf("连不上 %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	const msg = "host:devices"
	if _, err := fmt.Fprintf(conn, "%04x%s", len(msg), msg); err != nil {
		return nil, fmt.Errorf("向 %s 发送 %s 失败: %w", addr, msg, err)
	}
	status := make([]byte, 4)
	if _, err := io.ReadFull(conn, status); err != nil {
		return nil, fmt.Errorf("%s 没有回 %s: %w", addr, msg, err)
	}
	body, err := readHexPrefixed(conn)
	if err != nil {
		return nil, fmt.Errorf("%s 回的 %s 读不全: %w", addr, msg, err)
	}
	if string(status) != "OKAY" {
		return nil, fmt.Errorf("%s 拒绝了 %s: %s %s", addr, msg, status, body)
	}
	var serials []string
	for _, line := range strings.Split(body, "\n") {
		if serial, _, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && serial != "" {
			serials = append(serials, serial)
		}
	}
	return serials, nil
}

// readHexPrefixed reads one adb host-protocol string: 4 hex digits of length, then that many bytes.
func readHexPrefixed(r io.Reader) (string, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(head), 16, 16)
	if err != nil {
		return "", fmt.Errorf("长度 %q 不是十六进制", head)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return "", err
	}
	return string(body), nil
}
