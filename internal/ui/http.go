//go:build windows

package ui

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/root"
)

// maxJSONBody: every JSON request body is a handful of fields.
const maxJSONBody = 64 << 10

func (s *server) routes() {
	m := http.NewServeMux()
	m.HandleFunc("/", s.handleStatic)
	m.HandleFunc("/api/", s.handleNoAPI)
	m.HandleFunc("GET /api/info", s.handleInfo)
	m.HandleFunc("GET /api/instances", s.handleList)
	m.HandleFunc("POST /api/instances", s.handleCreate)
	m.HandleFunc("GET /api/instances/{i}", s.handleGet)
	m.HandleFunc("PATCH /api/instances/{i}", s.handleUpdate)
	m.HandleFunc("DELETE /api/instances/{i}", s.handleDelete)
	m.HandleFunc("POST /api/instances/{i}/start", s.handleStart)
	m.HandleFunc("POST /api/instances/{i}/stop", s.handleStop)
	m.HandleFunc("POST /api/instances/{i}/restart", s.handleRestart)
	m.HandleFunc("POST /api/instances/{i}/install", s.handleInstall)
	m.HandleFunc("POST /api/instances/{i}/window", s.handleWindow)
	m.HandleFunc("POST /api/instances/{i}/input", s.handleInput)
	m.HandleFunc("GET /api/instances/{i}/screenshot", s.handleScreenshot)
	m.HandleFunc("GET /api/instances/{i}/log", s.handleLog)
	m.HandleFunc("POST /api/open", s.handleOpen)
	m.HandleFunc("GET /api/check", s.handleCheck)
	m.HandleFunc("GET /api/jobs", s.handleJobs)
	m.HandleFunc("GET /api/jobs/{id}", s.handleJob)
	m.HandleFunc("POST /api/jobs/{id}/cancel", s.handleCancel)
	m.HandleFunc("GET /api/events", s.handleEvents)
	s.mux = m
}

// ServeHTTP applies the checks every request goes through, then routes.
func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if !s.hostOK(r.Host) {
		writeErrorStatus(w, http.StatusForbidden, "forbidden", "请求的 Host 不是本机管理器的地址", "")
		return
	}
	if r.URL.Path == "/api" || strings.HasPrefix(r.URL.Path, "/api/") {
		h.Set("Cache-Control", "no-store")
		if !s.authed(r) {
			writeErrorStatus(w, http.StatusUnauthorized, "unauthorized", "没有登录管理器",
				"请关掉这个页面，重新打开 ModAVD")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.originOK(r.Header.Get("Origin")) {
			writeErrorStatus(w, http.StatusForbidden, "forbidden", "请求不是从管理器页面发出的", "")
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

// cookieName carries our port: cookies are per host, not per port, and every manager window shares one Edge profile,
// so with two roots' managers open a single name would let the later login overwrite the earlier one's cookie.
func (s *server) cookieName() string { return cookieBase + "_" + strconv.Itoa(s.port) }

func (s *server) authed(r *http.Request) bool {
	for _, c := range r.CookiesNamed(s.cookieName()) {
		if subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1 {
			return true
		}
	}
	return false
}

func (s *server) handleNoAPI(w http.ResponseWriter, r *http.Request) {
	writeError(w, errs.Newf(errs.NotFound, "没有这个接口: %s %s", r.Method, r.URL.Path))
}

// ---- responses ---------------------------------------------------------------------------------------------------

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Advice  string `json:"advice"`
}

func newErrorBody(err error) *errorBody {
	if err == nil {
		return nil
	}
	e := errs.As(err)
	return &errorBody{Code: e.Code, Message: e.Message, Advice: e.Advice}
}

// httpStatus is the status table of API.md.
func httpStatus(code string) int {
	switch code {
	case errs.Usage, errs.InvalidArgument:
		return http.StatusBadRequest
	case errs.NotFound:
		return http.StatusNotFound
	case errs.Busy, errs.AlreadyExists, errs.PortConflict:
		return http.StatusConflict
	case errs.NotReady, errs.PrecheckFailed, errs.Unsupported:
		return http.StatusUnprocessableEntity
	case errs.Canceled:
		return 499 // nginx's "client closed request": there is no standard status for it
	case errs.LaunchFailed:
		return http.StatusBadGateway
	case errs.Timeout:
		return http.StatusGatewayTimeout
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		writeErrorStatus(w, http.StatusInternalServerError, errs.Internal, "结果转成 JSON 失败: "+err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

func writeError(w http.ResponseWriter, err error) {
	e := newErrorBody(err)
	writeErrorStatus(w, httpStatus(e.Code), e.Code, e.Message, e.Advice)
}

func writeErrorStatus(w http.ResponseWriter, status int, code, message, advice string) {
	body, _ := json.Marshal(struct {
		Error errorBody `json:"error"`
	}{errorBody{Code: code, Message: message, Advice: advice}})
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

// ---- requests ----------------------------------------------------------------------------------------------------

// pathIndex reads {i}; on a bad value it writes the 400 and returns false.
func pathIndex(w http.ResponseWriter, r *http.Request) (int, bool) {
	i, err := parseIndex(r.PathValue("i"))
	if err != nil {
		writeError(w, err)
		return 0, false
	}
	return i, true
}

func parseIndex(s string) (int, error) {
	if len(s) == 1 && s[0] >= '0' && s[0] <= '9' {
		if i := int(s[0] - '0'); root.ValidIndex(i) {
			return i, nil
		}
	}
	return 0, errs.Newf(errs.InvalidArgument, "实例编号 %q 不可用（只能是 0–9，5 除外）", s)
}

// decodeJSON reads a JSON request body into v. An empty body leaves v as it is (every field is optional somewhere).
// Unknown fields are refused, so a page that sends a misspelt option hears about it instead of being ignored.
func decodeJSON(r *http.Request, v any) error {
	data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
	if err != nil {
		return errs.Wrap(errs.Usage, err, "读请求体失败: "+err.Error())
	}
	if len(data) > maxJSONBody {
		return errs.New(errs.Usage, "请求体太大")
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return errs.New(errs.Usage, "请求体要是 JSON（Content-Type: application/json）")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return jsonError(err)
	}
	if dec.More() {
		return errs.New(errs.Usage, "请求体里只能有一个 JSON 对象")
	}
	return nil
}

func jsonError(err error) error {
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		field := typeErr.Field
		if field == "" {
			return errs.Wrap(errs.InvalidArgument, err, "请求体要是一个 JSON 对象")
		}
		return errs.Wrap(errs.InvalidArgument, err, fmt.Sprintf("字段 %s 的类型不对（要 %s）", field, jsonKind(typeErr.Type.Kind().String())))
	}
	if msg, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return errs.Wrap(errs.Usage, err, "不认识的字段 "+msg)
	}
	return errs.Wrap(errs.Usage, err, "请求体不是有效的 JSON: "+err.Error())
}

func jsonKind(k string) string {
	switch {
	case strings.HasPrefix(k, "int"), strings.HasPrefix(k, "uint"):
		return "整数"
	case strings.HasPrefix(k, "float"):
		return "数字"
	case k == "bool":
		return "true / false"
	case k == "string":
		return "字符串"
	default:
		return k
	}
}

// decode is decodeJSON writing the 400 itself.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeJSON(r, v); err != nil {
		writeError(w, err)
		return false
	}
	return true
}
