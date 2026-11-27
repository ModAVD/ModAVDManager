package adb

import (
	"io"
	"net"
	"strconv"
	"time"
)

// adbProbeTimeout bounds the connect and the reply of one adbProbe try (nothing listening is refused at once).
const adbProbeTimeout = 500 * time.Millisecond

// adbProbe asks 127.0.0.1:port, then [::1]:port: an adb server answers host:version with OKAY (probeAdb); nothing
// accepting connections is probeNone; something that accepts but does not answer like adb in time (another program,
// or an adb server still starting or stuck) is probeOther. host:version is what adb clients send first anyway and
// changes nothing on the server. Only for adb server ports, never an emulator's: a connection there is a device
// connection. A variable for the tests.
var adbProbe = func(port int) probeResult {
	accepted := false
	for _, h := range []string{"127.0.0.1", "::1"} {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(h, strconv.Itoa(port)), adbProbeTimeout)
		if err != nil {
			continue
		}
		accepted = true
		_ = conn.SetDeadline(time.Now().Add(adbProbeTimeout))
		reply := make([]byte, 4)
		_, err = io.WriteString(conn, "000chost:version")
		if err == nil {
			_, err = io.ReadFull(conn, reply)
		}
		conn.Close()
		if err == nil && string(reply) == "OKAY" {
			return probeAdb
		}
	}
	if accepted {
		return probeOther
	}
	return probeNone
}
