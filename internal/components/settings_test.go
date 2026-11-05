package components

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "components-settings.json")
	old := sourceSettingsPath
	sourceSettingsPath = func() string { return path }
	t.Cleanup(func() { sourceSettingsPath = old })
	if got := LoadSourceSettings(); got.Set || got.Mirror != "" || len(got.MirrorURLs) != 0 {
		t.Fatalf("missing settings = %+v", got)
	}
	want, err := SaveSourceSettings("tencent", nil)
	if err != nil || !want.Set || want.Mirror != "tencent" {
		t.Fatalf("save = %+v, %v", want, err)
	}
	if got := LoadSourceSettings(); !got.Set || got.Mirror != "tencent" {
		t.Fatalf("load = %+v", got)
	}
	if _, err := SaveSourceSettings("", []string{"https://mirror.example/repo"}); err != nil {
		t.Fatal(err)
	}
	got := LoadSourceSettings()
	if !got.Set || got.Mirror != "" || len(got.MirrorURLs) != 1 || got.MirrorURLs[0] != "https://mirror.example/repo/" {
		t.Fatalf("custom load = %+v", got)
	}
	if mode := fileMode(t, path); mode.Perm() == 0 {
		t.Fatalf("settings mode is empty: %o", mode.Perm())
	}
}

func TestSourceSettingsRejectBadMirror(t *testing.T) {
	path := filepath.Join(t.TempDir(), "components-settings.json")
	old := sourceSettingsPath
	sourceSettingsPath = func() string { return path }
	t.Cleanup(func() { sourceSettingsPath = old })
	if _, err := SaveSourceSettings("aliyun", nil); err == nil {
		t.Fatal("unknown mirror accepted")
	}
	if _, err := SaveSourceSettings("", []string{"ftp://example/"}); err == nil {
		t.Fatal("bad URL accepted")
	}
}

func TestImportLegacySource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "components-settings.json")
	old := sourceSettingsPath
	sourceSettingsPath = func() string { return path }
	t.Cleanup(func() { sourceSettingsPath = old })
	if err := os.MkdirAll(filepath.Join(dir, "root", "downloads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root", "downloads", "source.txt"), []byte("google\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, migrated, err := ImportLegacySource(filepath.Join(dir, "root"))
	if err != nil || !migrated || got.Mirror != "google" || !got.Set {
		t.Fatalf("import = %+v, migrated=%v, err=%v", got, migrated, err)
	}
	if _, migrated, err := ImportLegacySource(filepath.Join(dir, "root")); err != nil || migrated {
		t.Fatalf("second import migrated=%v err=%v", migrated, err)
	}
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode()
}
