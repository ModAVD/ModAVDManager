package components

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/playstore"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

const (
	storeImage = "the Play image's system.img"
	storeAPK   = "Phonesky.apk from the test image"
	storeCert  = "cert"
	storeProps = "Pkg.Revision=14\n"
)

// storeFiles is the test table of the store's files: two, of different sizes, as the real five are.
func storeFiles() []playstore.File {
	return []playstore.File{
		{Name: "Phonesky.apk", Partition: "product", Path: "priv-app/Phonesky/Phonesky.apk", Size: int64(len(storeAPK)), SHA256: sum256(storeAPK)},
		{Name: "cert.der", Partition: "product", Path: "etc/cert.der", Size: int64(len(storeCert)), SHA256: sum256(storeCert)},
	}
}

// storeSpec is the Google Play Store component as in the real table, with the test files: the zip at the Play
// image's URL (whose last part is the Google APIs image's), its own name in downloads\, the files pinned.
func storeSpec(data []byte) root.SDKComponent {
	c := root.SDKComponent{
		ID: "play-store", Name: root.PlayStoreName, FetchName: "Google Play 商店（测试）", Version: "14",
		Target: root.PlayStoreDir, URL: "sys-img/google_apis_playstore/x86_64-34_r14.zip",
		ZipName: "google_apis_playstore-x86_64-34_r14.zip", Size: int64(len(data)), SHA1: sum1(data), ArchiveRoot: "x86_64",
		KeyHashes: map[string]string{}, ExtractedBytes: 1 << 20, Properties: storeProps,
	}
	for _, f := range storeFiles() {
		c.StatusFiles = append(c.StatusFiles, f.Name)
		c.KeyFiles = append(c.KeyFiles, f.Name)
		c.KeyHashes[f.Name] = f.SHA256
	}
	return c
}

func storeZip(t *testing.T) []byte {
	t.Helper()
	// system.img not first: only that entry is unpacked
	return makeZip(t, zentry{"x86_64/vendor.img", "vendor"}, zentry{"x86_64/system.img", storeImage},
		zentry{"x86_64/kernel-ranchu", "kernel"})
}

// storeHooks puts the test store component and its files in the catalogue, and an extractor that checks it is given
// the unpacked system.img (and nothing else of the zip beside it) and writes the files, or fails with fail.
func storeHooks(t *testing.T, spec root.SDKComponent, fail error) *[]string {
	t.Helper()
	oldCat, oldExtract, oldFiles := playStoreCatalogue, playStoreExtract, playStoreFiles
	t.Cleanup(func() { playStoreCatalogue, playStoreExtract, playStoreFiles = oldCat, oldExtract, oldFiles })
	playStoreCatalogue = func() root.SDKComponent { return spec }
	playStoreFiles = storeFiles()
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
		if !slices.Equal(files, storeFiles()) {
			t.Errorf("extract asked for %+v", files)
		}
		if fail != nil {
			return fail
		}
		putFile(t, filepath.Join(dir, "Phonesky.apk"), storeAPK)
		putFile(t, filepath.Join(dir, "cert.der"), storeCert)
		return nil
	}
	return &calls
}

