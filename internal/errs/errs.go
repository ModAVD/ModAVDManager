// Package errs is the error type every modavd package returns to the CLI and MCP layers. The Code is part of the
// machine-readable contract (`--json` prints it as error.code, MCP tool errors carry it), the Message is Chinese text
// for the user, the Advice says what to do about it.
package errs

import (
	"errors"
	"fmt"
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
	Internal           = "internal"            // anything else
	NotImplemented     = "not_implemented"
)

type Error struct {
	Code    string
	Message string
	Advice  string
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
	return &Error{Code: code, Message: message, Cause: err}
}

func (e *Error) WithAdvice(advice string) *Error {
	e.Advice = advice
	return e
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
// 0 ok, 1 internal / other, 2 usage / invalid argument, 3 not found, 4 not ready / precheck failed,
// 5 busy / already exists / port conflict, 6 timeout, 7 launch failed, 8 canceled.
func ExitCode(err error) int {
	switch CodeOf(err) {
	case "":
		return 0
	case Usage, InvalidArgument:
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
	default:
		return 1
	}
}
