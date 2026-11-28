// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"modavd/internal/update"
)

// forwardHandoff delivers the restart's ID to the existing manager, even with --no-window. It never sends the
// local login cookie through an environment proxy or a redirect.
func forwardHandoff(st state, id update.HandoffID) error {
	status, _, err := postManager(st, "api/update/handoff/receive", struct {
		ID update.HandoffID `json:"id"`
	}{id})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("管理器接收更新结果失败（HTTP %d）", status)
	}
	return nil
}

// postManager sends one JSON POST to the running manager's local API (path without the leading slash) and returns the
// status and the body (at most 64 KiB). Only the manager's own address from its state file is accepted, the login
// cookie goes nowhere else, and neither an environment proxy nor a redirect is followed.
func postManager(st state, path string, body any) (int, []byte, error) {
	base, err := url.Parse(st.URL)
	if err != nil || base.Scheme != "http" || base.Host != "127.0.0.1:"+strconv.Itoa(st.Port) ||
		base.User != nil || base.Path != "/" || base.RawQuery != "" || base.Fragment != "" || st.Port <= 0 || st.Port > 65535 {
		return 0, nil, fmt.Errorf("管理器地址不对")
	}
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, st.URL+path, strings.NewReader(string(data)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", strings.TrimSuffix(st.URL, "/"))
	req.AddCookie(&http.Cookie{Name: "modavd_ui_" + strconv.Itoa(st.Port), Value: st.Token})
	client := &http.Client{Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, out, nil
}
