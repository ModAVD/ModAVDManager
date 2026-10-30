//go:build windows

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"modavd/internal/errs"
	"modavd/internal/logx"
)

// output writes the result or the error of one run in the chosen format.
type output struct {
	stdout, stderr io.Writer
	json           bool
	raw            bool // adb / mcp: stdout is not ours, errors go to stderr only
	log            *logx.Logger
}

type okEnvelope struct {
	OK   bool `json:"ok"`
	Data any  `json:"data"`
}

type errEnvelope struct {
	OK    bool      `json:"ok"`
	Error ErrorInfo `json:"error"`
}

// fail reports err and returns its exit code.
func (o *output) fail(err error) int {
	e := errs.As(err)
	if e.Cause != nil {
		o.log.Debugf("错误原因：%v", e.Cause)
	}
	switch {
	case o.raw && o.json:
		o.log.Errorf("%s", e.Error())
	case o.json:
		writeJSONLine(o.stdout, errEnvelope{OK: false, Error: errorInfo(e)})
	default:
		writeHumanError(o.stderr, e)
	}
	return errs.ExitCode(e)
}

// success prints data (envelope or summary). Non-zero only when the result could not be encoded.
func (o *output) success(cmd *command, data any) int {
	if o.json {
		if !writeJSONLine(o.stdout, okEnvelope{OK: true, Data: data}) {
			return errs.ExitCode(errs.New(errs.Internal, ""))
		}
		return 0
	}
	if cmd.human != nil {
		cmd.human(o.stdout, data)
	}
	return 0
}

// writeJSONLine writes v as exactly one line. A value that cannot be encoded (a bug) still yields one valid line:
// an internal error envelope; false then.
func writeJSONLine(w io.Writer, v any) bool {
	line, err := encodeLine(v)
	ok := err == nil
	if !ok {
		line, _ = encodeLine(errEnvelope{Error: ErrorInfo{Code: errs.Internal, Message: "结果无法编码成 JSON：" + err.Error()}})
	}
	w.Write(line)
	return ok
}

func encodeLine(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// "<", ">" and "&" stay readable in messages and paths.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeHumanError(w io.Writer, e *errs.Error) {
	msg := e.Message
	if msg == "" {
		msg = e.Code
	}
	fmt.Fprintf(w, "错误：%s\n", msg)
	if e.Advice != "" {
		fmt.Fprintf(w, "建议：%s\n", e.Advice)
	}
}
