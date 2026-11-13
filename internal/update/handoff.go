package update

// The manager's update (ModAVD.exe, 设置 → 更新). The manager cannot replace itself while it runs, so it hands the swap
// to the CLI: it downloads and verifies (Prepare), stops the instances, then starts
//
//	tools\modavd.exe update install --after-pid <its pid> --reopen <its exe> [--reopen-arg A ...]
//
// and exits. Each hand-off has its own ID and result under .update\manager-results\. The CLI passes that ID to
// the manager it starts again; only that launch takes the result.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
)

const (
	handoffDir = "manager-results"
	// HandoffArg is the manager's launch option for the result of one update.
	HandoffArg = "--update-handoff"
	// HandoffFresh: a result older than this is not shown (a manager started long after a failed hand-off).
	HandoffFresh = 30 * time.Minute
	// AfterPIDWait is how long the CLI waits for the manager it is to replace to exit.
	AfterPIDWait = time.Minute
)

// HandoffID identifies one update hand-off, independently of its version or timestamp.
type HandoffID string

func NewHandoffID() (HandoffID, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return HandoffID(hex.EncodeToString(b)), nil
}

// ParseHandoffID accepts only the generated form, which is also safe as a filename.
func ParseHandoffID(s string) (HandoffID, error) {
	if len(s) == 32 {
		valid := true
		for _, c := range s {
			valid = valid && (c >= '0' && c <= '9' || c >= 'a' && c <= 'f')
		}
		if valid {
			return HandoffID(s), nil
		}
	}
	return "", errs.New(errs.Usage, "更新交接 ID 必须是 32 位小写十六进制字符")
}

// PrepareHandoff prepares a manager update. If the source is unavailable, it can reuse an earlier stage, but
// only after checking its fingerprint under the update lock and comparing it to the installed version.
func (u *Updater) PrepareHandoff(ctx context.Context) (*Status, error) {
	st, err := u.Prepare(ctx)
	if errs.CodeOf(err) != errs.UpdateUnavailable {
		return st, err
	}
	if ctx.Err() != nil {
		return st, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
	lock, lerr := u.begin(st)
	if lerr != nil {
		return st, lerr
	}
	defer lock.Unlock()
	out := u.finish(st)
	if !out.Updatable || out.Phase != phaseStaged {
		return out, err
	}
	if verr := u.l.checkStaged(u.l.readState()); verr != nil {
		return u.finish(st), verr
	}
	staged, haveStage := ParseVersion(out.PreparedVersion)
	local, haveLocal := ParseVersion(out.CurrentVersion)
	if !haveStage || !haveLocal || !local.Less(staged) {
		return out, err
	}
	if ctx.Err() != nil {
		return out, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
	note := "查不了最新版本，使用已下载并核对好的 " + staged.Display()
	out.CheckError, out.Message = errs.As(err).Message, note
	out.Notices = append(out.Notices, note)
	u.Log.Warnf("%s", note)
	return out, nil
}

// HandoffResult is what the CLI tells the manager it starts again.
type HandoffResult struct {
	ID      HandoffID     `json:"id"`
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

func handoffPath(r *root.Root, id HandoffID) string {
	return filepath.Join(layout{root: r.Dir}.dir(), handoffDir, string(id)+".json")
}

// NewHandoffResult is the result of an install: st as Install returned it, err its error.
func NewHandoffResult(id HandoffID, st *Status, err error, now time.Time) *HandoffResult {
	res := &HandoffResult{ID: id, At: now.Format(time.RFC3339), OK: err == nil}
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

// SaveHandoff atomically writes this hand-off's result without replacing another hand-off's file.
func SaveHandoff(r *root.Root, res *HandoffResult) error {
	if res == nil {
		return errs.New(errs.Usage, "没有更新交接结果")
	}
	if _, err := ParseHandoffID(string(res.ID)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	path := handoffPath(r, res.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	defer os.Remove(tmp)
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

// TakeHandoff claims only id's result, once. Missing IDs never read a result. A stale, broken or mismatched owned
// result is discarded. An exclusive claim file prevents simultaneous consumers from taking the same result.
func TakeHandoff(r *root.Root, id HandoffID, now time.Time) *HandoffResult {
	if _, err := ParseHandoffID(string(id)); err != nil {
		return nil
	}
	path := handoffPath(r, id)
	claimPath := path + ".claim"
	claim, err := os.OpenFile(claimPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	claim.Close()
	defer os.Remove(claimPath)
	data, err := root.ReadFileShared(path)
	if err != nil {
		return nil
	}
	if err := os.Remove(path); err != nil {
		return nil
	}
	var res HandoffResult
	if json.Unmarshal(data, &res) != nil || res.ID != id {
		return nil
	}
	if !res.Fresh(now) {
		return nil
	}
	return &res
}

func (res *HandoffResult) Fresh(now time.Time) bool {
	at, err := time.Parse(time.RFC3339, res.At)
	return err == nil && now.Sub(at) <= HandoffFresh && at.Sub(now) <= time.Minute
}
