package update

// The manager's update (ModAVD.exe, 设置 → 更新). The manager cannot replace itself while it runs, so it hands the swap
// to the CLI: it downloads and verifies (Prepare), stops the instances, then starts
//
//	tools\modavd.exe update install --after-pid <its pid> --reopen <its exe> [--reopen-arg A ...]
//
// and exits. The CLI waits for it to be gone, installs, writes what happened to .update\manager-result.json and
// starts the manager again; the manager takes the file on start and shows the result.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
)

const (
	handoffName = "manager-result.json"
	// HandoffFresh: a result older than this is not shown (a manager started long after a failed hand-off).
	HandoffFresh = 30 * time.Minute
	// AfterPIDWait is how long the CLI waits for the manager it is to replace to exit.
	AfterPIDWait = time.Minute
)

// HandoffResult is what the CLI tells the manager it starts again.
type HandoffResult struct {
	At      string        `json:"at"` // RFC 3339
	OK      bool          `json:"ok"`
	Updated bool          `json:"updated"` // a new version was swapped in
	Version string        `json:"version"` // the root's version now, vX.Y.Z ("" when unknown)
	Message string        `json:"message"`
	Error   *HandoffError `json:"error"` // nil when OK
}

// HandoffError is the failure, as the CLI's --json error has it.
type HandoffError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Advice  string `json:"advice"`
}

func handoffPath(r *root.Root) string { return filepath.Join(layout{root: r.Dir}.dir(), handoffName) }

// NewHandoffResult is the result of an install: st as Install returned it, err its error.
func NewHandoffResult(st *Status, err error, now time.Time) *HandoffResult {
	res := &HandoffResult{At: now.Format(time.RFC3339), OK: err == nil}
	if st != nil {
		res.Updated, res.Version, res.Message = st.Updated, st.CurrentVersion, st.Message
	}
	if err != nil {
		e := errs.As(err)
		res.Error = &HandoffError{Code: e.Code, Message: e.Message, Advice: e.Advice}
		res.Message = e.Message
	}
	return res
}

// SaveHandoff writes res for the manager the CLI starts next (atomically, replacing an older one).
func SaveHandoff(r *root.Root, res *HandoffResult) error {
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	path := handoffPath(r)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	return root.ReplaceWithRetry(tmp, path)
}

// ProgramsInUse lists the other processes running this root's ModAVD.exe or tools\modavd.exe (an MCP server, another
// CLI ...), this process and the short-lived shutdown helpers aside. An install refuses them, so the manager asks
// before it stops any instance (the emulators themselves are what it stops, so they are not listed).
func (u *Updater) ProgramsInUse() ([]string, error) {
	uses, err := u.processesUsing(true)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range uses {
		if p.RootProgram && !p.ShutdownHelper {
			out = append(out, fmt.Sprintf("%s（pid %d）", p.Name, p.PID))
		}
	}
	return out, nil
}

// TakeHandoff reads and removes the CLI's result; nil when there is none, it cannot be read, or it is older than
// HandoffFresh (then it is removed too).
func TakeHandoff(r *root.Root, now time.Time) *HandoffResult {
	path := handoffPath(r)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	_ = os.Remove(path)
	var res HandoffResult
	if json.Unmarshal(data, &res) != nil {
		return nil
	}
	at, err := time.Parse(time.RFC3339, res.At)
	if err != nil || now.Sub(at) > HandoffFresh || at.Sub(now) > time.Minute {
		return nil
	}
	return &res
}
