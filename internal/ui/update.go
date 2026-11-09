package ui

// The update part of the manager's settings (API.md "更新"): the root's ModAVD package version and update state, the
// update source (GitHub / MirrorChyan) and the MirrorChyan CDK, a check on request, and 更新 (update_apply.go). The
// source and CDK are also `modavd update`'s; AUTO-MAS's own updater uses AUTO-MAS's settings. Nothing goes online
// unless the user clicks 检查更新 or 更新.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
	"modavd/internal/update"
)

const updateCheckTimeout = time.Minute

// updateView is GET /api/update and the "update" event.
type updateView struct {
	*update.Status
	Checking     bool       `json:"checking"`
	CheckedAt    string     `json:"checked_at"`     // RFC 3339, "" when not checked since the manager started
	Error        *errorBody `json:"error"`          // the last check's failure
	InstallByCLI bool       `json:"install_by_cli"` // 更新 is not offered here (CanInstall false): only the CLI installs
	CanInstall   bool       `json:"can_install"`    // POST /api/update/apply may be tried
	// Handoff is what the CLI reported after the last 更新 handed to it (the swap and the manager's restart); null when
	// this manager was not started by such a hand-off, or the report is older than update.HandoffFresh.
	Handoff *update.HandoffResult `json:"handoff"`
}

type updatePanel struct {
	mu       sync.Mutex
	checking bool
	checked  *update.Status // the last successful check
	at       time.Time
	err      *errorBody
	handoff  *update.HandoffResult // taken from .update when the manager started
}

func (p *updatePanel) busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.checking
}

func (s *server) updater() *update.Updater { return update.New(s.root, s.log, update.Options{}) }

func (s *server) updateView() updateView {
	st := s.updater().Status()
	p := s.upd
	p.mu.Lock()
	defer p.mu.Unlock()
	if c := p.checked; c != nil {
		st.Checked, st.LatestVersion, st.LatestSource, st.RequiredAutoMAS, st.ReleaseURL =
			true, c.LatestVersion, c.LatestSource, c.RequiredAutoMAS, c.ReleaseURL
		if cur, ok := update.ParseVersion(st.CurrentVersion); ok {
			if latest, ok := update.ParseVersion(c.LatestVersion); ok {
				st.UpdateAvailable = cur.Less(latest)
			}
		} else {
			st.UpdateAvailable = c.UpdateAvailable
		}
		st.Message = c.Message
	}
	can := s.canInstall()
	v := updateView{Status: st, Checking: p.checking, Error: p.err, InstallByCLI: !can, CanInstall: can, Handoff: p.handoff}
	if !p.at.IsZero() {
		v.CheckedAt = p.at.Format(time.RFC3339)
	}
	return v
}

// canInstall: 更新 may be tried here — the manager knows how to come back, and updating is supported on this platform
// (Windows only for now).
func (s *server) canInstall() bool { return s.reopen != nil && root.CurrentPlatform().Windows() }

func (s *server) handleUpdateGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.updateView())
}

func (s *server) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if !decode(w, r, &struct{}{}) {
		return
	}
	p := s.upd
	p.mu.Lock()
	if p.checking {
		p.mu.Unlock()
		writeError(w, errs.New(errs.Busy, "正在检查，稍后再试"))
		return
	}
	p.checking = true
	p.mu.Unlock()
	s.hub.broadcast("update", s.updateView())

	ctx, cancel := context.WithTimeout(r.Context(), updateCheckTimeout)
	defer cancel()
	st, err := s.updater().Check(ctx)
	p.mu.Lock()
	p.checking, p.at = false, time.Now()
	if err != nil {
		p.err = newErrorBody(err)
		s.log.Warnf("检查 ModAVD 更新没成功: %s", errs.As(err).Message)
	} else {
		p.err, p.checked = nil, st
	}
	p.mu.Unlock()
	v := s.updateView()
	s.hub.broadcast("update", v)
	writeJSON(w, http.StatusOK, v)
}

// handleUpdateSettings: {source?, cdk?} — cdk "" clears it; it is stored encrypted and never sent back.
func (s *server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source *string `json:"source"`
		CDK    *string `json:"cdk"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.Source == nil && body.CDK == nil {
		writeError(w, errs.New(errs.Usage, "要给 source 或 cdk"))
		return
	}
	source := ""
	if body.Source != nil {
		source = *body.Source
	}
	if _, err := update.SaveSettings(source, body.CDK); err != nil {
		writeError(w, err)
		return
	}
	v := s.updateView()
	s.hub.broadcast("update", v)
	writeJSON(w, http.StatusOK, v)
}
