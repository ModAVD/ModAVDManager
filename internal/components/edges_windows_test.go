package components

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
	"modavd/internal/testutil"
)

func TestAsideNameCollisionsAndError(t *testing.T) {
	base := filepath.Join(testutil.TempDir(t), "platform-tools")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := base + ".incomplete-20260102-030405"
	for _, suffix := range []string{"", "-2", "-3"} {
		got, err := asideName(base, now)
		if err != nil || got != want+suffix {
			t.Fatalf("%q %v want %s", got, err, want+suffix)
		}
		if err := os.Mkdir(got, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Invalid path gives the same non-ENOENT error for every candidate. This must return, not loop forever.
	done := make(chan error, 1)
	go func() { _, err := asideName(base+"\x00", now); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("invalid path accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("asideName loops on Lstat error")
	}
}

func TestSingleMirrorBadLocalPart(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	bad := append([]byte{}, pt...)
	bad[10] ^= 0xff
	putFile(t, filepath.Join(downloadsDir(r), "pt.zip.part"), string(bad))
	out := fetch(t, nil, r, opts(m))
	if out.err != nil || len(m.zipReqs()) != 1 || !out.warned("上次留下的 .part SHA-1 不对") {
		t.Fatalf("%v %+v %v", out.err, m.zipReqs(), out.warns)
	}
}

func TestLaterFailureReportsOldDirectory(t *testing.T) {
	pt, img := ptZip(t, adbBody), imgZip(t)
	setup(t, ptSpec(pt), imgSpec(img))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	putFile(t, filepath.Join(r.SDK(), "platform-tools", "old.txt"), "keep")
	o := opts(m)
	o.JSON = true
	out := fetch(t, nil, r, o)
	if out.err == nil {
		t.Fatal("missing second component accepted")
	}
	e := errs.As(out.err)
	// The CLI emits Advice in its JSON error. The path remains useful when no successful result is returned.
	b, _ := json.Marshal(map[string]string{"message": e.Message, "advice": e.Advice})
	matches, _ := filepath.Glob(filepath.Join(r.SDK(), "platform-tools.incomplete-*"))
	if len(matches) != 1 {
		t.Fatalf("asides %v", matches)
	}
	if !strings.Contains(string(b), filepath.Base(matches[0])) || readText(t, filepath.Join(matches[0], "old.txt")) != "keep" {
		t.Fatalf("%s", b)
	}
}

func TestDuplicateZipEntry(t *testing.T) {
	pt := ptZip(t, adbBody, zentry{"platform-tools/adb.exe", "replacement"})
	setup(t, ptSpec(pt))
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	r := tempRoot(t)
	out := fetch(t, nil, r, opts(m))
	if errs.CodeOf(out.err) != errs.ComponentsRejected || !strings.Contains(out.err.Error(), "出现了两次") {
		t.Fatal(out.err)
	}
	if _, err := os.Stat(filepath.Join(r.SDK(), "platform-tools")); !os.IsNotExist(err) {
		t.Fatal("duplicate archive installed")
	}
}

func TestSpaceSubtractsDownloadedBytes(t *testing.T) {
	setup(t)
	r := tempRoot(t)
	c := root.SDKComponent{URL: "file.zip", Size: 10 << 20, ExtractedBytes: 20 << 20}
	f := &fetcher{r: r, o: Options{Log: logx.Nop()}}
	for _, ext := range []string{".part", ""} {
		t.Run(ext, func(t *testing.T) {
			path := f.zipPath(c) + ext
			putFile(t, path, "")
			fh, err := os.OpenFile(path, os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			fh.Truncate(4 << 20)
			fh.Close()
			defer os.Remove(path)
			diskFree = func(string) (uint64, error) { return 526 << 20, nil }
			if err := f.checkSpace([]root.SDKComponent{c}); err != nil {
				t.Fatal(err)
			}
			diskFree = func(string) (uint64, error) { return (526 << 20) - 1, nil }
			if errs.CodeOf(f.checkSpace([]root.SDKComponent{c})) != errs.PrecheckFailed {
				t.Fatal("space boundary accepted")
			}
		})
	}
}

func TestRetryDelayAndCancel(t *testing.T) {
	pt := ptZip(t, adbBody)
	setup(t, ptSpec(pt))
	retryDelay = 3 * time.Second
	m := newMirror(t, map[string][]byte{"pt.zip": pt})
	var first time.Time
	var gap time.Duration
	tries := 0
	m.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool {
		tries++
		if tries == 1 {
			first = time.Now()
			w.WriteHeader(500)
			return true
		}
		gap = time.Since(first)
		return false
	}
	out := fetch(t, nil, tempRoot(t), opts(m))
	if out.err != nil || tries != 2 || gap < 3*time.Second || gap > 6*time.Second || !out.said("3 秒后重试（已下载的部分保留，接着下）") {
		t.Fatalf("%v tries=%d gap=%v", out.err, tries, gap)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m2 := newMirror(t, map[string][]byte{"pt.zip": pt})
	m2.serve = func(w http.ResponseWriter, r *http.Request, data []byte) bool { w.WriteHeader(500); return true }
	o := opts(m2)
	o.Log = logx.New(func(_ logx.Level, msg string) {
		if strings.Contains(msg, "3 秒后重试") {
			cancel()
		}
	})
	start := time.Now()
	_, err := Fetch(ctx, tempRoot(t), o)
	if errs.CodeOf(err) != errs.Canceled || len(m2.zipReqs()) != 1 || time.Since(start) > time.Second {
		t.Fatal(fmt.Sprint(err), len(m2.zipReqs()), time.Since(start))
	}
}
