//go:build windows

package update

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/testutil"
)

// From 0.41 the version is not 0.<emulator build>.x: the package's manifest says the emulator build in "build", and
// the package check compares sdk\emulator's Pkg.BuildId with that, not with the version.
func TestVerifyPackageVersionNotTiedToBuild(t *testing.T) {
	put := func(t *testing.T, version string, build any, noManifest bool, pkgBuildID string) layout {
		t.Helper()
		l := layout{root: testutil.TempDir(t)}
		var extra map[string]string
		if pkgBuildID != "" {
			extra = map[string]string{"sdk/emulator/source.properties": "Pkg.Revision=37.2.10\nPkg.BuildId=" + pkgBuildID + "\n"}
		}
		files := pkgFiles(40, extra)
		for rel, body := range files {
			p := l.pkgAt(rel)
			os.MkdirAll(filepath.Dir(p), 0o755)
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if noManifest {
			return l
		}
		var m map[string]any
		json.Unmarshal(manifestFor(40, files), &m)
		m["version"] = version
		if build == nil {
			delete(m, "build")
		} else {
			m["build"] = build
		}
		b, _ := json.Marshal(m)
		os.WriteFile(l.pkgAt(manifestName), b, 0o644)
		return l
	}
	for _, tc := range []struct {
		name    string
		version string
		build   any
		target  Version
		tag     string
		wantErr string
		pkgID   string // the package's Pkg.BuildId; "" = mas-40
	}{
		{"new version, emulator mas-40", "v0.41.0", 40, Version{0, 41, 0}, "v0.41.0", "", ""},
		{"version far from the build", "v0.57.3", 40, Version{0, 57, 3}, "v0.57.3", "", ""},
		{"manifest names another build", "v0.41.0", 39, Version{0, 41, 0}, "v0.41.0", "包里模拟器的构建号（40）和要装的 0.41.0（构建号 39）不一致", ""},
		{"legacy manifest without build", "v0.40.0", nil, Version{0, 40, 0}, "v0.40.0", "", ""},
		{"legacy manifest without build, other version", "v0.41.0", nil, Version{0, 41, 0}, "v0.41.0", "包里模拟器的构建号（40）", ""},
		{"build not positive", "v0.41.0", 0, Version{0, 41, 0}, "v0.41.0", "构建号（0）不对", ""},
		{"version differs from the target", "v0.41.0", 40, Version{0, 42, 0}, "v0.42.0", "清单的版本", ""},
		// From 0.42 the build ID is the version: "build" is that string and Pkg.BuildId the same version.
		{"version build", "v0.42.0", "0.42.0", Version{0, 42, 0}, "v0.42.0", "", "0.42.0"},
		{"version build, patch release", "v0.42.3", "0.42.0", Version{0, 42, 3}, "v0.42.3", "", "0.42.0"},
		{"version build, v prefix", "v0.42.0", "v0.42.0", Version{0, 42, 0}, "v0.42.0", "", "v0.42.0"},
		{"version build mismatch", "v0.43.0", "0.43.0", Version{0, 43, 0}, "v0.43.0", "包里模拟器的构建号（0.42.0）和要装的 0.43.0（构建号 0.43.0）不一致", "0.42.0"},
		{"version manifest, legacy emulator", "v0.41.0", "0.41.0", Version{0, 41, 0}, "v0.41.0", "包里模拟器的构建号（40）", ""},
		{"version manifest, legacy emulator same number", "v0.41.0", "0.41.0", Version{0, 41, 0}, "v0.41.0", "包里模拟器的构建号（41）", "mas-41"},
		{"legacy manifest, version emulator", "v0.42.0", 42, Version{0, 42, 0}, "v0.42.0", "包里模拟器的构建号（0.42.0）", "0.42.0"},
		{"no build, version emulator", "v0.42.0", nil, Version{0, 42, 0}, "v0.42.0", "包里模拟器的构建号（0.42.0）", "0.42.0"},
		{"build string not a version", "v0.42.0", "42", Version{0, 42, 0}, "v0.42.0", "构建号（42）不对", "0.42.0"},
		{"build pre-release", "v0.42.0", "0.42.0-rc1", Version{0, 42, 0}, "v0.42.0", "构建号（0.42.0-rc1）不对", "0.42.0"},
		{"build mas string", "v0.42.0", "mas-42", Version{0, 42, 0}, "v0.42.0", "构建号（mas-42）不对", "mas-42"},
		{"emulator pre-release", "v0.42.0", "0.42.0", Version{0, 42, 0}, "v0.42.0", "包里模拟器的构建号（读不到）", "0.42.0-rc1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := put(t, tc.version, tc.build, false, tc.pkgID)
			_, err := l.verifyPackage(tc.target, tc.tag)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if tc.wantErr == "" {
				if err := l.buildStage(tc.target); err != nil {
					t.Fatalf("buildStage: %v", err)
				}
			}
		})
	}
	t.Run("legacy package without manifest", func(t *testing.T) {
		l := put(t, "", nil, true, "")
		if _, err := l.verifyPackage(Version{0, 40, 0}, "v37.2.10-mas40"); err != nil {
			t.Fatal(err)
		}
		if _, err := l.verifyPackage(Version{0, 41, 0}, "v37.2.10-mas41"); err == nil {
			t.Fatal("mas-40 emulator accepted as mas-41")
		}
	})
}
