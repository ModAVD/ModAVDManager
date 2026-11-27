package appproxy

import (
	"reflect"
	"testing"
)

const packagesList = `com.android.shell 2000 0 /data/user_de/0/com.android.shell platform:privapp:targetSdkVersion=34 3002,3003
com.example.game 10123 0 /data/user/0/com.example.game default:targetSdkVersion=34 3003
com.example.shared1 10150 0 /data/user/0/com.example.shared1 default:targetSdkVersion=33 3003
com.example.shared2 10150 1 /data/user/0/com.example.shared2 default:targetSdkVersion=33 3003
com.google.android.gms 10124 0 /data/user/0/com.google.android.gms privapp:targetSdkVersion=34 3002,3003
com.work.profile 1010125 0 /data/user/10/com.work.profile default 3003
broken line
not.a.uid abc 0 /x default none
`

func TestParsePackages(t *testing.T) {
	got := ParsePackages([]byte(packagesList))
	want := map[string]int{"com.example.game": 10123, "com.example.shared1": 10150, "com.example.shared2": 10150,
		"com.google.android.gms": 10124}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestResolveApps(t *testing.T) {
	st, uids := ResolveApps([]string{"com.google.android.gms", "com.example.shared2", "com.not.installed",
		"com.example.game"}, ParsePackages([]byte(packagesList)))
	want := []AppStatus{
		{Package: "com.google.android.gms", UID: 10124},
		{Package: "com.example.shared2", UID: 10150, SharedWith: []string{"com.example.shared1"}},
		{Package: "com.not.installed"},
		{Package: "com.example.game", UID: 10123},
	}
	if !reflect.DeepEqual(st, want) {
		t.Errorf("status %+v", st)
	}
	if !reflect.DeepEqual(uids, []int{10123, 10124, 10150}) {
		t.Errorf("uids %v", uids)
	}
	if _, uids := ResolveApps([]string{"com.example.shared1", "com.example.shared2"}, ParsePackages([]byte(packagesList))); !reflect.DeepEqual(uids, []int{10150}) {
		t.Errorf("shared uid twice: %v", uids)
	}
}

func TestRanges(t *testing.T) {
	got := Ranges([]int{10010, 10123, 10124, 10125, 10200, 10202})
	want := []UIDRange{{10010, 10010}, {10123, 10125}, {10200, 10200}, {10202, 10202}}
	if !reflect.DeepEqual(got, want) || got[1].String() != "10123-10125" {
		t.Errorf("%v", got)
	}
	if Ranges(nil) != nil {
		t.Error("nil")
	}
}
