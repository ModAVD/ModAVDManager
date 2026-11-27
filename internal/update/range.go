package update

// GitHub's full package, but only the files that changed (AUTO-MAS update_range.py, with the same thresholds): read
// the zip's central directory with HTTP Range requests, compare every entry with the root's file at the same path by
// CRC32 and size, fetch only the entries that differ and copy the rest from the root, so that pkg\ ends up exactly as
// unpacking the whole package would leave it. The manifest check that follows is the same as for a full download.
//
// archive/zip reads the package through remoteZip, an io.ReaderAt that holds only the pieces fetched so far. Anything
// that makes the range result unusable (unsupported ranges, too much changed, a bad CRC or manifest) returns a
// *rangeUnavailable: the caller drops pkg\ and downloads the whole package instead. Local creation, write and close errors stop the update.

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"modavd/internal/download"
	"modavd/internal/errs"
	"modavd/internal/root"
)

const (
	// rangeMaxFetchRatio, rangeMaxFetchEntries: fetching more than this share of the package, or more entries, is not
	// worth it; download the whole package.
	rangeMaxFetchRatio   = 0.4
	rangeMaxFetchEntries = 3000
	// rangeMergeGap, rangeMaxGroup: entries less than rangeMergeGap apart are fetched with one request, up to
	// rangeMaxGroup bytes per request (a single larger entry is a request of its own).
	rangeMergeGap = 1 << 20
	rangeMaxGroup = 8 << 20
	// rangeHeaderSlack: what a local file header may carry beyond the central directory's name and extra field (a
	// longer extra field, the data descriptor after the data).
	rangeHeaderSlack = 1024
	localHeaderLen   = 30
	// rangeMaxDirectory: the central directory (with the end record) is read in one request; one this large is not a
	// ModAVD package.
	rangeMaxDirectory = 32 << 20
)

// rangeUnavailable: download the whole package. Only failed attempts, not normal threshold decisions, add a notice.
type rangeUnavailable struct {
	reason string
	normal bool // a threshold decision, not a failed range attempt
}

func (e *rangeUnavailable) Error() string { return e.reason }

func unavailable(format string, args ...any) *rangeUnavailable {
	return &rangeUnavailable{reason: fmt.Sprintf(format, args...)}
}

// segment is a piece of the remote package that has been fetched.
type segment struct {
	start, length int64
	reader        io.ReaderAt
}

// remoteZip is the remote package as an io.ReaderAt for archive/zip: reads are served from the fetched segments. While
// fill is set (reading the central directory) a read of bytes not fetched yet fetches the whole gap up to the next
// segment; otherwise such a read fails (the span of an entry was underestimated). While probing, the offset of the
// first read is recorded.
type remoteZip struct {
	size    int64
	segs    []segment
	fill    func(off, n int64) ([]byte, error)
	probing bool
	probed  int64
}

var errNotFetched = errors.New("读到了没取回的位置")

func (r *remoteZip) find(pos int64) *segment {
	for k := range r.segs {
		s := &r.segs[k]
		if s.start <= pos && pos < s.start+s.length {
			return s
		}
	}
	return nil
}

// gapEnd is where the bytes not fetched from pos on end: the next segment's start, or the end of the package.
func (r *remoteZip) gapEnd(pos int64) int64 {
	end := r.size
	for _, s := range r.segs {
		if s.start > pos && s.start < end {
			end = s.start
		}
	}
	return end
}

func (r *remoteZip) add(start int64, data []byte) {
	r.segs = append(r.segs, segment{start, int64(len(data)), bytes.NewReader(data)})
}

func (r *remoteZip) drop(start int64) {
	for k, s := range r.segs {
		if s.start == start {
			r.segs = append(r.segs[:k], r.segs[k+1:]...)
			return
		}
	}
}

