//go:build darwin && cgo

package macos

/*
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/socketvar.h>
#include <sys/sysctl.h>
#include <netinet/in.h>
#include <netinet/in_pcb.h>
#include <netinet/tcp.h>
#include <netinet/tcp_var.h>
#include <netinet/tcp_fsm.h>
#include <libproc.h>
#include <sys/proc_info.h>
#include <errno.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

struct modavd_tcp_listener {
    uint64_t socket;
    uint64_t generation;
    int port;
};

// Read the global table before looking up owners: libproc can deny access to a
// process, but its sockets must still prevent us from reporting a port as free.
static int modavd_tcp_table_once(struct modavd_tcp_listener **out, size_t *count) {
    size_t size = 0;
    if (sysctlbyname("net.inet.tcp.pcblist64", NULL, &size, NULL, 0) != 0)
        return errno;
    if (size < 2 * sizeof(struct xinpgen) || size > 128 * 1024 * 1024)
        return EPROTO;
    void *buffer = malloc(size);
    if (!buffer) return ENOMEM;
    if (sysctlbyname("net.inet.tcp.pcblist64", buffer, &size, NULL, 0) != 0) {
        int error = errno;
        free(buffer);
        return error;
    }
    if (size < 2 * sizeof(struct xinpgen)) {
        free(buffer);
        return EPROTO;
    }
    struct xinpgen first;
    memcpy(&first, buffer, sizeof(first));
    if (first.xig_len != sizeof(first)) {
        free(buffer);
        return EPROTO;
    }
    size_t capacity = (size - 2 * sizeof(first)) / sizeof(struct xtcpcb64);
    struct modavd_tcp_listener *rows = calloc(capacity ? capacity : 1, sizeof(*rows));
    if (!rows) {
        free(buffer);
        return ENOMEM;
    }
    size_t used = 0, offset = sizeof(first);
    int error = EPROTO;
    while (offset <= size - sizeof(uint32_t)) {
        uint32_t length;
        memcpy(&length, (char *)buffer + offset, sizeof(length));
        if (length == sizeof(struct xinpgen)) {
            if (length != size - offset) break;
            struct xinpgen last;
            memcpy(&last, (char *)buffer + offset, sizeof(last));
            error = first.xig_gen == last.xig_gen && first.xig_count == last.xig_count ? 0 : EAGAIN;
            break;
        }
        // These public SDK structures describe pcblist64, not the private _n ABI.
        // Reject a different ABI or a short table instead of silently skipping it.
        if (length != sizeof(struct xtcpcb64) || length > size - offset)
            break;
        struct xtcpcb64 tcp;
        memcpy(&tcp, (char *)buffer + offset, sizeof(tcp));
        offset += length;
        if (tcp.xt_inpcb.inp_gencnt > first.xig_gen) {
            error = EAGAIN;
            break;
        }
        if (tcp.t_state != TCPS_LISTEN) continue;
        struct xsocket64 *socket = &tcp.xt_inpcb.xi_socket;
        if (socket->xso_protocol != IPPROTO_TCP || socket->so_type != SOCK_STREAM ||
            (socket->xso_family != AF_INET && socket->xso_family != AF_INET6) ||
            tcp.xt_inpcb.inp_lport == 0 || used == capacity)
            break;
        rows[used++] = (struct modavd_tcp_listener){
            socket->xso_so, tcp.xt_inpcb.inp_gencnt, ntohs(tcp.xt_inpcb.inp_lport)
        };
    }
    free(buffer);
    if (error) {
        free(rows);
        return error;
    }
    *out = rows;
    *count = used;
    return 0;
}

static int modavd_tcp_table(struct modavd_tcp_listener **out, size_t *count) {
    *out = NULL;
    *count = 0;
    int error = EAGAIN;
    for (int attempt = 0; attempt < 4; attempt++) {
        error = modavd_tcp_table_once(out, count);
        if (error != EAGAIN && error != ENOMEM) return error;
    }
    return error;
}

// This is only an owner lookup. Missing or unreadable descriptors never remove
// entries from the independently collected global occupancy table.
static int modavd_tcp_process(int pid, struct modavd_tcp_listener **out, size_t *count) {
    *out = NULL;
    *count = 0;
    for (int attempt = 0; attempt < 4; attempt++) {
        errno = 0;
        int size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, NULL, 0);
        if (size <= 0) return size == 0 && errno == 0 ? 0 : (errno ? errno : EIO);
        if (size % sizeof(struct proc_fdinfo) != 0 || size > 64 * 1024 * 1024)
            return EPROTO;
        size_t capacity = size / sizeof(struct proc_fdinfo) + 16;
        size_t bytes = capacity * sizeof(struct proc_fdinfo);
        struct proc_fdinfo *fds = malloc(bytes);
        if (!fds) return ENOMEM;
        errno = 0;
        size = proc_pidinfo(pid, PROC_PIDLISTFDS, 0, fds, (int)bytes);
        int error = errno;
        if (size <= 0 || size % sizeof(*fds) != 0) {
            free(fds);
            return error ? error : EIO;
        }
        if ((size_t)size >= bytes) {
            free(fds);
            continue;
        }
        size_t n = size / sizeof(*fds);
        struct modavd_tcp_listener *rows = calloc(n ? n : 1, sizeof(*rows));
        if (!rows) {
            free(fds);
            return ENOMEM;
        }
        size_t used = 0;
        for (size_t i = 0; i < n; i++) {
            if (fds[i].proc_fdtype != PROX_FDTYPE_SOCKET) continue;
            struct socket_fdinfo info;
            if (proc_pidfdinfo(pid, fds[i].proc_fd, PROC_PIDFDSOCKETINFO, &info, sizeof(info)) != sizeof(info))
                continue;
            if (info.psi.soi_kind != SOCKINFO_TCP || info.psi.soi_type != SOCK_STREAM ||
                info.psi.soi_protocol != IPPROTO_TCP ||
                (info.psi.soi_family != AF_INET && info.psi.soi_family != AF_INET6) ||
                info.psi.soi_proto.pri_tcp.tcpsi_state != TSI_S_LISTEN)
                continue;
            struct in_sockinfo *in = &info.psi.soi_proto.pri_tcp.tcpsi_ini;
            rows[used++] = (struct modavd_tcp_listener){
                info.psi.soi_so, in->insi_gencnt, ntohs((uint16_t)in->insi_lport)
            };
        }
        free(fds);
        *out = rows;
        *count = used;
        return 0;
    }
    return EAGAIN;
}
*/
import "C"

