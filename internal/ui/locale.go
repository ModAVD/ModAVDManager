package ui

import (
	"net/http"

	"modavd/internal/errs"
	"modavd/internal/i18n"
)

// The manager shows its text in i18n.Current() (ModAVD/ModAVD#165): the back end translates the human-readable
// strings of its answers and events (localizeJSON), the page its own (GET /api/locale). In Chinese nothing is
// re-encoded, so the bytes are as before.

// localizeJSON translates the text of an answer or event for the current language; data itself in Chinese.
func localizeJSON(data []byte) []byte { return i18n.LocalizeJSON(i18n.Current(), data) }

// localizeError translates a structured error's text.
func localizeError(e errorBody) errorBody {
	lang := i18n.Current()
	e.Message = i18n.Translate(lang, e.Message)
	e.Advice = i18n.Translate(lang, e.Advice)
	return e
}

type localeView struct {
	Language   string            `json:"language"`   // what the manager shows now
	Preference string            `json:"preference"` // "auto" or a code
	System     string            `json:"system"`     // what "auto" picks
	Languages  []i18n.Language   `json:"languages"`
	Messages   map[string]string `json:"messages"` // the page's strings in language ({} for Chinese)
}

func currentLocale() localeView {
	lang := i18n.Current()
	return localeView{
		Language:   lang,
		Preference: i18n.Preference(),
		System:     i18n.Resolve(i18n.PrefAuto),
		Languages:  i18n.Languages(),
		Messages:   i18n.Messages(lang),
	}
}

// handleLocale is GET /api/locale.
func (s *server) handleLocale(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, currentLocale())
}

// handleSetLocale is PUT /api/locale {"preference":"auto"|code}: saves the choice for this Windows user and switches
// the manager at once; the page reloads to show it.
func (s *server) handleSetLocale(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Preference *string `json:"preference"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.Preference == nil {
		writeError(w, errs.New(errs.Usage, "缺少 preference"))
		return
	}
	pref := *req.Preference
	if pref != i18n.PrefAuto && !i18n.Supported(pref) {
		writeError(w, errs.Newf(errs.InvalidArgument, "不认识的语言 %q", pref))
		return
	}
	if err := i18n.SavePreference(pref); err != nil {
		writeError(w, errs.Wrap(errs.Internal, err, "保存语言设置失败: "+err.Error()))
		return
	}
	i18n.Set(pref)
	s.log.Infof("界面语言改为 %s（%s）", pref, i18n.Current())
	writeJSON(w, http.StatusOK, currentLocale())
}
