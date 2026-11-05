// Package errs is the error type every modavd package returns to the CLI and MCP layers. The Code is part of the
// machine-readable contract (`--json` prints it as error.code, MCP tool errors carry it), the Message is Chinese text
// for the user, the Advice says what to do about it.
package errs

import (
	"errors"
	"fmt"
	"maps"
)

const (
	Usage              = "usage"               // bad command line or tool arguments
	InvalidArgument    = "invalid_argument"    // a value out of range (memory, cpu, fps, index ...)
	NotFound           = "not_found"           // the instance does not exist
	AlreadyExists      = "already_exists"      // the instance already exists
	NotReady           = "not_ready"           // the root is incomplete: components missing, emulator not ours / too old
	PrecheckFailed     = "precheck_failed"     // a blocking check before boot failed (acceleration, memory, disk ...)
	Busy               = "busy"                // the instance is running, or another modavd holds its lock
	PortConflict       = "port_conflict"       // a port we need is taken by some other program
	Timeout            = "timeout"             // boot or shutdown did not finish in time
	LaunchFailed       = "launch_failed"       // emulator.exe exited, or qemu went away while booting
	MetadataUnreadable = "metadata_unreadable" // mas-avd.json exists but cannot be read; never overwritten then
	Unsupported        = "unsupported"         // not possible on this instance / host
	Canceled           = "canceled"            // the caller canceled (Ctrl+C, MCP notifications/cancelled)
	UpdateUnavailable  = "update_unavailable"  // update: the update source cannot be asked now (offline, rate limited, HTTP error)
	UpdateRejected     = "update_rejected"     // update: a download failed its checks (size, hash, contents)
	Internal           = "internal"            // anything else
	NotImplemented     = "not_implemented"
)

// Codes of components fetch.
const (
	LicenseDeclined       = "license_declined"       // the Android SDK license was not accepted; nothing downloaded
	ComponentsUnavailable = "components_unavailable" // no mirror delivered the license or a component (offline, HTTP error, timeout, size)
	ComponentsRejected    = "components_rejected"    // a download failed its checks (SHA-1 on every mirror; files, pins, version unpacked)
)

// Code of the per-app proxy (modavd proxy).
const (
	ProxyUnavailable = "proxy_unavailable" // the proxy does not answer, or not as SOCKS5 / HTTP; none set on the host for auto
)

// Keys of Error.Details.
const (
	BootDiagDir    = "boot_diag_dir"   // where a failed boot's guest state was saved (logs\<instance>-<time>-bootfail)
	Reason         = "reason"          // busy: running, locked, starting, stopping, window_mismatch, port_in_use, port_other_root
	Lock           = "lock"            // the lock category when Reason is locked
	InstanceStatus = "instance_status" // a lifecycle status, not_found, or unknown
	QemuRunning    = "qemu_running"    // true, false, or unknown
	InstanceExists = "instance_exists" // files or metadata currently occupy the index; not ownership by this call
)

type Error struct {
	Code    string
	Message string
	Advice  string
	// Details are machine-readable facts besides the message (BootDiagDir ...): --json's error.details, the manager
	// API's, MCP's error text. Read them with DetailsOf, which also finds those of the errors this one wraps.
	Details map[string]string
	Cause   error
}

func (e *Error) Error() string {
	if e.Advice != "" {
		return e.Message + "。" + e.Advice
	}
	return e.Message
}

func (e *Error) Unwrap() error { return e.Cause }

func New(code, message string) *Error { return &Error{Code: code, Message: message} }

func Newf(code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap keeps err as the cause; the message is shown, the cause only in debug output.
func Wrap(code string, err error, message string) *Error {
	if code == Internal && CodeOf(err) == Unsupported {
		code = Unsupported
	}
	return &Error{Code: code, Message: message, Cause: err}
}

func (e *Error) WithAdvice(advice string) *Error {
	e.Advice = advice
	return e
}

// WithDetail is a copy of As(err) with the detail key set (err itself is left as it is).
func WithDetail(err error, key, value string) *Error {
	return WithDetails(err, map[string]string{key: value})
}

// WithDetails copies As(err) and its details, then sets details. Wrapped details remain available to DetailsOf.
func WithDetails(err error, details map[string]string) *Error {
	e := *As(err)
	d := make(map[string]string, len(e.Details)+len(details))
	maps.Copy(d, e.Details)
	maps.Copy(d, details)
	e.Details = d
	return &e
}

// DetailsOf merges the Details of every *Error in err's chain (errors.Unwrap), an outer one's value winning; nil when
// there are none. So a wrapper (create wrapping the start that failed) still names what the inner error found out.
func DetailsOf(err error) map[string]string {
	var out map[string]string
	for ; err != nil; err = errors.Unwrap(err) {
		e, ok := err.(*Error)
		if !ok {
			continue
		}
		for k, v := range e.Details {
			if _, set := out[k]; set {
				continue
			}
			if out == nil {
				out = make(map[string]string)
			}
			out[k] = v
		}
	}
	return out
}

// CodeOf returns the code of the first *Error in err's chain, Internal for any other non-nil error, "" for nil.
func CodeOf(err error) string {
	if err == nil {
		return ""
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return Internal
}

// As returns the first *Error in err's chain, wrapping other errors as Internal.
func As(err error) *Error {
	if err == nil {
		return nil
	}
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return &Error{Code: Internal, Message: err.Error(), Cause: err}
}

// ExitCode maps an error to the process exit code (part of the contract, see README):
// 0 ok, 1 internal / other, 2 usage / invalid argument / license declined, 3 not found, 4 not ready / precheck
// failed / unsupported, 5 busy / already exists / port conflict, 6 timeout, 7 launch failed, 8 canceled, 9 update or
// components unavailable / rejected, proxy unavailable.
func ExitCode(err error) int {
	switch CodeOf(err) {
	case "":
		return 0
	case Usage, InvalidArgument, LicenseDeclined:
		return 2
	case NotFound:
		return 3
	case NotReady, PrecheckFailed, Unsupported:
		return 4
	case Busy, AlreadyExists, PortConflict:
		return 5
	case Timeout:
		return 6
	case LaunchFailed:
		return 7
	case Canceled:
		return 8
	case UpdateUnavailable, UpdateRejected, ComponentsUnavailable, ComponentsRejected, ProxyUnavailable:
		return 9
	default:
		return 1
	}
}
