//go:build darwin && arm64 && cgo

package components

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/playstore"
	"modavd/internal/root"
)

func TestNativePlayStoreStatusCapabilitiesAndProfile(t *testing.T) {
	r, err := root.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := PlayStoreState(r)
	if !st.InstallSupported || st.RequiresBridge || st.Ready || st.Archive != "arm64-v8a-35_r09.zip" || st.DownloadBytes != 1789211399 {
		t.Fatalf("native status: %+v", st)
	}
	p := playstore.CurrentProfile()
	if len(st.Files) != len(p.Files) {
		t.Fatalf("files: %+v", st.Files)
	}
	for i, f := range p.Files {
		if st.Files[i].Name != f.Name || st.Files[i].Size != f.Size || st.Files[i].Guest != f.Guest() {
			t.Fatalf("profile file %d: %+v / %+v", i, st.Files[i], f)
		}
	}
	// Status checks presence and metadata only. Sparse files avoid copying real
	// Google payloads; install/import must still verify their hashes separately.
	c := root.PlayStoreComponent()
	if err := os.MkdirAll(st.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range p.Files {
		file, err := os.Create(filepath.Join(st.Dir, f.Name))
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(f.Size)
		file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	props := c.Properties + "ModAVD.Source=import\n"
	write := func(text string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(st.Dir, "source.properties"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(props)
	if got := PlayStoreState(r); !got.Ready || got.Source != PlayStoreFromImport || got.Version != "9" {
		t.Fatalf("prepared ARM64 component: %+v", got)
	}
	for _, wrong := range []string{
		strings.ReplaceAll(props, "SystemImage.Abi=arm64-v8a", "SystemImage.Abi=x86_64"),
		strings.ReplaceAll(props, "AndroidVersion.ApiLevel=35", "AndroidVersion.ApiLevel=34"),
		strings.ReplaceAll(props, "SystemImage.TagId=google_apis_playstore", "SystemImage.TagId=google_apis"),
	} {
		write(wrong)
		if got := PlayStoreState(r); got.Ready {
			t.Fatalf("incompatible metadata accepted: %s", wrong)
		}
	}
}
