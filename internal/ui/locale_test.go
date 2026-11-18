package ui

import (
	"encoding/json"
	"testing"

	"modavd/internal/i18n"
)

func TestLocalizeJSON(t *testing.T) {
	in := []byte(`{"message":"3 秒","advice":"3 秒","title":"3 秒","warnings":["3 秒"],"big":12345678901234567890,` +
		`"items":[{"title":"3 秒","reason":"3 秒","id":"3 秒"}],"details":{"reason":"3 秒"},"job":{"error":{"code":"3 秒","message":"3 秒"}}}` + "\n")
	if got := localizeJSON(in); string(got) != string(in) {
		t.Fatalf("Chinese changed the bytes: %s", got)
	}
	i18n.Set(i18n.English)
	defer i18n.Set(i18n.Chinese)
	var v map[string]any
	out := localizeJSON(in)
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	en := i18n.T("3 秒")
	if en == "3 秒" {
		t.Fatal("no English for the test key")
	}
	item := v["items"].([]any)[0].(map[string]any)
	job := v["job"].(map[string]any)["error"].(map[string]any)
	for name, got := range map[string]any{
		"message": v["message"], "advice": v["advice"], "warnings": v["warnings"].([]any)[0],
		"item title": item["title"], "item reason": item["reason"], "job error": job["message"],
	} {
		if got != en {
			t.Errorf("%s: %v", name, got)
		}
	}
	for name, got := range map[string]any{
		"title": v["title"], "item id": item["id"], "details": v["details"].(map[string]any)["reason"], "code": job["code"],
	} {
		if got != "3 秒" {
			t.Errorf("%s translated: %v", name, got)
		}
	}
	if string(json.RawMessage(out)) == "" || !json.Valid(out) || out[len(out)-1] != '\n' {
		t.Errorf("output %q", out)
	}
	var big struct{ Big json.Number }
	json.Unmarshal(out, &big)
	if big.Big != "12345678901234567890" {
		t.Errorf("number %s", big.Big)
	}
}
