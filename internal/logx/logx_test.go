package logx

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestJSONFieldSink: fields follow time, level and msg (sorted, never replacing them); a plain sink and a plain Log
// line are unchanged.
func TestJSONFieldSink(t *testing.T) {
	var buf bytes.Buffer
	var plainMsgs []string
	l := NewFields(JSONFieldSink(&buf, Info)).Tee(func(_ Level, msg string) { plainMsgs = append(plainMsgs, msg) })
	l.LogFields(Info, "下载中", Fields{"progress": map[string]any{"stage": "downloading"}, "msg": "x", "a": 1, "bad": func() {}})
	l.Infof("plain")
	l.LogFields(Debug, "hidden", Fields{"a": 1})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines %q", lines)
	}
	if !strings.HasPrefix(lines[0], `{"time":"`) || !strings.HasSuffix(lines[0], `"level":"info","msg":"下载中","a":1,"progress":{"stage":"downloading"}}`) {
		t.Errorf("fields line %s", lines[0])
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &m); err != nil || len(m) != 3 || m["msg"] != "plain" {
		t.Errorf("plain line %s %v", lines[1], err)
	}
	if strings.Join(plainMsgs, ",") != "下载中,plain,hidden" {
		t.Errorf("plain sink got %v", plainMsgs)
	}
	var old bytes.Buffer
	New(JSONSink(&old, Info)).LogFields(Info, "m", Fields{"progress": 1})
	if strings.Contains(old.String(), "progress") {
		t.Errorf("JSONSink wrote fields: %s", old.String())
	}
}
