//go:build windows

package update

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"modavd/internal/testutil"
)

// bigFiles are n incompressible files of size bytes under sdk/emulator/lib (the bulk of a package that rarely changes).
func bigFiles(seed uint64, n, size int) map[string]string {
	out := map[string]string{}
	rng := rand.New(rand.NewPCG(seed, 7))
	for k := 0; k < n; k++ {
		b := make([]byte, size)
		for i := range b {
			b[i] = byte(rng.Uint32())
		}
		out[fmt.Sprintf("sdk/emulator/lib/big%d.dll", k)] = string(b)
	}
	return out
}

func merged(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// dlLog records the package requests: their Range headers and the body bytes sent.
type dlLog struct {
	mu     sync.Mutex
	ranges []string
	bytes  int64
}

type countingWriter struct {
	http.ResponseWriter
	log *dlLog
}

func (w countingWriter) Write(b []byte) (int, error) {
	n, err := w.ResponseWriter.Write(b)
	w.log.mu.Lock()
	w.log.bytes += int64(n)
	w.log.mu.Unlock()
	return n, err
}

// serve serves body (honouring Range) and logs the request; rangeBody, when set, is what range requests get.
func (l *dlLog) serve(body, rangeBody []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		l.ranges = append(l.ranges, r.Header.Get("Range"))
		l.mu.Unlock()
		b := body
		if rangeBody != nil && r.Header.Get("Range") != "" {
			b = rangeBody
		}
		http.ServeContent(countingWriter{w, l}, r, "pkg.zip", zipTime, bytes.NewReader(b))
	}
}

// fullGets is the number of package requests without a bounded range (a whole download or a resume to the end).
func (l *dlLog) fullGets() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, r := range l.ranges {
		if r == "" || strings.HasSuffix(r, "-") {
			n++
		}
	}
	return n
}

