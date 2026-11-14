package root

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"

	"modavd/internal/jsonord"
)

// EmuBuild is the emulator's build ID: sdk\emulator\source.properties Pkg.BuildId and modavd-manifest.json's "build".
// Two forms are accepted:
//   - legacy "mas-N" (manifest: the integer N), used up to 0.41; it compares as version 0.N.0 (through 0.40 the
//     ModAVD version's minor number was N);
//   - the ModAVD version the emulator was built for, "X.Y.Z" (a leading "v" is accepted; manifest: the string).
//
// The zero value is "unknown".
type EmuBuild struct {
	Major, Minor, Patch int
	Legacy              bool // "mas-N": N is Minor
}

var emuBuildVersionRe = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)

// LegacyEmuBuild is "mas-<n>".
func LegacyEmuBuild(n int) EmuBuild { return EmuBuild{Minor: n, Legacy: true} }

// ParseEmuBuild reads "mas-N" (N > 0) or "X.Y.Z" / "vX.Y.Z" (not 0.0.0; no pre-release suffix). Surrounding
// whitespace is ignored; anything else is ok=false.
func ParseEmuBuild(text string) (EmuBuild, bool) {
	text = PyStrip(text)
	if m := modBuildRe.FindStringSubmatch(text); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			return EmuBuild{}, false
		}
		return LegacyEmuBuild(n), true
	}
	return parseEmuBuildVersion(text)
}

// parseEmuBuildVersion is the "X.Y.Z" form only.
func parseEmuBuildVersion(text string) (EmuBuild, bool) {
	m := emuBuildVersionRe.FindStringSubmatch(PyStrip(text))
	if m == nil {
		return EmuBuild{}, false
	}
	var b EmuBuild
	var err1, err2, err3 error
	b.Major, err1 = strconv.Atoi(m[1])
	b.Minor, err2 = strconv.Atoi(m[2])
	b.Patch, err3 = strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil || b.IsZero() {
		return EmuBuild{}, false
	}
	return b, true
}

// ManifestEmuBuild reads a modavd-manifest.json "build" value: a positive integer N (mas-N) or a version string.
// ok=false when missing or anything else.
func ManifestEmuBuild(v *jsonord.Value) (EmuBuild, bool) {
	if n, ok := v.Int(); ok {
		if n <= 0 || n > math.MaxInt32 {
			return EmuBuild{}, false
		}
		return LegacyEmuBuild(int(n)), true
	}
	if s, ok := v.Str(); ok {
		return parseEmuBuildVersion(s)
	}
	return EmuBuild{}, false
}

// IsZero: unknown.
func (b EmuBuild) IsZero() bool { return b == EmuBuild{} }

// LegacyNumber is N of "mas-N"; ok=false for the version form and unknown.
func (b EmuBuild) LegacyNumber() (int, bool) {
	if !b.Legacy {
		return 0, false
	}
	return b.Minor, true
}

// String is the build ID as written in Pkg.BuildId: "mas-41" or "0.42.0"; "" when unknown.
func (b EmuBuild) String() string {
	switch {
	case b.IsZero():
		return ""
	case b.Legacy:
		return "mas-" + strconv.Itoa(b.Minor)
	}
	return fmt.Sprintf("%d.%d.%d", b.Major, b.Minor, b.Patch)
}

// VersionText is the version the build compares as, for people: "0.41.0" for mas-41; "" when unknown.
func (b EmuBuild) VersionText() string {
	if b.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d.%d.%d", b.Major, b.Minor, b.Patch)
}

// Compare orders builds by (major, minor, patch), mas-N as 0.N.0: -1, 0 or +1. mas-41 and 0.41.0 compare equal (use
// == to tell the forms apart).
func (b EmuBuild) Compare(o EmuBuild) int {
	for _, d := range [3]int{b.Major - o.Major, b.Minor - o.Minor, b.Patch - o.Patch} {
		if d < 0 {
			return -1
		}
		if d > 0 {
			return 1
		}
	}
	return 0
}

// AtLeast: known and not below min.
func (b EmuBuild) AtLeast(min EmuBuild) bool { return !b.IsZero() && b.Compare(min) >= 0 }

// UnmarshalJSON reads a manifest "build": a positive integer N is mas-N, a string is the "X.Y.Z" form. A
// non-positive integer gives unknown (the manifest check rejects it); other values are an error.
func (b *EmuBuild) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	*b = EmuBuild{}
	if len(data) > 0 && data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		v, ok := parseEmuBuildVersion(s)
		if !ok {
			return fmt.Errorf("emulator build %q is not a version", s)
		}
		*b = v
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return fmt.Errorf("emulator build %s is neither an integer nor a version", data)
	}
	if n > 0 {
		*b = LegacyEmuBuild(n)
	}
	return nil
}

// MarshalJSON writes what UnmarshalJSON reads: N for mas-N, "X.Y.Z" otherwise, 0 when unknown.
func (b EmuBuild) MarshalJSON() ([]byte, error) {
	if b.IsZero() || b.Legacy {
		return []byte(strconv.Itoa(b.Minor)), nil
	}
	return json.Marshal(b.String())
}