// entries are the names in dir, sorted; nil when it is not there.
func entries(dir string) []string {
	es, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func storeDir(r *root.Root) string { return filepath.Join(r.SDK(), root.PlayStoreDir) }

// TestFetchPlayStore: with PlayStore the store comes after the required components: its zip under its own name,
// only the system image unpacked and handed to the extractor; afterwards sdk\playstore holds only the files and
// source.properties (which says download), and the image, the zip and sdk\.staging are gone. The next run skips it.
// Without PlayStore it is neither asked for nor touched.
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
	if len(*calls) != 0 || exists(storeDir(r)) || len(out.res.Components) != 2 {
		t.Fatalf("without PlayStore: calls %v, components %+v", *calls, out.res.Components)
	}
	if st := PlayStoreState(r); st.Ready || st.Source != "" || len(st.Files) != 2 || st.Files[0].Present {
		t.Errorf("state before: %+v", st)
	}

	o := opts(m)
	o.PlayStore = true
	out = fetch(t, nil, r, o)
	if out.err != nil {
		t.Fatal(out.err)
	}
	dir := storeDir(r)
	if got := readText(t, filepath.Join(dir, "Phonesky.apk")); got != storeAPK {
		t.Errorf("Phonesky.apk = %q", got)
	}
	if got := readText(t, filepath.Join(dir, "source.properties")); got != storeProps+"ModAVD.Source=download\n" {
		t.Errorf("source.properties = %q", got)
	}
	if got := entries(dir); !slices.Equal(got, []string{"Phonesky.apk", "cert.der", "source.properties"}) {
		t.Errorf("sdk\\playstore holds %v, want the files and source.properties", got)
	}
	if len(*calls) != 1 || exists(filepath.Join(r.SDK(), ".staging")) {
		t.Errorf("calls %v, staging left %v", *calls, exists(filepath.Join(r.SDK(), ".staging")))
	}
	if got := entries(downloadsDir(r)); !slices.Equal(got, []string{licenseFile}) {
		t.Errorf("downloads %v", got)
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
	if st := PlayStoreState(r); !st.Ready || st.Source != PlayStoreFromDownload || st.Version != "14" || !st.Files[1].Present ||
		st.Files[0].Guest != "/product/priv-app/Phonesky/Phonesky.apk" {
		t.Errorf("state after: %+v", st)
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
			s.KeyHashes = map[string]string{"Phonesky.apk": sum256("another"), "cert.der": sum256(storeCert)}
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
		if exists(storeDir(r)) || exists(filepath.Join(r.SDK(), ".staging")) {
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

// TestFetchPlayStoreBadZip: a Play image zip of the right size whose SHA-1 is wrong (on every mirror) is refused and
// not kept: no zip, no .part in downloads\, nothing unpacked or extracted, no sdk\playstore.
func TestFetchPlayStoreBadZip(t *testing.T) {
	store := storeZip(t)
	setup(t)
	calls := storeHooks(t, storeSpec(store), nil)
	bad := append([]byte(nil), store...)
	bad[len(bad)/2] ^= 0xFF
	m := newMirror(t, map[string][]byte{"x86_64-34_r14.zip": bad})
	r := tempRoot(t)
	o := opts(m)
	o.PlayStore = true
	out := fetch(t, nil, r, o)
	wantCode(t, out.err, "components_rejected")
	for _, e := range entries(downloadsDir(r)) {
		if e != licenseFile {
			t.Errorf("downloads\\ keeps %s", e)
		}
	}
	if len(*calls) != 0 || exists(storeDir(r)) || exists(filepath.Join(r.SDK(), ".staging")) {
		t.Errorf("calls %v, sdk\\playstore %v", *calls, exists(storeDir(r)))
	}
}

// ---- import ------------------------------------------------------------------------------------------------------

// userFiles writes files (name -> body) into a fresh folder of the user's and returns their ImportFiles.
func userFiles(t *testing.T, files ...[2]string) []ImportFile {
	t.Helper()
	dir := testutil.TempDir(t)
	var out []ImportFile
	for _, f := range files {
		p := filepath.Join(dir, f[0])
		putFile(t, p, f[1])
		out = append(out, ImportFile{Path: p})
	}
	return out
}

func doImport(t *testing.T, r *root.Root, files []ImportFile) (*PlayStoreImportResult, error) {
	t.Helper()
	return ImportPlayStore(context.Background(), r, files, Options{})
}

func reasonOf(err error) string { return errs.DetailsOf(err)[errs.Reason] }

// TestImportPlayStoreFiles: the files themselves, under other names, go in under theirs, checked; source.properties
// says import; the user's files stay as they were. Importing again finds them complete and changes nothing.
func TestImportPlayStoreFiles(t *testing.T) {
	setup(t)
	calls := storeHooks(t, storeSpec(storeZip(t)), nil)
	r := tempRoot(t)
	given := userFiles(t, [2]string{"store (1).apk", storeAPK}, [2]string{"c.bin", storeCert})
	res, err := doImport(t, r, given)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "files" || res.Skipped || !slices.Equal(res.Used, []string{"store (1).apk", "c.bin"}) || len(res.Kept) != 0 ||
		!res.Status.Ready || res.Status.Source != PlayStoreFromImport {
		t.Errorf("result %+v", res)
	}
	dir := storeDir(r)
	if got := entries(dir); !slices.Equal(got, []string{"Phonesky.apk", "cert.der", "source.properties"}) {
		t.Errorf("sdk\\playstore holds %v", got)
	}
	if readText(t, filepath.Join(dir, "cert.der")) != storeCert || !strings.Contains(readText(t, filepath.Join(dir, "source.properties")), "ModAVD.Source=import") {
		t.Error("files in sdk\\playstore")
	}
	for k, body := range []string{storeAPK, storeCert} {
		if readText(t, given[k].Path) != body {
			t.Errorf("%s changed", given[k].Path)
		}
	}
	if len(*calls) != 0 || exists(filepath.Join(r.SDK(), ".staging")) || exists(downloadsDir(r)) {
		t.Errorf("calls %v, staging or downloads\\ left", *calls)
	}
	if !strings.Contains(logText(t, r), "用自己的文件补全 Google Play 商店") {
		t.Error("no log")
	}

	res, err = doImport(t, r, given[:1])
	if err != nil || !res.Skipped || !res.Status.Ready {
		t.Fatalf("again: %+v %v", res, err)
	}
}

// TestImportPlayStoreZip: Google's zip (any name) is checked against the size and SHA-1 and read like a download:
// only its system image unpacked into the staging and handed to the extractor, the files go in, nothing is left in
// the staging or downloads\, and the user's zip stays.
func TestImportPlayStoreZip(t *testing.T) {
	setup(t)
	store := storeZip(t)
	calls := storeHooks(t, storeSpec(store), nil)
	r := tempRoot(t)
	given := userFiles(t, [2]string{"play.zip", string(store)})
	res, err := doImport(t, r, given)
	if err != nil {
		t.Fatal(err)
	}
	if res.From != "zip" || !res.Status.Ready || res.Status.Source != PlayStoreFromImport || len(*calls) != 1 {
		t.Errorf("result %+v, calls %v", res, *calls)
	}
	if got := entries(storeDir(r)); !slices.Equal(got, []string{"Phonesky.apk", "cert.der", "source.properties"}) {
		t.Errorf("sdk\\playstore holds %v", got)
	}
	if exists(filepath.Join(r.SDK(), ".staging")) || exists(downloadsDir(r)) || readText(t, given[0].Path) != string(store) {
		t.Error("staging or downloads\\ left, or the zip changed")
	}
}

// TestImportPlayStoreRefuses: a file that is neither the zip nor one of the files (by size), one whose SHA-256 or
// SHA-1 does not match, and a set that misses one are refused with their reasons; nothing is extracted and
// sdk\playstore stays as it was (absent, or complete with the right files).
func TestImportPlayStoreRefuses(t *testing.T) {
	setup(t)
	store := storeZip(t)
	calls := storeHooks(t, storeSpec(store), nil)
	badZip := append([]byte(nil), store...)
	badZip[10] ^= 0xFF
	forged := strings.Repeat("x", len(storeAPK)) // the store's size, not its content
	r := tempRoot(t)
	for _, c := range []struct {
		name   string
		files  [][2]string
		reason string
		wants  string
	}{
		{"unknown", [][2]string{{"Phonesky.apk", storeAPK + "!"}, {"cert.der", storeCert}}, ImportReasonUnknown, "Phonesky.apk"},
		{"forged file", [][2]string{{"Phonesky.apk", forged}, {"cert.der", storeCert}}, ImportReasonMismatch, "SHA-256"},
		{"forged file after a good one", [][2]string{{"cert.der", storeCert}, {"a.apk", forged}}, ImportReasonMismatch, "a.apk"},
		{"forged zip", [][2]string{{"x86_64-34_r14.zip", string(badZip)}}, ImportReasonMismatch, "SHA-1"},
		{"good zip, forged file", [][2]string{{"x86_64-34_r14.zip", string(store)}, {"p.apk", forged}}, ImportReasonMismatch, "p.apk"},
		{"one missing", [][2]string{{"Phonesky.apk", storeAPK}}, ImportReasonIncomplete, "cert.der"},
	} {
		_, err := doImport(t, r, userFiles(t, c.files...))
		if errs.CodeOf(err) != errs.ComponentsRejected || reasonOf(err) != c.reason || !strings.Contains(err.Error(), c.wants) {
			t.Errorf("%s: %v %v", c.name, err, errs.DetailsOf(err))
		}
		if exists(storeDir(r)) || exists(filepath.Join(r.SDK(), ".staging")) || len(*calls) != 0 {
			t.Fatalf("%s: sdk\\playstore %v, calls %v", c.name, entries(storeDir(r)), *calls)
		}
	}

	// complete already: a forged file is still refused, and the right files stay
	if _, err := doImport(t, r, userFiles(t, [2]string{"a.apk", storeAPK}, [2]string{"c", storeCert})); err != nil {
		t.Fatal(err)
	}
	if _, err := doImport(t, r, userFiles(t, [2]string{"a.apk", forged})); reasonOf(err) != ImportReasonMismatch {
		t.Errorf("forged over complete: %v", err)
	}
	if readText(t, filepath.Join(storeDir(r), "Phonesky.apk")) != storeAPK {
		t.Error("the store's file was replaced")
	}

	if _, err := doImport(t, r, nil); errs.CodeOf(err) != errs.Usage {
		t.Errorf("no file: %v", err)
	}
	if _, err := doImport(t, r, []ImportFile{{Path: filepath.Join(testutil.TempDir(t), "gone.apk")}}); errs.CodeOf(err) != errs.NotFound {
		t.Errorf("a file that is not there: %v", err)
	}
	if _, err := doImport(t, r, []ImportFile{{Path: testutil.TempDir(t)}}); reasonOf(err) != ImportReasonUnknown {
		t.Errorf("a directory: %v", err)
	}
}

// TestImportPlayStoreKeeps: files not given are taken from sdk\playstore when they are right; the old directory,
// incomplete, is set aside.
func TestImportPlayStoreKeeps(t *testing.T) {
	setup(t)
	storeHooks(t, storeSpec(storeZip(t)), nil)
	r := tempRoot(t)
	putFile(t, filepath.Join(storeDir(r), "cert.der"), storeCert)
	putFile(t, filepath.Join(storeDir(r), "Phonesky.apk"), "half")
	res, err := doImport(t, r, userFiles(t, [2]string{"p.apk", storeAPK}))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Kept, []string{"cert.der"}) || !strings.Contains(res.SetAside, ".incomplete-") || !res.Status.Ready {
		t.Errorf("result %+v", res)
	}
	if !exists(filepath.Join(r.Dir, res.SetAside)) {
		t.Errorf("set aside %s is not there", res.SetAside)
	}
}

// TestImportPlayStoreRoot: only on a ModAVD root, one import or fetch at a time.
func TestImportPlayStoreRoot(t *testing.T) {
	setup(t)
	storeHooks(t, storeSpec(storeZip(t)), nil)
	given := userFiles(t, [2]string{"p.apk", storeAPK}, [2]string{"c", storeCert})
	notRoot, err := root.New(testutil.TempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doImport(t, notRoot, given); errs.CodeOf(err) != errs.NotReady {
		t.Errorf("not a root: %v", err)
	}
	r := tempRoot(t)
	if err := os.MkdirAll(r.LogsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	l, ok, err := host.TryLock(filepath.Join(r.LogsDir(), "fetch-components.lock"))
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	_, err = doImport(t, r, given)
	l.Unlock()
	if errs.CodeOf(err) != errs.Busy || exists(storeDir(r)) {
		t.Errorf("while a fetch runs: %v", err)
	}
	fetchGOOS = "linux"
	if _, err := doImport(t, r, given); errs.CodeOf(err) != errs.Unsupported {
		t.Errorf("linux: %v", err)
	}
}

// TestPlayStoreImportFiles: a directory gives the files in it named like the store's, a file itself; a directory
// without any of them and a path that is not there are refused.
func TestPlayStoreImportFiles(t *testing.T) {
	storeHooks(t, storeSpec(nil), nil)
	dir := testutil.TempDir(t)
	putFile(t, filepath.Join(dir, "cert.der"), storeCert)
	putFile(t, filepath.Join(dir, "notes.txt"), "x")
	zip := filepath.Join(testutil.TempDir(t), "x.zip")
	putFile(t, zip, "zip")
	got, err := PlayStoreImportFiles([]string{dir, zip})
	if err != nil || len(got) != 2 || got[0].Path != filepath.Join(dir, "cert.der") || got[1].Path != zip {
		t.Fatalf("%+v %v", got, err)
	}
	empty := testutil.TempDir(t)
	if _, err := PlayStoreImportFiles([]string{empty}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("empty folder: %v", err)
	}
	if _, err := PlayStoreImportFiles([]string{filepath.Join(empty, "gone")}); errs.CodeOf(err) != errs.NotFound {
		t.Errorf("gone: %v", err)
	}
}