import (
	"fmt"
	"sort"
	"syscall"
	"unsafe"

	"modavd/internal/errs"
)

type darwinTCPListener struct {
	socket, generation uint64
	port               int
}

func darwinTCPTable() ([]darwinTCPListener, error) {
	var rows *C.struct_modavd_tcp_listener
	var count C.size_t
	if code := C.modavd_tcp_table(&rows, &count); code != 0 {
		err := syscall.Errno(code)
		return nil, errs.Wrap(errs.Internal, err, fmt.Sprintf("读不到完整的系统 TCP 监听表: %v", err))
	}
	defer C.free(unsafe.Pointer(rows))
	result := make([]darwinTCPListener, 0, int(count))
	for _, row := range unsafe.Slice(rows, int(count)) {
		result = append(result, darwinTCPListener{uint64(row.socket), uint64(row.generation), int(row.port)})
	}
	return result, nil
}

func darwinTCPOwners(rows []darwinTCPListener) (map[darwinTCPListener]map[uint32]bool, error) {
	owners := make(map[darwinTCPListener]map[uint32]bool, len(rows))
	for _, row := range rows {
		owners[row] = map[uint32]bool{}
	}
	if len(rows) == 0 {
		return owners, nil
	}
	processes, err := ListProcesses()
	if err != nil {
		return nil, err
	}
	for _, process := range processes {
		if _, err := process.checkedInfo(); err != nil {
			continue
		}
		var found *C.struct_modavd_tcp_listener
		var count C.size_t
		if code := C.modavd_tcp_process(C.int(process.PID), &found, &count); code != 0 {
			continue
		}
		// A PID may have exited or been reused while its descriptors were read.
		if _, err := process.checkedInfo(); err == nil {
			for _, row := range unsafe.Slice(found, int(count)) {
				key := darwinTCPListener{uint64(row.socket), uint64(row.generation), int(row.port)}
				if key.socket != 0 && key.generation != 0 && owners[key] != nil {
					owners[key][process.PID] = true
				}
			}
		}
		C.free(unsafe.Pointer(found))
	}
	return owners, nil
}

// Listeners reads every IPv4/IPv6 TCP LISTEN socket without connecting. PID is
// zero when its owner cannot be read; that row still represents an occupied port.
// A shared socket may have more than one owner and therefore more than one row.
func Listeners() ([]Listener, error) {
	rows, err := darwinTCPTable()
	if err != nil {
		return nil, err
	}
	owners, err := darwinTCPOwners(rows)
	if err != nil {
		return nil, err
	}
	var result []Listener
	for _, row := range rows {
		if len(owners[row]) == 0 {
			result = append(result, Listener{Port: row.port})
			continue
		}
		for pid := range owners[row] {
			result = append(result, Listener{Port: row.port, PID: pid})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Port != result[j].Port {
			return result[i].Port < result[j].Port
		}
		return result[i].PID < result[j].PID
	})
	return result, nil
}

// ListenerPID requires one known PID across all sockets listening on port.
// Incomplete ownership is an error: the ADB caller must not adopt an unknown
// service merely because a TCP socket exists. ok=false with no error means free.
func ListenerPID(port int) (pid uint32, ok bool, err error) {
	rows, err := darwinTCPTable()
	if err != nil {
		return 0, false, err
	}
	wanted := rows[:0]
	for _, row := range rows {
		if row.port == port {
			wanted = append(wanted, row)
		}
	}
	if len(wanted) == 0 {
		return 0, false, nil
	}
	owners, err := darwinTCPOwners(wanted)
	if err != nil {
		return 0, true, err
	}
	for _, row := range wanted {
		if len(owners[row]) != 1 {
			return 0, true, errs.Newf(errs.Internal, "TCP 端口 %d 已被占用，但无法确认唯一的所属进程", port)
		}
		for owner := range owners[row] {
			if owner == 0 || (pid != 0 && pid != owner) {
				return 0, true, errs.Newf(errs.Internal, "TCP 端口 %d 由多个进程监听，无法确认唯一的所属进程", port)
			}
			pid = owner
		}
	}
	return pid, true, nil
}

// BusyPorts returns the sorted, unique occupied TCP ports. Owner permissions do
// not affect occupancy; an incomplete global table returns an error, never free.
func BusyPorts(ports []int) ([]int, error) {
	rows, err := darwinTCPTable()
	if err != nil {
		return nil, err
	}
	wanted := make(map[int]bool, len(ports))
	for _, port := range ports {
		wanted[port] = true
	}
	busy := map[int]bool{}
	for _, row := range rows {
		if wanted[row.port] {
			busy[row.port] = true
		}
	}
	result := make([]int, 0, len(busy))
	for port := range busy {
		result = append(result, port)
	}
	sort.Ints(result)
	return result, nil
}
