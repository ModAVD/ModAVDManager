package update

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"modavd/internal/root"
)

// Version is a ModAVD version: 0.<minor>.<patch> (the first number is the stage, 0 while in beta). Ordered as
// (Major, Minor, Patch). Through 0.40 the minor number was the emulator build (Pkg.BuildId mas-N); later versions
// are numbered on their own and the package says its emulator build in modavd-manifest.json's "build".
type Version struct{ Major, Minor, Patch int }

// String is the version's identity, vX.Y.Z: state.json, comparisons, MirrorChyan's current_version.
func (v Version) String() string { return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Display is X.Y.Z, for people.
func (v Version) Display() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Less orders versions as (Major, Minor, Patch).
func (v Version) Less(o Version) bool {
	if v.Major != o.Major {
		return v.Major < o.Major
	}
	if v.Minor != o.Minor {
		return v.Minor < o.Minor
	}
	return v.Patch < o.Patch
}

var (
	newVersionRe    = regexp.MustCompile(`^v?([0-9]+)\.([0-9]+)\.([0-9]+)$`)
	legacyVersionRe = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+-mas([0-9]+)$`)
)

// ParseVersion reads vX.Y.Z (v optional) and the legacy tags v37.2.10-masN as (0, N, 0); ok=false for anything else
// (pre-releases such as v0.34.0-beta.1 included).
func ParseVersion(text string) (Version, bool) {
	s := strings.TrimSpace(text)
	if m := newVersionRe.FindStringSubmatch(s); m != nil {
		a, e1 := strconv.Atoi(m[1])
		b, e2 := strconv.Atoi(m[2])
		c, e3 := strconv.Atoi(m[3])
		return Version{a, b, c}, e1 == nil && e2 == nil && e3 == nil
	}
	if m := legacyVersionRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		return Version{0, n, 0}, err == nil
	}
	return Version{}, false
}

// requiresManifest: a release tag of the new format (not a legacy v37.2.10-masN) has modavd-manifest.json.
func requiresManifest(tag string) bool { return newVersionRe.MatchString(strings.TrimSpace(tag)) }

// displayVersion is the X.Y.Z of text when it parses, text otherwise.
func displayVersion(text string) string {
	if v, ok := ParseVersion(text); ok {
		return v.Display()
	}
	return text
}

// LocalVersion is the installed package's version (root.InstalledVersion).
func LocalVersion(r *root.Root) (Version, bool) {
	major, minor, patch, ok := r.InstalledVersion()
	return Version{major, minor, patch}, ok
}

func manifestVersion(dir string) (Version, bool) {
	b, err := root.ReadFileShared(filepath.Join(dir, manifestName))
	if err != nil {
		return Version{}, false
	}
	var m struct {
		Version any `json:"version"`
	}
	if json.Unmarshal(trimBOM(b), &m) != nil {
		return Version{}, false
	}
	s, _ := m.Version.(string)
	return ParseVersion(s)
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}
