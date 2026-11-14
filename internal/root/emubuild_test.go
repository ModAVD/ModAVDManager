package root

import (
	"encoding/json"
	"testing"

	"modavd/internal/jsonord"
)

func TestParseEmuBuild(t *testing.T) {
	for text, want := range map[string]EmuBuild{
		"mas-41":   {Minor: 41, Legacy: true},
		" mas-7 ":  {Minor: 7, Legacy: true},
		"0.42.0":   {Minor: 42},
		"v0.42.1":  {Minor: 42, Patch: 1},
		"1.2.3":    {Major: 1, Minor: 2, Patch: 3},
		"0.042.00": {Minor: 42},
		"0.0.1":    {Patch: 1},
	} {
		got, ok := ParseEmuBuild(text)
		if !ok || got != want {
			t.Errorf("ParseEmuBuild(%q) = %+v %v, want %+v", text, got, ok, want)
		}
	}
	for _, bad := range []string{"", "mas-", "mas-0", "MAS-41", "mas-41b", "41", "0.42", "0.42.0-rc1", "0.42.0.1",
		"x0.42.0", "vv0.42.0", "0.0.0", "v", "mas-v0.42.0", "99999999999999999999.0.0"} {
		if got, ok := ParseEmuBuild(bad); ok || !got.IsZero() {
			t.Errorf("ParseEmuBuild(%q) = %+v %v", bad, got, ok)
		}
	}
}

func TestEmuBuildStringCompare(t *testing.T) {
	mas41, _ := ParseEmuBuild("mas-41")
	v41, _ := ParseEmuBuild("0.41.0")
	v42, _ := ParseEmuBuild("v0.42.0")
	if mas41.String() != "mas-41" || v42.String() != "0.42.0" || (EmuBuild{}).String() != "" {
		t.Fatalf("String: %q %q", mas41, v42)
	}
	if mas41.VersionText() != "0.41.0" || v42.VersionText() != "0.42.0" || (EmuBuild{}).VersionText() != "" {
		t.Fatal("VersionText")
	}
	if n, ok := mas41.LegacyNumber(); !ok || n != 41 {
		t.Fatal("LegacyNumber mas-41")
	}
	if _, ok := v42.LegacyNumber(); ok {
		t.Fatal("LegacyNumber 0.42.0")
	}
	if mas41.Compare(v41) != 0 || mas41 == v41 {
		t.Fatal("mas-41 compares as 0.41.0 but is another form")
	}
	if mas41.Compare(v42) != -1 || v42.Compare(mas41) != 1 || LegacyEmuBuild(40).Compare(mas41) != -1 {
		t.Fatal("order")
	}
	one, _ := ParseEmuBuild("1.0.0")
	if one.Compare(LegacyEmuBuild(99)) != 1 {
		t.Fatal("1.0.0 vs mas-99")
	}
	for b, ok := range map[EmuBuild]bool{
		LegacyEmuBuild(25): true, LegacyEmuBuild(24): false, v42: true, {Minor: 24, Patch: 9}: false,
		{Minor: 25}: true, {}: false,
	} {
		if b.AtLeast(MinEmulatorBuild) != ok {
			t.Errorf("%v AtLeast(min) != %v", b, ok)
		}
	}
	if MinEmulatorBuild.VersionText() != "0.25.0" {
		t.Fatal(MinEmulatorBuild.VersionText())
	}
}

func TestEmuBuildJSON(t *testing.T) {
	type doc struct {
		Build EmuBuild `json:"build"`
	}
	for text, want := range map[string]EmuBuild{
		`{"build": 41}`:        LegacyEmuBuild(41),
		`{"build": "0.42.0"}`:  {Minor: 42},
		`{"build": "v0.42.1"}`: {Minor: 42, Patch: 1},
		`{"build": 0}`:         {},
		`{"build": -3}`:        {},
		`{"build": null}`:      {},
		`{}`:                   {},
	} {
		var d doc
		if err := json.Unmarshal([]byte(text), &d); err != nil || d.Build != want {
			t.Errorf("%s: %+v %v, want %+v", text, d.Build, err, want)
		}
	}
	for _, bad := range []string{`{"build": "mas-41"}`, `{"build": "41"}`, `{"build": "0.42"}`, `{"build": 41.5}`,
		`{"build": true}`, `{"build": [41]}`} {
		var d doc
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("%s accepted: %+v", bad, d.Build)
		}
	}
	for b, want := range map[EmuBuild]string{LegacyEmuBuild(41): `{"build":41}`, {Minor: 42}: `{"build":"0.42.0"}`} {
		out, err := json.Marshal(doc{b})
		if err != nil || string(out) != want {
			t.Errorf("Marshal %v: %s %v", b, out, err)
		}
		var back doc
		if err := json.Unmarshal(out, &back); err != nil || back.Build != b {
			t.Errorf("round trip %v: %+v %v", b, back.Build, err)
		}
	}
}

func TestManifestEmuBuild(t *testing.T) {
	for text, want := range map[string]string{
		`{"build": 41}`: "mas-41", `{"build": "0.42.0"}`: "0.42.0", `{"build": "v1.0.0"}`: "1.0.0",
		`{"build": 0}`: "", `{"build": -1}`: "", `{"build": 41.0}`: "", `{"build": "41"}`: "", `{"build": "mas-41"}`: "",
		`{"build": "0.42.0-rc1"}`: "", `{"build": null}`: "", `{}`: "", `{"build": 99999999999}`: "",
	} {
		v, err := jsonord.Parse([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		got, ok := ManifestEmuBuild(v.Obj().Get("build"))
		if ok == (want == "") || got.String() != want {
			t.Errorf("%s: %q %v want %q", text, got, ok, want)
		}
	}
}
