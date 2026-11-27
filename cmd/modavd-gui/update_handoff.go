// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"encoding/json"
	"fmt"
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
	base, err := url.Parse(st.URL)
	if err != nil || base.Scheme != "http" || base.Host != "127.0.0.1:"+strconv.Itoa(st.Port) ||
		base.User != nil || base.Path != "/" || base.RawQuery != "" || base.Fragment != "" || st.Port <= 0 || st.Port > 65535 {
		return fmt.Errorf("管理器地址不对")
	}
	body, err := json.Marshal(struct {
		ID update.HandoffID `json:"id"`
	}{id})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, st.URL+"api/update/handoff/receive", strings.NewReader(string(body)))
	if err != nil {
		return err
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
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("管理器接收更新结果失败（HTTP %d）", resp.StatusCode)
	}
	return nil
}
