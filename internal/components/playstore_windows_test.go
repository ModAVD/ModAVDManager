package components

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/playstore"
	"modavd/internal/root"
)

const (
	storeImage = "the Play image's system.img"
	storeAPK   = "Phonesky.apk from the test image"
	storeProps = "Pkg.Revision=14\n"
)

// storeSpec is the Google Play Store component as in the real table, with test files: the zip at the Play image's
// URL (whose last part is the Google APIs image's), its own name in downloads\, two pinned files.
func storeSpec(data []byte) root.SDKComponent {
	return root.SDKComponent{
		ID: "play-store", Name: root.PlayStoreName, FetchName: "Google Play 商店（测试）", Version: "14",
		Target: root.PlayStoreDir, StatusFiles: []string{"Phonesky.apk"},
		URL: "sys-img/google_apis_playstore/x86_64-34_r14.zip", ZipName: "google_apis_playstore-x86_64-34_r14.zip",
		Size: int64(len(data)), SHA1: sum1(data), ArchiveRoot: "x86_64",
		KeyFiles: []string{"Phonesky.apk", "cert.der"}, KeyHashes: map[string]string{"Phonesky.apk": sum256(storeAPK),
			"cert.der": sum256("cert")}, ExtractedBytes: 1 << 20, Properties: storeProps,
	}
}

func storeZip(t *testing.T) []byte {
	t.Helper()
	// system.img not first: only that entry is unpacked
	return makeZip(t, zentry{"x86_64/vendor.img", "vendor"}, zentry{"x86_64/system.img", storeImage},
		zentry{"x86_64/kernel-ranchu", "kernel"})
}

// storeHooks puts the test store component in the catalogue and an extractor that checks it is given the unpacked
// system.img (and nothing else of the zip beside it) and writes the two files, or fails with fail.
func storeHooks(t *testing.T, spec root.SDKComponent, fail error) *[]string {
	t.Helper()
	oldCat, oldExtract := playStoreCatalogue, playStoreExtract
	t.Cleanup(func() { playStoreCatalogue, playStoreExtract = oldCat, oldExtract })
	playStoreCatalogue = func() root.SDKComponent { return spec }
	var calls []string
	playStoreExtract = func(img string, files []playstore.File, dir string) error {
		b, err := os.ReadFile(img)
		if err != nil || string(b) != storeImage {
			t.Errorf("extract got %s: %q %v", img, b, err)
		}
		if siblings, _ := os.ReadDir(filepath.Dir(img)); len(siblings) != 2 { // system.img and the output directory
			t.Errorf("staging holds %d entries", len(siblings))
		}
		calls = append(calls, filepath.Base(img))
		if len(files) != len(playstore.Files) {
			t.Errorf("extract asked for %d files", len(files))
		}
		if fail != nil {
			return fail
		}
		putFile(t, filepath.Join(dir, "Phonesky.apk"), storeAPK)
		putFile(t, filepath.Join(dir, "cert.der"), "cert")
		return nil
	}
	return &calls
}

