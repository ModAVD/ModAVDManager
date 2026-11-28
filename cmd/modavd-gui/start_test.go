package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"modavd/internal/ui"
)

func TestParseArgsStart(t *testing.T) {
	for _, c := range []struct {
		args []string
		want *ui.StartRequest
	}{
		{nil, nil},
		{[]string{"--start", "3"}, &ui.StartRequest{Index: 3}},
		{[]string{"--start", "mas_3", "--no-window"}, &ui.StartRequest{Index: 3}},
		{[]string{"--start=0", "--launch=com.hypergryph.arknights"}, &ui.StartRequest{Index: 0, Launch: "com.hypergryph.arknights"}},
		{[]string{"--launch", "com.example.app/.Main", "--start", "9"}, &ui.StartRequest{Index: 9, Launch: "com.example.app/.Main"}},
	} {
		var r reported
		a, code, ok := parseArgs(c.args, r.report)
		if !ok || code != 0 || len(r.msgs) != 0 {
			t.Fatalf("%v: ok %v, exit %d, reported %q", c.args, ok, code, r.msgs)
		}
		if (a.start == nil) != (c.want == nil) || a.start != nil && *a.start != *c.want {
			t.Errorf("%v: start %+v, want %+v", c.args, a.start, c.want)
		}
	}
}

// A shortcut with a typo shows the error the way every other wrong argument does (a stream or, with neither, one box
// that is no more than the existing error box) and exits with 2 before it reaches the root, a running manager or a
// boot.
func TestParseArgsStartErrors(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--launch", "com.example.app"}, "--launch 要和 --start 一起用"},
		{[]string{"--launch", "", "--start", "3"}, "应用包名不对"},
		{[]string{"--start"}, "flag needs an argument: -start"},
		{[]string{"--start", ""}, "实例编号"},
		{[]string{"--start", "5"}, "实例编号"},
		{[]string{"--start", "10"}, "实例编号"},
		{[]string{"--start", "-1"}, "实例编号"},
		{[]string{"--start", "abc"}, "实例编号"},
		{[]string{"--start", "3 4"}, "实例编号"},
		{[]string{"--start", "3", "--launch", "app"}, "应用包名不对"},
		{[]string{"--start", "3", "--launch", "com.x;reboot"}, "应用包名不对"},
		{[]string{"--start", "3", "--launch", "com.x/.A;reboot"}, "Activity 名不对"},
		{[]string{"--start", "3", "--launch", "com.x/.A$B"}, "Activity 名不对"},
		{[]string{"--start", "3", "--launch", "$(calc).x"}, "应用包名不对"},
		{[]string{"--start", "3", "--launch", "com.x -n y"}, "应用包名不对"},
	} {
		var r reported
		_, code, ok := parseArgs(c.args, r.report)
		if ok || code != 2 || len(r.msgs) != 1 || !r.isErr[0] || !strings.Contains(r.msgs[0], c.want) {
			t.Errorf("%q: ok %v, exit %d, reported %q %v; want one error with %q", c.args, ok, code, r.msgs, r.isErr, c.want)
		}
	}
}

func TestHelpListsStart(t *testing.T) {
	var r reported
	parseArgs([]string{"--help"}, r.report)
	if len(r.msgs) != 1 {
		t.Fatalf("help %q", r.msgs)
	}
	for _, want := range []string{"--start 实例编号：", "--launch 包名[/Activity]：", "[--start 实例编号 [--launch 包名[/Activity]]]"} {
		if !strings.Contains(r.msgs[0], want) {
			t.Errorf("help lacks %q:\n%s", want, r.msgs[0])
		}
	}
}

func fakeManager(t *testing.T, status int, reply string, onRequest func(r *http.Request, body []byte)) (state, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if onRequest != nil {
			onRequest(r, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, reply)
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(base.Port())
	return state{URL: srv.URL + "/", Port: port, Token: "local-token"}, &calls
}

// The request goes to the running manager's own start endpoint, signed in with its cookie and from its own origin.
func TestForwardStart(t *testing.T) {
	for _, c := range []struct {
		req  ui.StartRequest
		body string
	}{
		{ui.StartRequest{Index: 3}, `{}`},
		{ui.StartRequest{Index: 7, Launch: "com.example.app/.Main"}, `{"launch":"com.example.app/.Main"}`},
	} {
		st, calls := fakeManager(t, http.StatusAccepted, `{"id":"j1","kind":"start","index":3}`, func(r *http.Request, body []byte) {
			if r.Method != http.MethodPost || r.URL.Path != "/api/instances/"+strconv.Itoa(c.req.Index)+"/start" ||
				r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("request %s %s, origin %q, type %q", r.Method, r.URL, r.Header.Get("Origin"), r.Header.Get("Content-Type"))
			}
			_, port, _ := strings.Cut(r.Host, ":")
			if ck, err := r.Cookie("modavd_ui_" + port); err != nil || ck.Value != "local-token" {
				t.Errorf("authentication: %v", err)
			}
			if !json.Valid(body) || string(body) != c.body {
				t.Errorf("body %s, want %s", body, c.body)
			}
		})
		if err := forwardStart(st, c.req); err != nil || calls.Load() != 1 {
			t.Fatalf("%+v: %v, calls %d", c.req, err, calls.Load())
		}
	}
}

func TestForwardStartRefused(t *testing.T) {
	st, _ := fakeManager(t, http.StatusConflict, `{"error":{"code":"busy","message":"实例 3 上已有任务在运行","advice":"等它做完"}}`, nil)
	err := forwardStart(st, ui.StartRequest{Index: 3})
	if err == nil || !strings.Contains(err.Error(), "实例 3 上已有任务在运行") || !strings.Contains(err.Error(), "等它做完") {
		t.Fatalf("busy: %v", err)
	}
	st, _ = fakeManager(t, http.StatusBadRequest, `{"error":{"code":"invalid_argument","message":"应用包名不对：「x」"}}`, nil)
	if err := forwardStart(st, ui.StartRequest{Index: 3, Launch: "x"}); err == nil || !strings.Contains(err.Error(), "应用包名不对") {
		t.Fatalf("bad name: %v", err)
	}
	st, _ = fakeManager(t, http.StatusUnauthorized, `not json`, nil)
	if err := forwardStart(st, ui.StartRequest{Index: 3}); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("unauthorized: %v", err)
	}
	// an ordinary 200 is not an accepted job
	st, _ = fakeManager(t, http.StatusOK, `{}`, nil)
	if err := forwardStart(st, ui.StartRequest{Index: 3}); err == nil {
		t.Fatal("200 taken for an accepted job")
	}
}

func TestForwardStartOnlyToTheManagersOwnAddress(t *testing.T) {
	st, calls := fakeManager(t, http.StatusAccepted, `{}`, nil)
	base, _ := url.Parse(st.URL)
	for _, address := range []string{"http://elsewhere.invalid/", st.URL + "other/", st.URL + "?x=1", "http://user@" + base.Host + "/",
		strings.Replace(st.URL, "http://", "https://", 1)} {
		bad := st
		bad.URL = address
		if err := forwardStart(bad, ui.StartRequest{Index: 3}); err == nil || calls.Load() != 0 {
			t.Fatalf("accepted address %s: %v", address, err)
		}
	}
	// a redirect is not followed with the cookie
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	b, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(b.Port())
	if err := forwardStart(state{URL: srv.URL + "/", Port: port}, ui.StartRequest{Index: 3}); err == nil || hits.Load() != 1 {
		t.Fatalf("redirect: %v, hits %d", err, hits.Load())
	}
}