func (r *remoteZip) ReadAt(p []byte, off int64) (int, error) {
	if r.probing && r.probed < 0 {
		r.probed = off
	}
	if off < 0 {
		return 0, errNotFetched
	}
	n := 0
	for n < len(p) {
		pos := off + int64(n)
		if pos >= r.size {
			return n, io.EOF
		}
		s := r.find(pos)
		if s == nil {
			if r.fill == nil {
				return n, fmt.Errorf("%w（%d）", errNotFetched, pos)
			}
			end := r.gapEnd(pos)
			data, err := r.fill(pos, end-pos)
			if err != nil {
				return n, err
			}
			r.add(pos, data)
			continue
		}
		want := min(int64(len(p)-n), s.length-(pos-s.start))
		got, err := s.reader.ReadAt(p[n:n+int(want)], pos-s.start)
		n += got
		if err != nil {
			return n, err
		}
		if int64(got) != want {
			return n, io.ErrUnexpectedEOF
		}
	}
	return n, nil
}

// rangeEntry is an entry to fetch: where its local header starts and how far its data may reach.
type rangeEntry struct {
	zipEntry
	start, length int64
}

// rangePlan is what to fetch and what to copy from the root.
type rangePlan struct {
	remote *remoteZip
	fetch  []rangeEntry
	copy   []zipEntry
}

func (p *rangePlan) fetchBytes() int64 {
	var n int64
	for _, e := range p.fetch {
		n += e.length
	}
	return n
}

// sameAsLocal: the root's file at e.rel is a regular file of the entry's size and CRC32.
func (l layout) sameAsLocal(e zipEntry) bool {
	p := l.at(e.rel)
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || uint64(st.Size()) != e.f.UncompressedSize64 {
		return false
	}
	f, err := root.OpenShared(p)
	if err != nil {
		return false
	}
	defer f.Close()
	h := crc32.NewIEEE()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return h.Sum32() == e.f.CRC32
}

// rangeRead is bounded by rangeMaxDirectory for metadata and rangeMaxGroup for small entry groups.
func (u *Updater) rangeRead(ctx context.Context, r *release, off, n int64, what string) ([]byte, error) {
	d := download.Client{HTTP: u.client(), StallTimeout: u.stallTimeout}
	var data []byte
	err := u.rangeRetry(ctx, off, n, what, func() (err error) {
		data, err = d.Range(ctx, r.url, off, n, r.size, what)
		return err
	})
	return data, err
}

// rangeRetry retries only transport failures. Local writer, seek and truncate errors stop immediately.
func (u *Updater) rangeRetry(ctx context.Context, off, n int64, what string, fetch func() error) error {
	var last error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		err := fetch()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
		var de *download.Error
		if !errors.As(err, &de) {
			return err
		}
		last = err
		if de.Kind == download.NoRange || de.Kind == download.Size {
			break
		}
		u.Log.Warnf("%s 第 %d 次没取到: %v", what, attempt, err)
		if attempt < downloadAttempts {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
			}
		}
	}
	return unavailable("按区间取 %d-%d 没成（%v）", off, off+n-1, last)
}