// TestFetchPlayStore: with PlayStore the store comes after the required components: its zip under its own name,
// only the system image unpacked and handed to the extractor, the files and source.properties in sdk\playstore, the
// image, the zip and sdk\.staging gone; the next run skips it. Without PlayStore it is neither asked for nor touched.
func TestFetchPlayStore(t *testing.T) {
	pt, img, store := ptZip(t, adbBody), imgZip(t), storeZip(t)
	setup(t, ptSpec(pt), imgSpec(img))
	calls := storeHooks(t, storeSpec(store), nil)
	m := newMirror(t, map[string][]byte{"pt.zip": pt, "x86_64-test.zip": img, "x86_64-34_r14.zip": store})
	r := tempRoot(t)

	// not asked for: nothing about the store
	out := fetch(t, nil, r, opts(m))
	if out.err != nil {
		t.Fatal(out.err)
	}
	for _, q := range m.zipReqs() {
		if strings.Contains(q.path, "playstore") {
			t.Errorf("asked for %s", q.path)
		}
	}
	if len(*calls) != 0 || exists(filepath.Join(r.SDK(), root.PlayStoreDir)) || len(out.res.Components) != 2 {
		t.Fatalf("without PlayStore: calls %v, components %+v", *calls, out.res.Components)
	}

	o := opts(m)
	o.PlayStore = true
	out = fetch(t, nil, r, o)
	if out.err != nil {
		t.Fatal(out.err)
	}
	dir := filepath.Join(r.SDK(), root.PlayStoreDir)
	if got := readText(t, filepath.Join(dir, "Phonesky.apk")); got != storeAPK {
		t.Errorf("Phonesky.apk = %q", got)
	}
	if got := readText(t, filepath.Join(dir, "source.properties")); got != storeProps {
		t.Errorf("source.properties = %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 3 {
		t.Errorf("sdk\\playstore has %d entries, want the two files and source.properties", len(entries))
	}
	if len(*calls) != 1 || exists(filepath.Join(r.SDK(), ".staging")) {
		t.Errorf("calls %v, staging left %v", *calls, exists(filepath.Join(r.SDK(), ".staging")))
	}
	if entries, _ := os.ReadDir(downloadsDir(r)); len(entries) != 1 {
		t.Errorf("downloads %v", entries)
	}
	last := out.res.Components[len(out.res.Components)-1]
	if len(out.res.Components) != 3 || last.ID != "play-store" || !last.Downloaded || !last.Installed || last.Version != "14" ||
		!out.res.Components[0].Skipped {
		t.Errorf("components %+v", out.res.Components)
	}
	if !out.said("补全 Google 组件（adb、Android 14 系统镜像、Google Play 商店）") ||
		!strings.Contains(out.notices.String(), "接下来要下载的 Google Play 商店（测试） 由 Google 提供") {
		t.Errorf("messages %q / %q", out.infos, out.notices.String())
	}

	// again: complete, skipped
	n := len(m.all())
	if out = fetch(t, nil, r, o); out.err != nil || len(m.all()) != n || !out.res.Components[2].Skipped {
		t.Fatalf("rerun: %v %+v", out.err, out.res)
	}
}

// TestFetchPlayStoreRefuses: an extractor that fails, a zip without the system image and a file that does not match
// its pin are rejected; nothing goes into sdk\playstore and the staging is cleared. KeepDownloads keeps the zip
// under its own name.
func TestFetchPlayStoreRefuses(t *testing.T) {
	store := storeZip(t)
	setup(t)
	for _, c := range []struct {
		name  string
		zip   []byte
		fail  error
		spec  func(root.SDKComponent) root.SDKComponent
		wants string
	}{
		{"extract fails", store, errors.New("SHA-256 不对"), nil, "SHA-256 不对"},
		{"no system.img", makeZip(t, zentry{"x86_64/vendor.img", "vendor"}), nil, nil, "没有 x86_64/system.img"},
		{"pin", store, nil, func(s root.SDKComponent) root.SDKComponent {
			s.KeyHashes = map[string]string{"Phonesky.apk": sum256("another"), "cert.der": sum256("cert")}
			return s
		}, "校验失败"},
	} {
		spec := storeSpec(c.zip)
		if c.spec != nil {
			spec = c.spec(spec)
		}
		storeHooks(t, spec, c.fail)
		m := newMirror(t, map[string][]byte{"x86_64-34_r14.zip": c.zip})
		r := tempRoot(t)
		o := opts(m)
		o.PlayStore = true
		out := fetch(t, nil, r, o)
		wantCode(t, out.err, "components_rejected")
		if out.err == nil || !strings.Contains(out.err.Error(), c.wants) {
			t.Errorf("%s: %v", c.name, out.err)
		}
		if exists(filepath.Join(r.SDK(), root.PlayStoreDir)) || exists(filepath.Join(r.SDK(), ".staging")) {
			t.Errorf("%s: sdk\\playstore or the staging left", c.name)
		}
	}

	storeHooks(t, storeSpec(store), nil)
	m := newMirror(t, map[string][]byte{"x86_64-34_r14.zip": store})
	r := tempRoot(t)
	o := opts(m)
	o.PlayStore, o.KeepDownloads = true, true
	out := fetch(t, nil, r, o)
	if out.err != nil || !exists(filepath.Join(downloadsDir(r), "google_apis_playstore-x86_64-34_r14.zip")) ||
		exists(filepath.Join(downloadsDir(r), "x86_64-34_r14.zip")) {
		t.Fatalf("keep downloads: %v %v", out.err, out.res.Downloads)
	}
}
