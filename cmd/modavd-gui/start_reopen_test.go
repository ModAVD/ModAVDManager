//go:build windows

package main

import (
	"net/http"
	"strings"
	"testing"

	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
	"modavd/internal/ui"
)

// A second launch with --start hands the request to the manager that holds the root, and with --no-window opens
// nothing else; a manager that refuses is an error shown the way startup errors are (exit 1), not a silent success.
func TestReopenForwardsStart(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testutil.TempDir(t))
	r, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := lockInstance(r)
	if err != nil || first == nil {
		t.Fatalf("lock: %v %v", first, err)
	}
	defer first.release()
	if second, err := lockInstance(r); err != nil || second != nil {
		t.Fatalf("the manager does not hold the root: %v %v", second, err)
	}

	var got []string
	st, calls := fakeManager(t, http.StatusAccepted, `{"id":"j1"}`, func(rq *http.Request, body []byte) {
		got = append(got, rq.URL.Path+" "+string(body))
	})
	if err := first.writeState(st.URL, st.Token); err != nil {
		t.Fatal(err)
	}
	errBoxes, _ := showBoxes(t)
	log := logx.Nop()
	if code := reopen(r, true, false, log, "", &ui.StartRequest{Index: 3, Launch: "com.example.app"}); code != 0 || calls.Load() != 1 ||
		len(got) != 1 || got[0] != `/api/instances/3/start {"launch":"com.example.app"}` || len(*errBoxes) != 0 {
		t.Fatalf("exit %d, calls %d, got %q, errors %q", code, calls.Load(), got, *errBoxes)
	}
	// no --start: nothing is sent
	if code := reopen(r, true, false, log, "", nil); code != 0 || calls.Load() != 1 {
		t.Fatalf("without --start: exit %d, calls %d", code, calls.Load())
	}

	refuse, _ := fakeManager(t, http.StatusConflict, `{"error":{"code":"busy","message":"实例 3 正在被另一个任务占着"}}`, nil)
	if err := first.writeState(refuse.URL, refuse.Token); err != nil {
		t.Fatal(err)
	}
	if code := reopen(r, true, false, log, "", &ui.StartRequest{Index: 3}); code != 1 || len(*errBoxes) != 1 ||
		!strings.Contains((*errBoxes)[0], "实例 3 正在被另一个任务占着") {
		t.Fatalf("refused: exit %d, errors %q", code, *errBoxes)
	}
}