// treeOf is every regular file under dir by "/" path.
func treeOf(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	if err := walkFiles(dir, func(rel string, _ os.FileInfo) {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		out[rel] = b
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t *testing.T, got, want map[string][]byte) {
	t.Helper()
	for rel, b := range want {
		if g, ok := got[rel]; !ok || !bytes.Equal(g, b) {
			t.Errorf("%s differs (present %v)", rel, ok)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s should not be there", rel)
		}
	}
}

// rangeCase is a root of build 36 with ten large files and a package of build 37 that keeps them.
type rangeCase struct {
	big      map[string]string
	newFiles map[string]string
	zip      []byte
}

func newRangeCase(t *testing.T) rangeCase {
	big := bigFiles(1, 10, 300<<10)
	nf := pkgFiles(37, merged(big, map[string]string{"tools/new.ps1": "new in 37"}))
	return rangeCase{big, nf, zipOf(t, 37, nf, false)}
}

func stagedMode(u *Updater) string {
	s, _ := u.l.readState().Get("staged").Obj().Get("mode").Str()
	return s
}

// TestRangeFetchesOnlyChanged: with a server that serves ranges only the changed entries are fetched (a small part
// of the package, never the whole), pkg\ comes out byte for byte as unpacking the whole package leaves it, progress is
// reported, state.json says range, and the install brings the root to the new version.
func TestRangeFetchesOnlyChanged(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	c := newRangeCase(t)
	r := writeRoot(t, 36, merged(c.big, map[string]string{"tools/old.ps1": "dropped in 37"}))
	f := newSources(t, 37, c.zip)
	log := &dlLog{}
	f.dl = log.serve(c.zip, nil)
	u := f.updater(r)
	var progress []Progress
	u.Opt.Progress = func(p Progress) { progress = append(progress, p) }
	ctx := context.Background()

	rel, err := u.githubLatest(ctx)
	if err != nil || !u.rangeAllowed(rel) {
		t.Fatalf("release %+v %v", rel, err)
	}
	files, verified, err := u.stageByRange(ctx, rel)
	if err != nil || verified != "manifest" || files != len(c.newFiles)+1 {
		t.Fatalf("stageByRange: %d %q %v", files, verified, err)
	}
	if log.fullGets() != 0 || log.bytes*10 > int64(len(c.zip)) {
		t.Fatalf("fetched %d of %d bytes, requests %q", log.bytes, len(c.zip), log.ranges)
	}
	// the same package unpacked whole
	other := layout{root: testutil.TempDir(t)}
	archive := filepath.Join(other.root, "pkg.zip")
	os.WriteFile(archive, c.zip, 0o644)
	if _, err := other.extractPackage(archive, "", nil); err != nil {
		t.Fatal(err)
	}
	sameTree(t, treeOf(t, u.l.pkg()), treeOf(t, other.pkg()))
	var last *Progress
	for k := range progress {
		if progress[k].Stage == "downloading" && progress[k].Total > 0 {
			last = &progress[k]
		}
	}
	if last == nil || last.Percent != 100 || last.Done != last.Total || last.Total*10 > int64(len(c.zip)) {
		t.Fatalf("progress %+v", progress)
	}

	st, err := u.Prepare(ctx)
	if err != nil || st.Phase != phaseStaged || len(st.Notices) != 0 || stagedMode(u) != "range" {
		t.Fatalf("prepare: %+v %v (mode %q)", st, err, stagedMode(u))
	}
	if st, err = u.Install(ctx); err != nil || !st.Updated || st.CurrentVersion != "v0.37.0" {
		t.Fatalf("install: %+v %v", st, err)
	}
	for p, body := range c.newFiles {
		if read(t, r, p) != body {
			t.Errorf("%s not updated", p)
		}
	}
	if lexists(filepath.Join(r.Dir, "tools", "old.ps1")) {
		t.Error("a file the new package dropped is still there")
	}
	if log.fullGets() != 0 {
		t.Fatalf("whole package downloaded: %q", log.ranges)
	}
}

// forgeCRC is data with its last 4 bytes changed so that its CRC32 is want (CRC32 is linear, so any 4 bytes can be
// fixed up).
func forgeCRC(data []byte, want uint32) []byte {
	tab := crc32.MakeTable(crc32.IEEE)
	var top [256]byte
	for i := 0; i < 256; i++ {
		top[tab[i]>>24] = byte(i)
	}
	prefix := data[:len(data)-4]
	var idx [4]byte
	reg := ^want
	for k := 3; k >= 0; k-- {
		i := top[reg>>24]
		idx[k] = i
		reg = (reg ^ tab[i]) << 8
	}
	reg = ^crc32.ChecksumIEEE(prefix)
	out := append([]byte{}, prefix...)
	for k := 0; k < 4; k++ {
		b := byte(reg) ^ idx[k]
		out = append(out, b)
		reg = tab[byte(reg)^b] ^ (reg >> 8)
	}
	return out
}

// TestRangeFallsBack: whenever the range path cannot be trusted or is not worth it, the whole package is downloaded,
// the notice says why, state.json says full, and the root still ends up exactly at the new version.
func TestRangeFallsBack(t *testing.T) {
	cases := []struct {
		name   string
		notice string
		setup  func(t *testing.T, c *rangeCase, f *fakeSources, log *dlLog, local map[string]string)
	}{
		{"server ignores Range", "没按区间回", func(t *testing.T, c *rangeCase, f *fakeSources, log *dlLog, _ map[string]string) {
			f.dl = func(w http.ResponseWriter, r *http.Request) {
				log.mu.Lock()
				log.ranges = append(log.ranges, r.Header.Get("Range"))
				log.mu.Unlock()
				w.Header().Set("Content-Length", fmt.Sprint(len(c.zip)))
				w.WriteHeader(http.StatusOK)
				w.Write(c.zip)
			}
		}},
		{"too much changed", "变了的文件太多", func(t *testing.T, c *rangeCase, f *fakeSources, log *dlLog, _ map[string]string) {
			big := bigFiles(2, 10, 300<<10)
			c.newFiles = pkgFiles(37, merged(big, map[string]string{"tools/new.ps1": "new in 37"}))
			c.zip = zipOf(t, 37, c.newFiles, false)
			f.zip = c.zip
			f.dl = log.serve(c.zip, nil)
		}},
		{"fetched data wrong", "取回的", func(t *testing.T, c *rangeCase, f *fakeSources, log *dlLog, _ map[string]string) {
			zr, _ := zip.NewReader(bytes.NewReader(c.zip), int64(len(c.zip)))
			bad := append([]byte{}, c.zip...)
			for _, zf := range zr.File {
				if zf.Name == "tools/new.ps1" {
					off, _ := zf.DataOffset()
					bad[off+int64(zf.CompressedSize64)/2] ^= 0x55
				}
			}
			f.dl = log.serve(c.zip, bad)
		}},
		{"assembled package fails the manifest", "核对不过", func(t *testing.T, c *rangeCase, f *fakeSources, log *dlLog, local map[string]string) {
			// the root's big0 has the package's size and CRC32 but other bytes: only the manifest's SHA-256 notices
			p := "sdk/emulator/lib/big0.dll"
			want := c.big[p]
			fake := []byte(strings.Repeat("x", len(want)))
			fake = forgeCRC(fake, crc32.ChecksumIEEE([]byte(want)))
			if crc32.ChecksumIEEE(fake) != crc32.ChecksumIEEE([]byte(want)) {
				t.Fatal("forgeCRC")
			}
			local[p] = string(fake)
			f.dl = log.serve(c.zip, nil)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withSettings(t, SourceGitHub, "")
			c := newRangeCase(t)
			f := newSources(t, 37, c.zip)
			log := &dlLog{}
			local := merged(c.big)
			tc.setup(t, &c, f, log, local)
			f.digest = "sha256:" + hashBytes(c.zip)
			r := writeRoot(t, 36, local)
			u := f.updater(r)
			ctx := context.Background()
			st, err := u.Prepare(ctx)
			if err != nil || st.Phase != phaseStaged || stagedMode(u) != "full" || len(st.Notices) != 1 ||
				!strings.Contains(st.Notices[0], tc.notice) || !strings.Contains(st.Notices[0], "改下整包") {
				t.Fatalf("prepare: %+v %v (mode %q)", st, err, stagedMode(u))
			}
			if log.fullGets() != 1 {
				t.Fatalf("want one whole download: %q", log.ranges)
			}
			if st, err = u.Install(ctx); err != nil || !st.Updated || st.CurrentVersion != "v0.37.0" {
				t.Fatalf("install: %+v %v", st, err)
			}
			for p, body := range c.newFiles {
				if read(t, r, p) != body {
					t.Errorf("%s not updated", p)
				}
			}
		})
	}
}

// TestRangeSkippedWithDownload: a whole download already there (here half of it) is used and resumed; no range attempt.
func TestRangeSkippedWithDownload(t *testing.T) {
	withSettings(t, SourceGitHub, "")
	c := newRangeCase(t)
	r := writeRoot(t, 36, c.big)
	f := newSources(t, 37, c.zip)
	log := &dlLog{}
	f.dl = log.serve(c.zip, nil)
	u := f.updater(r)
	part := u.l.downloadAt("ModAVD-0.37.0-win-x64.zip") + ".part"
	os.MkdirAll(filepath.Dir(part), 0o755)
	os.WriteFile(part, c.zip[:len(c.zip)/2], 0o644)
	st, err := u.Prepare(context.Background())
	if err != nil || st.Phase != phaseStaged || stagedMode(u) != "full" || len(st.Notices) != 0 {
		t.Fatalf("prepare: %+v %v (mode %q)", st, err, stagedMode(u))
	}
	if len(log.ranges) != 1 || log.ranges[0] != fmt.Sprintf("bytes=%d-", len(c.zip)/2) {
		t.Fatalf("requests %q", log.ranges)
	}
}

// TestRangeAllowed: only GitHub's full package of a version with a manifest, and only with no download of it there.
func TestRangeAllowed(t *testing.T) {
	r := writeRoot(t, 36, nil)
	u := New(r, nil, Options{})
	gh := func() *release {
		return &release{source: SourceGitHub, tag: "v0.37.0", assetName: "ModAVD-0.37.0-win-x64.zip", url: "https://x", size: 10}
	}
	if !u.rangeAllowed(gh()) {
		t.Fatal("GitHub package refused")
	}
	for name, change := range map[string]func(*release){
		"mirrorchyan": func(x *release) { x.source = SourceMirrorChyan },
		"incremental": func(x *release) { x.incremental = true },
		"legacy tag":  func(x *release) { x.tag = "v37.2.10-mas32" },
		"no size":     func(x *release) { x.size = 0 },
	} {
		x := gh()
		change(x)
		if u.rangeAllowed(x) {
			t.Errorf("%s allowed", name)
		}
	}
	os.MkdirAll(u.l.download(), 0o755)
	os.WriteFile(u.l.downloadAt("ModAVD-0.37.0-win-x64.zip"), []byte("x"), 0o644)
	if u.rangeAllowed(gh()) {
		t.Error("allowed with a finished download")
	}
}

// TestRangeLimits: AUTO-MAS's thresholds, 40% of the package or 3000 entries.
func TestRangeLimits(t *testing.T) {
	for _, c := range []struct {
		entries     int
		bytes, size int64
		want        bool
	}{
		{3000, 40, 100, false}, {3001, 1, 100, true}, {1, 41, 100, true}, {0, 0, 100, false},
		{2999, 400 << 20, 1000 << 20, false}, {1, 400<<20 + 1, 1000 << 20, true},
	} {
		if got := rangeTooMuch(c.entries, c.bytes, c.size); got != c.want {
			t.Errorf("rangeTooMuch(%d, %d, %d) = %v", c.entries, c.bytes, c.size, got)
		}
	}
}

// TestRangeGroups: entries close together are one request, far apart or past the group size are separate.
func TestRangeGroups(t *testing.T) {
	e := func(start, length int64) rangeEntry { return rangeEntry{start: start, length: length} }
	g := rangeGroups([]rangeEntry{e(5<<20, 100), e(0, 100), e(100+rangeMergeGap, 10), e(5<<20+200, rangeMaxGroup)})
	if len(g) != 3 || g[0].start != 0 || g[0].length != 110+rangeMergeGap || len(g[0].members) != 2 ||
		g[1].start != 5<<20 || g[1].length != 100 || g[2].start != 5<<20+200 {
		t.Fatalf("%+v", g)
	}
}
