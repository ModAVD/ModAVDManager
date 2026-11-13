// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"modavd/internal/update"
)

func TestForwardHandoffUsesOnlyLocalAuthenticatedEndpoint(t *testing.T) {
	id := update.HandoffID(strings.Repeat("a", 32))
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/update/handoff/receive" || r.Header.Get("Origin") != "http://"+r.Host {
			t.Errorf("request: %s %s, origin %s", r.Method, r.URL, r.Header.Get("Origin"))
		}
		_, port, _ := strings.Cut(r.Host, ":")
		cookie, err := r.Cookie("modavd_ui_" + port)
		if err != nil || cookie.Value != "local-token" {
			t.Errorf("authentication: %v", err)
		}
		var body struct{ ID update.HandoffID }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID != id {
			t.Errorf("body: %+v, %v", body, err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(base.Port())
	st := state{URL: srv.URL + "/", Port: port, Token: "local-token"}
	if err := forwardHandoff(st, id); err != nil || calls.Load() != 1 {
		t.Fatalf("forward: %v, calls %d", err, calls.Load())
	}
	for _, address := range []string{"http://elsewhere.invalid/", srv.URL + "/other/", srv.URL + "/?x=1", "http://user@" + base.Host + "/"} {
		st.URL = address
		if err := forwardHandoff(st, id); err == nil || calls.Load() != 1 {
			t.Fatalf("accepted address %s: %v", address, err)
		}
	}
}

func TestForwardHandoffDoesNotFollowRedirect(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	base, _ := url.Parse(srv.URL)
	port, _ := strconv.Atoi(base.Port())
	err := forwardHandoff(state{URL: srv.URL + "/", Port: port}, update.HandoffID(strings.Repeat("a", 32)))
	if err == nil || calls.Load() != 1 {
		t.Fatalf("redirect: %v, calls %d", err, calls.Load())
	}
}

func TestBadHandoffIDIsRejectedBeforeRootLookup(t *testing.T) {
	errors, _ := showBoxes(t)
	if code := run([]string{update.HandoffArg, "../bad"}); code != 2 || len(*errors) != 1 {
		t.Fatalf("code %d, errors %q", code, *errors)
	}
}