// planRange reads the package's central directory and compares it with the root: which entries to fetch, which to
// copy. Too much to fetch is a *rangeUnavailable.
func (u *Updater) planRange(ctx context.Context, r *release) (*rangePlan, error) {
	remote := &remoteZip{size: r.size, probed: -1}
	remote.fill = func(off, n int64) ([]byte, error) {
		if n > rangeMaxDirectory {
			return nil, unavailable("包的目录大得不正常（%d 字节）", n)
		}
		return u.rangeRead(ctx, r, off, n, "包的目录")
	}
	zr, err := zip.NewReader(remote, r.size)
	remote.fill = nil
	if err != nil {
		if ctx.Err() != nil {
			return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
		var ru *rangeUnavailable
		if errors.As(err, &ru) {
			return nil, err
		}
		return nil, unavailable("包的目录读不了: %v", err)
	}
	entries, err := packageEntries(zr, r.topDir, true)
	if err != nil {
		// the package itself is bad: the full download gets the same answer and reports it
		return nil, unavailable("%s", errs.As(err).Message)
	}
	plan := &rangePlan{remote: remote}
	for _, e := range sortedEntries(entries) {
		if ctx.Err() != nil {
			return nil, errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
		if !strings.EqualFold(e.rel, manifestName) && u.l.sameAsLocal(e) {
			plan.copy = append(plan.copy, e)
			continue
		}
		start, err := remote.headerOffset(e.f)
		if err != nil {
			return nil, err
		}
		length := min(localHeaderLen+int64(len(e.f.Name))+int64(len(e.f.Extra))+rangeHeaderSlack+int64(e.f.CompressedSize64), r.size-start)
		plan.fetch = append(plan.fetch, rangeEntry{e, start, length})
	}
	if fetched := plan.fetchBytes(); rangeTooMuch(len(plan.fetch), fetched, r.size) {
		return nil, &rangeUnavailable{reason: fmt.Sprintf("变了的文件太多（%d 个、约 %s，超过整包的 %.0f%% 或 %d 个文件）",
			len(plan.fetch), mib(fetched), rangeMaxFetchRatio*100, rangeMaxFetchEntries), normal: true}
	}
	return plan, nil
}

// rangeTooMuch: fetching entries entries of bytes bytes from a package of size bytes is not worth it.
func rangeTooMuch(entries int, bytes, size int64) bool {
	return entries > rangeMaxFetchEntries || float64(bytes) > rangeMaxFetchRatio*float64(size)
}

// headerOffset is where f's local file header starts in the package. archive/zip keeps it to itself, but reading that
// header is the first thing DataOffset does, so the offset of that read is it.
func (r *remoteZip) headerOffset(f *zip.File) (int64, error) {
	r.probing, r.probed = true, -1
	f.DataOffset() // fails when the header is not fetched, which is the usual case; only the read's offset matters
	r.probing = false
	if r.probed < 0 || r.probed >= r.size {
		return 0, unavailable("找不到 %s 在包里的位置", f.Name)
	}
	return r.probed, nil
}

// rangeGroups are the entries to fetch in package order, those close together merged into one request.
type rangeGroup struct {
	start, length int64
	members       []rangeEntry
}

func rangeGroups(fetch []rangeEntry) []rangeGroup {
	sorted := append([]rangeEntry{}, fetch...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].start < sorted[b].start })
	var groups []rangeGroup
	for _, e := range sorted {
		if n := len(groups); n > 0 {
			g := &groups[n-1]
			end := max(g.start+g.length, e.start+e.length)
			if e.start-(g.start+g.length) <= rangeMergeGap && end-g.start <= rangeMaxGroup {
				g.length = end - g.start
				g.members = append(g.members, e)
				continue
			}
		}
		groups = append(groups, rangeGroup{e.start, e.length, []rangeEntry{e}})
	}
	return groups
}

func mib(n int64) string { return fmt.Sprintf("%.1f MB", float64(n)/(1<<20)) }

