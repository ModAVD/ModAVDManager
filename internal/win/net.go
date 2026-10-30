//go:build windows

package win

import (
	"encoding/binary"
	"fmt"
	"sort"
	"syscall"
	"unsafe"

	"modavd/internal/errs"
)

const (
	afInet                   = 2
	afInet6                  = 23
	tcpTableOwnerPIDListener = 3 // TCP_TABLE_CLASS TCP_TABLE_OWNER_PID_LISTENER
	mibTCPStateListen        = 2
	errorInsufficientBuffer  = 122
	errorNotSupported        = 50

	// MIB_TCPROW_OWNER_PID: dwState, dwLocalAddr, dwLocalPort, dwRemoteAddr, dwRemotePort, dwOwningPid.
	tcp4RowSize, tcp4State, tcp4Port, tcp4PID = 24, 0, 8, 20
	// MIB_TCP6ROW_OWNER_PID: ucLocalAddr[16], dwLocalScopeId, dwLocalPort, ucRemoteAddr[16], dwRemoteScopeId,
	// dwRemotePort, dwState, dwOwningPid.
	tcp6RowSize, tcp6State, tcp6Port, tcp6PID = 56, 48, 20, 52
)

// tcpTable is the raw GetExtendedTcpTable result for one address family (the table can grow between the size query
// and the read, hence the loop).
func tcpTable(af uintptr) ([]byte, error) {
	if err := procGetExtendedTcpTable.Find(); err != nil {
		return nil, err
	}
	size := uint32(16 << 10)
	for range 8 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0,
			af, tcpTableOwnerPIDListener, 0)
		switch r {
		case 0:
			return buf, nil
		case errorInsufficientBuffer:
			size += 4 << 10
		default:
			return nil, syscall.Errno(r)
		}
	}
	return nil, syscall.Errno(errorInsufficientBuffer)
}

// parseTCPTable reads the listening rows of a MIB_TCPTABLE_OWNER_PID / MIB_TCP6TABLE_OWNER_PID (dwNumEntries, then
// the rows from offset 4). The port is in network byte order in the low 16 bits of dwLocalPort.
func parseTCPTable(buf []byte, rowSize, stateOff, portOff, pidOff int) []Listener {
	if len(buf) < 4 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(buf))
	var out []Listener
	for i := 0; i < n; i++ {
		row := 4 + i*rowSize
		if row+rowSize > len(buf) {
			break
		}
		if binary.LittleEndian.Uint32(buf[row+stateOff:]) != mibTCPStateListen {
			continue
		}
		v := binary.LittleEndian.Uint32(buf[row+portOff:])
		out = append(out, Listener{
			Port: int(v&0xff)<<8 | int(v>>8&0xff),
			PID:  binary.LittleEndian.Uint32(buf[row+pidOff:]),
		})
	}
	return out
}

// Listeners is every LISTEN socket, TCP over IPv4 and IPv6 (GetExtendedTcpTable TCP_TABLE_OWNER_PID_LISTENER), like
// psutil.net_connections(kind="tcp") filtered to LISTEN. Reads the table only; never connects.
func Listeners() ([]Listener, error) {
	buf, err := tcpTable(afInet)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("读不到系统的 TCP 监听表: %v", err))
	}
	out := parseTCPTable(buf, tcp4RowSize, tcp4State, tcp4Port, tcp4PID)
	buf, err = tcpTable(afInet6)
	if err != nil {
		// A host without IPv6 has no table for it; anything else is a real failure.
		if err == syscall.Errno(errorNotSupported) || err == errorInvalidParameter {
			return out, nil
		}
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("读不到系统的 TCP（IPv6）监听表: %v", err))
	}
	return append(out, parseTCPTable(buf, tcp6RowSize, tcp6State, tcp6Port, tcp6PID)...), nil
}

// ListenerPID: who listens on port. ok=false when nobody does.
func ListenerPID(port int) (pid uint32, ok bool, err error) {
	ls, err := Listeners()
	if err != nil {
		return 0, false, err
	}
	for _, l := range ls {
		if l.Port == port {
			return l.PID, true, nil
		}
	}
	return 0, false, nil
}

// BusyPorts returns the sorted subset of ports somebody listens on.
func BusyPorts(ports []int) ([]int, error) {
	ls, err := Listeners()
	if err != nil {
		return nil, err
	}
	wanted := map[int]bool{}
	for _, p := range ports {
		wanted[p] = true
	}
	busy := map[int]bool{}
	for _, l := range ls {
		if wanted[l.Port] {
			busy[l.Port] = true
		}
	}
	out := make([]int, 0, len(busy))
	for p := range busy {
		out = append(out, p)
	}
	sort.Ints(out)
	return out, nil
}
