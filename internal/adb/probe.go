package adb

// probeResult is what adbProbe found on an adb server port.
type probeResult int

const (
	probeSkip  probeResult = iota // no probe on this platform: the system table decides
	probeNone                     // nothing accepts connections there
	probeAdb                      // an adb server answers host:version
	probeOther                    // something accepts but does not answer like adb in time
)