// stageByRange builds pkg\ from the changed entries of r's package and the root's unchanged files and checks it
// against the manifest: the file count and how it was verified. Any *rangeUnavailable means: download the whole
// package instead (pkg\ is left for the full unpack to replace).
func (u *Updater) stageByRange(ctx context.Context, r *release) (int, string, error) {
	// downloading is reported by progress alone, as for a full download (`modavd update` logs those itself)
	u.progress("downloading", "从 GitHub 读整包的文件目录，和本地的文件比对", -1, 0, 0)
	plan, err := u.planRange(ctx, r)
	if err != nil {
		return 0, "", err
	}
	groups := rangeGroups(plan.fetch)
	var total int64 // what the requests ask for (entries' spans overlap when they are small)
	for _, g := range groups {
		total += g.length
	}
	label := fmt.Sprintf("从 GitHub 按区间下载 %d 个文件（%s）", len(plan.fetch), mib(total))
	u.Log.Infof("%s，没变的 %d 个从本地复制（整包 %s）", label, len(plan.copy), mib(r.size))
	if err := removeTree(u.l.pkg()); err != nil {
		return 0, "", err
	}
	if err := os.MkdirAll(u.l.pkg(), 0o755); err != nil {
		return 0, "", err
	}
	var fetched int64
	u.progress("downloading", label, 0, 0, total)
	for _, g := range groups {
		if err := u.stageRangeGroup(ctx, r, g, plan.remote); err != nil {
			return 0, "", err
		}
		fetched += g.length
		u.progress("downloading", label, min(float64(fetched)*100/float64(max(total, 1)), 100), min(fetched, total), total)
	}
	u.step("extracting", "正在拼新版（没变的文件从本地复制）")
	for k, e := range plan.copy {
		if ctx.Err() != nil {
			return 0, "", errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
		if err := copyFile(u.l.at(e.rel), u.l.pkgAt(e.rel)); err != nil {
			return 0, "", err
		}
		if k%200 == 0 || k == len(plan.copy)-1 {
			u.progress("extracting", "正在拼新版（没变的文件从本地复制）", float64(k+1)*100/float64(len(plan.copy)), 0, 0)
		}
	}
	u.step("verifying", "正在按清单核对每个文件")
	verified, err := u.l.verifyPackage(r.version, r.tag)
	if err != nil {
		if errs.CodeOf(err) == errs.UpdateRejected {
			return 0, "", unavailable("按区间拼出来核对不过（%s）", errs.As(err).Message)
		}
		return 0, "", err
	}
	u.Log.Infof("按区间取回 %s（整包 %s），%d 个文件按清单核对通过", mib(fetched), mib(r.size), len(plan.fetch)+len(plan.copy))
	return len(plan.fetch) + len(plan.copy), verified, nil
}

// stageRangeGroup keeps small groups in memory. A large entry uses a private file under pkg so an interrupted
// staging attempt is cleaned by the existing staging recovery. Drop the ReaderAt segment before closing the file.
func (u *Updater) stageRangeGroup(ctx context.Context, r *release, g rangeGroup, remote *remoteZip) (err error) {
	if g.length <= rangeMaxGroup {
		data, err := u.rangeRead(ctx, r, g.start, g.length, r.assetName)
		if err != nil {
			return err
		}
		remote.add(g.start, data)
	} else {
		f, createErr := os.CreateTemp(u.l.pkg(), ".range-*")
		if createErr != nil {
			return createErr
		}
		defer func() {
			// A local cleanup failure must also stop the update instead of starting a full download.
			cerr := f.Close()
			rerr := os.Remove(f.Name())
			if cerr != nil {
				err = cerr
			}
			if rerr != nil {
				err = rerr
			}
		}()
		d := download.Client{HTTP: u.client(), StallTimeout: u.stallTimeout}
		if err := u.rangeRetry(ctx, g.start, g.length, r.assetName, func() error {
			if _, err := f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := f.Truncate(0); err != nil {
				return err
			}
			return d.RangeTo(ctx, r.url, g.start, g.length, r.size, r.assetName, f)
		}); err != nil {
			return err
		}
		remote.segs = append(remote.segs, segment{g.start, g.length, f})
	}
	defer remote.drop(g.start)
	for _, e := range g.members {
		if ctx.Err() != nil {
			return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
		if err := extractEntry(e.zipEntry, u.l.pkgAt(e.rel)); err != nil {
			if errs.CodeOf(err) != errs.UpdateRejected {
				return err
			}
			return unavailable("取回的 %s 不对: %v", e.rel, errs.As(err).Message)
		}
	}
	return nil
}

// rangeAllowed: GitHub's full package of a version that carries a manifest (the range path cannot check the package's
// SHA-256, so the manifest has to vouch for every file), with no full download of it (finished or partial) to use.
func (u *Updater) rangeAllowed(r *release) bool {
	if u.nativeUpdates() || r.source != SourceGitHub || r.incremental || !requiresManifest(r.tag) || r.size <= 0 {
		return false
	}
	final := u.l.downloadAt(r.assetName)
	return !lexists(final) && !lexists(final+".part")
}

// rangeNotice is the notice for a range attempt that fell back to the whole package.
func rangeNotice(err error) string {
	return "按区间下载没用上：" + strings.TrimSpace(err.Error()) + "，改下整包"
}
