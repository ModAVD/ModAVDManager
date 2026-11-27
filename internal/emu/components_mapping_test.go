package emu

import (
	"encoding/json"
	"modavd/internal/root"
	"reflect"
	"testing"
)

func TestComponentStatusesEveryField(t *testing.T) {
	in := []root.ComponentStatus{
		{ID: "emulator", Name: "模拟器", Version: "37.2.10", Build: "mas-37", Installed: true, Optional: false, LocalSDK: true},
		{ID: "platform-tools", Name: "adb", Version: "37.0.1", Build: "", Installed: false, Optional: true, LocalSDK: false},
	}
	want := []ComponentStatus{
		{ID: "emulator", Name: "模拟器", Version: "37.2.10", Build: "mas-37", Installed: true, Optional: false, LocalSDK: true},
		{ID: "platform-tools", Name: "adb", Version: "37.0.1", Build: "", Installed: false, Optional: true, LocalSDK: false},
	}
	got := ComponentStatuses(in)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v want %+v", got, want)
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var fields []map[string]any
	json.Unmarshal(b, &fields)
	for _, f := range fields {
		if len(f) != 7 {
			t.Fatalf("missing JSON fields: %s", b)
		}
	}
	if got := ComponentStatuses(nil); got == nil || len(got) != 0 {
		t.Fatalf("empty status %v", got)
	}
}
