package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"modavd/internal/ui"
)

// forwardStart asks the manager that is already running to start the instance (and open the app), as the page's start
// button does: POST /api/instances/{i}/start. The manager answers 202 with the job once it has begun; how the job ends
// is on its page, not here. A refusal (the instance is busy, running headless ...) comes back as its message.
func forwardStart(st state, req ui.StartRequest) error {
	body := struct {
		Launch string `json:"launch,omitempty"`
	}{req.Launch}
	status, out, err := postManager(st, "api/instances/"+strconv.Itoa(req.Index)+"/start", body)
	if err != nil {
		return err
	}
	if status == http.StatusAccepted {
		return nil
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Advice  string `json:"advice"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &e) == nil && e.Error.Message != "" {
		msg := e.Error.Message
		if e.Error.Advice != "" {
			msg += "。" + e.Error.Advice
		}
		return fmt.Errorf("%s", msg)
	}
	return fmt.Errorf("管理器没有接受开机请求（HTTP %d）", status)
}
