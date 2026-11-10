package adb

import (
	"io"
	"net"
	"strconv"
	"time"
)

// adbProbeTimeout bounds the connect and the reply of one adbAnswers try (nothing listening is refused at once).
const adbProbeTimeout = 500 * time.Millisecond

// adbAnswers: an adb server on 127.0.0.1:port or [::1]:port answers host:version with OKAY. What adb clients send
// first anyway; it changes nothing on the server. Only for adb server ports, never an emulator's: a connection there
// is a device connection. A variable for the tests.
var adbAnswers = func(port int) bool {
	for _, h := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(h, strconv.Itoa(port)), adbProbeTimeout)
		if err != nil {
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(adbProbeTimeout))
		reply := make([]byte, 4)
		_, err = io.WriteString(conn, "000chost:version")
		if err == nil {
			_, err = io.ReadFull(conn, reply)
		}
		conn.Close()
		if err == nil && string(reply) == "OKAY" {
			return true
		}
	}
	return false
}
