//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package ui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/update"
)

const uiHandoffID update.HandoffID = "0123456789abcdef0123456789abcdef"

func TestUnrelatedManagerDoesNotTakeHandoff(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		if err := update.SaveHandoff(o.Root, update.NewHandoffResult(uiHandoffID, nil, nil, time.Now())); err != nil {
			t.Fatal(err)
		}
	})
	var v map[string]any
	expect(t, h.do("GET", "/api/update", nil), http.StatusOK, &v)
	if v["handoff"] != nil {
		t.Fatalf("unrelated startup exposed an old handoff: %v", v["handoff"])
	}
	if got := update.TakeHandoff(h.s.root, uiHandoffID, time.Now()); got == nil {
		t.Fatal("unrelated startup consumed the owned result")
	}
}

func handoffHarness(t *testing.T) *harness {
	t.Helper()
	return newHarness(t, func(o *Options) {
		o.HandoffID = uiHandoffID
		if err := update.SaveHandoff(o.Root, update.NewHandoffResult(uiHandoffID,
			&update.Status{Updated: true, CurrentVersion: "v0.40.0"}, nil, time.Now())); err != nil {
			t.Fatal(err)
		}
	})
}

func TestUpdateHandoffClaimedOnceAcrossWindows(t *testing.T) {
	h := handoffHarness(t)
	// Wrong IDs cannot claim or clear the pending result.
	other := strings.Repeat("a", 32)
	expect(t, h.do("POST", "/api/update/handoff/take", `{"id":"`+other+`"}`), http.StatusOK, nil)
	var count atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			resp := h.do("POST", "/api/update/handoff/take", `{"id":"`+string(uiHandoffID)+`"}`)
			defer resp.Body.Close()
			var result *update.HandoffResult
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || resp.StatusCode != http.StatusOK {
				t.Errorf("claim: HTTP %d, %v", resp.StatusCode, err)
			} else if result != nil {
				if result.ID != uiHandoffID || !result.OK {
					t.Errorf("wrong result: %+v", result)
				}
				count.Add(1)
			}
		})
	}
	wg.Wait()
	if count.Load() != 1 {
		t.Fatalf("%d windows received the notification", count.Load())
	}
	var v map[string]any
	expect(t, h.do("GET", "/api/update", nil), http.StatusOK, &v)
	if v["handoff"] != nil {
		t.Fatal("reopened window can still see the notification")
	}
}

func TestUpdateHandoffDeliveredToRunningManager(t *testing.T) {
	h := newHarness(t, nil)
	other := update.HandoffID(strings.Repeat("a", 32))
	for _, id := range []update.HandoffID{uiHandoffID, other} {
		if err := update.SaveHandoff(h.s.root, update.NewHandoffResult(id, nil,
			errs.New(errs.Busy, "管理器没有退出"), time.Now())); err != nil {
			t.Fatal(err)
		}
	}
	var received map[string]any
	expect(t, h.do("POST", "/api/update/handoff/receive", `{"id":"`+string(uiHandoffID)+`"}`), http.StatusOK, &received)
	if received["received"] != true {
		t.Fatalf("not received: %v", received)
	}
	// Retrying delivery cannot publish it again after a page claimed it.
	var result *update.HandoffResult
	expect(t, h.do("POST", "/api/update/handoff/take", `{"id":"`+string(uiHandoffID)+`"}`), http.StatusOK, &result)
	if result == nil || result.Error == nil || result.Error.Code != errs.Busy {
		t.Fatalf("wrong result: %+v", result)
	}
	expect(t, h.do("POST", "/api/update/handoff/receive", `{"id":"`+string(uiHandoffID)+`"}`), http.StatusOK, &received)
	if received["received"] != false {
		t.Fatalf("redelivered: %v", received)
	}
	if update.TakeHandoff(h.s.root, other, time.Now()) == nil {
		t.Fatal("delivery consumed another handoff")
	}
}

func TestUpdateHandoffClaimAuthAndExpiry(t *testing.T) {
	h := handoffHarness(t)
	path := "/api/update/handoff/take"
	body := `{"id":"` + string(uiHandoffID) + `"}`
	expectError(t, h.do("POST", path, body, noCookie), http.StatusUnauthorized, "unauthorized")
	expectError(t, h.do("POST", path, body, withHeader("Origin", "http://elsewhere.invalid")), http.StatusForbidden, "forbidden")
	expectError(t, h.do("POST", path, `{"id":"../bad"}`), http.StatusBadRequest, errs.Usage)
	h.s.upd.mu.Lock()
	h.s.upd.handoff.At = time.Now().Add(-update.HandoffFresh - time.Second).Format(time.RFC3339)
	h.s.upd.mu.Unlock()
	var result *update.HandoffResult
	expect(t, h.do("POST", path, body), http.StatusOK, &result)
	if result != nil {
		t.Fatalf("expired result: %+v", result)
	}
}

func TestLegacyHandoffIsIgnored(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		if err := os.MkdirAll(filepath.Join(o.Root.Dir, ".update"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(o.Root.Dir, ".update", "manager-result.json"),
			[]byte(`{"at":"`+time.Now().Format(time.RFC3339)+`","ok":true}`), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	if h.s.upd.handoff != nil {
		t.Fatal("legacy unowned result became a notification")
	}
}
