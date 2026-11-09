package components

// The Google Play Store's five files (root.PlayStoreComponent, ModAVD/ModAVD#125). They get into sdk\playstore two
// ways, and only they stay there: fetched along with the other components when asked for (Options.PlayStore: Google's
// Play image is downloaded, the files are taken out of its system.img, the image and the zip are deleted), or imported
// from what the user has (ImportPlayStore): that zip, or the five files themselves. Each file is checked against its
// pinned size and SHA-256 before it goes in; source.properties says where they came from (ModAVD.Source). Instances
// get the store with `modavd playstore <i> install` (package emu); new instances never do.

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strings"

	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/playstore"
	"modavd/internal/root"
)

// Where the files in sdk\playstore came from (source.properties ModAVD.Source, PlayStoreStatus.Source).
const (
	PlayStoreFromDownload = "download" // components fetch --play-store
	PlayStoreFromImport   = "import"   // components import-playstore
	playStoreSourceKey    = "ModAVD.Source"
)

// Values of errs.Reason on an import's errs.ComponentsRejected.
const (
	ImportReasonUnknown    = "unknown_file" // neither the Play image's zip nor one of the five files (by size)
	ImportReasonMismatch   = "mismatch"     // the size fits, the SHA-1 (zip) or SHA-256 (file) does not
	ImportReasonIncomplete = "incomplete"   // some of the five are neither given nor already in sdk\playstore
)

// playStoreFiles is the table of the five files (a variable for the tests, with root.PlayStoreComponent's pins
// swapped in through playStoreCatalogue to match).
var playStoreFiles = playstore.CurrentProfile().Files

// PlayStoreFile is one of the five files in a PlayStoreStatus.
type PlayStoreFile struct {
	Name    string `json:"name"`    // its name in sdk\playstore
	Guest   string `json:"guest"`   // where an install puts it in the instance
	Size    int64  `json:"size"`    // its pinned size
	Present bool   `json:"present"` // a file of that size is there
}

// PlayStoreStatus is the root's Google Play Store files: what components status, the manager's component list and its
// instance buttons show. Cheap: sizes and source.properties only; the SHA-256 of each file is checked where the files
// are put in (fetch, import) and used (playstore install).
type PlayStoreStatus struct {
	Dir              string          `json:"dir"`     // <sdk>\playstore
	Ready            bool            `json:"ready"`   // all five are there and source.properties has the revision
	Source           string          `json:"source"`  // download, import, or "" (none, or put there some other way)
	Version          string          `json:"version"` // Pkg.Revision, "" when none
	Files            []PlayStoreFile `json:"files"`
	Archive          string          `json:"archive"`           // the matching Google Play image zip
	DownloadBytes    int64           `json:"download_bytes"`    // size of that archive
	InstallSupported bool            `json:"install_supported"` // preparing the files does not enable instance operations
}

// PlayStoreState reads r's Google Play Store files (in the SDK the instances run on).
func PlayStoreState(r *root.Root) PlayStoreStatus {
	c := playStoreCatalogue()
	dir := c.Dir(r.RuntimeSDK())
	profile := playstore.CurrentProfile()
	st := PlayStoreStatus{Dir: dir, Files: []PlayStoreFile{}, Archive: filepath.Base(profile.ImageURL),
		DownloadBytes: profile.ImageSize, InstallSupported: root.CurrentPlatform().Windows()}
	all := true
	for _, f := range playStoreFiles {
		fi, err := os.Stat(filepath.Join(dir, f.Name))
		present := err == nil && fi.Mode().IsRegular() && fi.Size() == f.Size
		all = all && present
		st.Files = append(st.Files, PlayStoreFile{Name: f.Name, Guest: f.Guest(), Size: f.Size, Present: present})
	}
	st.Version = root.Revision(dir)
	if data, err := root.ReadFileShared(filepath.Join(dir, "source.properties")); err == nil {
		_, values := root.ReadKeyValues(data, false)
		st.Source = values[playStoreSourceKey]
	}
	st.Ready = all && strings.EqualFold(st.Version, c.Version)
	if len(c.RequiredProperties) != 0 {
		st.Ready = all && c.MetadataMatches(dir)
	}
	return st
}

// ImportFile is one file handed to ImportPlayStore: Path is read (never changed), Name is what messages call it (the
// user's name for it; Path's last part when "").
type ImportFile struct {
	Path string
	Name string
}

func (f ImportFile) name() string {
	if f.Name != "" {
		return f.Name
	}
	return filepath.Base(f.Path)
}

// PlayStoreImportResult is the result of ImportPlayStore (`components import-playstore`, the manager's 「使用我自己的
// 文件」). Every field is always present.
type PlayStoreImportResult struct {
	Root     string          `json:"root"`
	From     string          `json:"from"`      // zip (Google's Play image) or files (the five files themselves)
	Used     []string        `json:"used"`      // the given files that were checked and taken, by their names
	Kept     []string        `json:"kept"`      // files of sdk\playstore that were right and stay (not given this time)
	Skipped  bool            `json:"skipped"`   // sdk\playstore had all five already: the given files were checked, nothing changed
	SetAside string          `json:"set_aside"` // where an incomplete old sdk\playstore went (sdk\...incomplete-<time>), "" when none
	Status   PlayStoreStatus `json:"status"`    // after the import
}

// PlayStoreImportFiles turns the command line's paths into ImportPlayStore's files: a file as it is; a directory as
// the files in it named like the five (Phonesky.apk ...), others in it are left alone. errs.NotFound for a path that is
// not there, errs.InvalidArgument for a directory with none of them.
func PlayStoreImportFiles(paths []string) ([]ImportFile, error) {
	var out []ImportFile
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, errs.Newf(errs.NotFound, "找不到 %s", p)
		}
		if !fi.IsDir() {
			out = append(out, ImportFile{Path: p})
			continue
		}
		n := len(out)
		for _, f := range playStoreFiles {
			q := filepath.Join(p, f.Name)
			if st, err := os.Stat(q); err == nil && st.Mode().IsRegular() {
				out = append(out, ImportFile{Path: q})
			}
		}
		if len(out) == n {
			return nil, errs.Newf(errs.InvalidArgument, "文件夹 %s 里没有 Google Play 商店的那五个文件（%s）", p, strings.Join(playStoreNames(), "、")).
				WithAdvice("Google Play 版镜像的压缩包请直接给出它的路径")
		}
	}
	return out, nil
}

func playStoreNames() []string {
	names := make([]string, len(playStoreFiles))
	for k, f := range playStoreFiles {
		names[k] = f.Name
	}
	return names
}

// importItem is a given file once its size said what it is: the Play image's zip, or file slot of playStoreFiles.
type importItem struct {
	ImportFile
	size int64
	zip  bool
	slot int
}

func importRejected(reason, format string, args ...any) *errs.Error {
	return errs.WithDetail(rejected(format, args...), errs.Reason, reason)
}

// ImportPlayStore puts the Google Play Store's five files into r's sdk\playstore from files the user has: Google's
// Play image zip for this host (under any name, checked against the size and SHA-1 of Google's repository, then
// read like a download), or the five files themselves under any names (each checked against its pinned size and
// SHA-256 while it is copied). A file whose size is neither is refused before anything is read; one whose hash does
// not match after; nothing changes then. Files not given are taken from sdk\playstore when they are there and match,
// and the import is refused when some are still missing. When sdk\playstore is complete already, the given files
// are still checked and nothing changes (Skipped). The user's files are only read. It holds the fetch's lock and
// writes logs\fetch-components.log like a fetch; no license is asked (nothing is downloaded).
//
// Error codes: Unsupported (no native catalogue), Usage (no file), NotFound, NotReady (not a ModAVD root, or sdkRoot set),
// Busy (a fetch or an import runs), PrecheckFailed (space), ComponentsRejected (errs.Reason: ImportReason*),
// Canceled, Internal.
func ImportPlayStore(ctx context.Context, r *root.Root, files []ImportFile, o Options) (res *PlayStoreImportResult, err error) {
	if fetchGOOS != "windows" && !(fetchGOOS == "darwin" && nativeDarwinComponents) {
		return nil, errs.New(errs.Unsupported, "这个系统还没有组件清单").WithAdvice("目前支持 Windows 和 Apple Silicon macOS")
	}
	if r == nil {
		return nil, errs.New(errs.NotFound, "找不到 ModAVD 根目录")
	}
	if len(files) == 0 {
		return nil, errs.New(errs.Usage, "没有给要导入的文件").
			WithAdvice("给 Google Play 版镜像的压缩包 " + filepath.Base(playstore.CurrentProfile().ImageURL) + "，或者商店的五个文件：" + strings.Join(playStoreNames(), "、"))
	}
	if o.Log == nil {
		o.Log = logx.Nop()
	}
	o.PlayStore = true
	f := &fetcher{r: r, o: o, sdk: r.SDK()}
	emulator := filepath.Join("sdk", "emulator", root.CurrentPlatform().Emulator)
	if _, err := os.Stat(filepath.Join(f.sdk, "emulator", root.CurrentPlatform().Emulator)); err != nil {
		return nil, errs.Newf(errs.NotReady, "这里没有 %s，不像是 ModAVD 的文件夹：%s", emulator, r.Dir)
	}
	if r.LocalSDKRoot() != "" {
		return nil, errs.New(errs.NotReady, "这个根目录在 mas-avd.json 里设了 sdkRoot（开发用的本地 SDK），不导入组件").
			WithAdvice(`实例用的是那份 SDK；要给这个根目录的 sdk\ 导入，先去掉 sdkRoot`)
	}
	if f.darwin() {
		if err := f.platformPaths(); err != nil {
			return nil, err
		}
		if err := f.checkImportSources(files); err != nil {
			return nil, err
		}
	}
	f.openLog()
	defer f.closeLog()
	lock, err := f.lock()
	if err != nil {
		return nil, err
	}
	defer lock.Unlock()
	defer func() {
		if err != nil {
			if f.where != "" {
				f.note("失败：" + f.where)
			}
			f.note("原因：" + errs.As(err).Error())
			if !f.darwin() || f.platformPaths() == nil {
				f.clearStaging()
			}
		}
		f.note(fmt.Sprintf("退出码 %d", errs.ExitCode(err)))
	}()
	f.say("ModAVD：用自己的文件补全 Google Play 商店")
	f.say("文件夹：" + r.Dir)
	if f.darwin() {
		if err := f.platformPaths(); err != nil {
			return nil, err
		}
		if err := f.platformIdle(); err != nil {
			return nil, err
		}
	}
	f.clearStaging()

	c := playStoreCatalogue()
	items, err := classifyImport(files, c)
	if err != nil {
		return nil, err
	}
	res = &PlayStoreImportResult{Root: r.Dir, From: "files", Used: []string{}, Kept: []string{}}
	need := int64(500 << 20)
	var zipItem *importItem
	for k := range items {
		it := &items[k]
		f.note(fmt.Sprintf("  给的文件：%s（%d 字节）", it.name(), it.size))
		if it.zip {
			if zipItem == nil {
				zipItem = it
			}
		} else {
			need += it.size
		}
	}
	if zipItem != nil {
		res.From = "zip"
		need = 500<<20 + c.ExtractedBytes
		if f.darwin() {
			// Hash and extract the same private copy: the user's path may be
			// replaced after the import has verified it.
			need += c.Size
		}
	} else if f.darwin() {
		// Missing files are copied from the old component too. It remains in
		// place until the complete replacement is ready.
		need = 500 << 20
		for _, pf := range playStoreFiles {
			need += pf.Size
		}
	}
	if err := f.importSpace(need); err != nil {
		return nil, err
	}

	staging, out, err := f.playStoreStaging(c)
	if err != nil {
		return nil, err
	}
	defer f.removeStaging(staging)
	zipPath := ""
	if zipItem != nil {
		zipPath = zipItem.Path
		if f.darwin() {
			zipPath = filepath.Join(staging, "source.zip")
		}
	}
	// every given file first: a wrong one refuses the import before the 3 GB of a zip are unpacked
	have := make([]bool, len(playStoreFiles))
	for k := range items {
		it := &items[k]
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		if it.zip {
			f.where = "校验 " + it.name()
			algorithm := "SHA-1"
			if c.SHA256 != "" {
				algorithm += "、SHA-256"
			}
			f.say("校验 " + it.name() + "（" + algorithm + "）…")
			copyTo := ""
			if f.darwin() && it == zipItem {
				copyTo = zipPath
			}
			if err := f.verifyImportArchive(ctx, *it, c, copyTo); err != nil {
				return nil, err
			}
		} else {
			want := playStoreFiles[it.slot]
			f.where = "校验 " + it.name()
			f.say(fmt.Sprintf("校验 %s（%s）…", it.name(), want.Name))
			var sum string
			if have[it.slot] {
				// the same file given twice: checked, the first copy stays
				sum, err = f.readHashed(ctx, it.Path, sha256.New(), nil, it.name(), want.Size)
			} else {
				sum, err = f.copyHashed(ctx, it.Path, filepath.Join(out, want.Name), it.name(), want.Size)
			}
			if err != nil {
				return nil, err
			}
			if sum != want.SHA256 {
				return nil, importRejected(ImportReasonMismatch, "%s 和 Google Play 版镜像 r%s 里的 %s 大小一样，SHA-256 却对不上（是 %s，应为 %s）",
					it.name(), c.Version, want.Name, sum, want.SHA256)
			}
			have[it.slot] = true
		}
		res.Used = append(res.Used, it.name())
	}

	target := c.Dir(f.sdk)
	if c.Complete(target) {
		f.say(`sdk\` + c.Target + " 里五个文件都已齐全、校验无误，没有改动")
		res.Skipped, res.Status = true, PlayStoreState(r)
		return res, nil
	}
	if zipItem != nil {
		emptyDir(out)
		if f.darwin() {
			if err := f.importSpace(500<<20 + c.ExtractedBytes); err != nil {
				return nil, err
			}
		}
		if err := f.playStoreFromZip(ctx, c, zipPath, staging, out); err != nil {
			return nil, err
		}
	} else {
		var missing []string
		for k, pf := range playStoreFiles {
			if have[k] {
				continue
			}
			// one already in place and right stays (copied: the old directory is only set aside when the new one is whole)
			f.where = `检查 sdk\` + c.Target + `\` + pf.Name
			if fi, err := os.Stat(filepath.Join(target, pf.Name)); err == nil && fi.Mode().IsRegular() && fi.Size() == pf.Size {
				sum, err := f.copyHashed(ctx, filepath.Join(target, pf.Name), filepath.Join(out, pf.Name), pf.Name, pf.Size)
				if errs.CodeOf(err) == errs.Canceled {
					return nil, err
				}
				if err == nil && sum == pf.SHA256 {
					res.Kept = append(res.Kept, pf.Name)
					continue
				}
				os.Remove(filepath.Join(out, pf.Name))
			}
			missing = append(missing, pf.Name)
		}
		if len(missing) > 0 {
			return nil, importRejected(ImportReasonIncomplete, "还缺 %s，什么也没有改", strings.Join(missing, "、")).
				WithAdvice("把五个文件一起给出，或者给 Google Play 版镜像的压缩包 " + filepath.Base(c.URL))
		}
	}
	aside, err := f.finishPlayStore(ctx, c, out, PlayStoreFromImport)
	res.SetAside = setAsideRel(f.sdk, aside)
	if err != nil {
		return nil, err
	}
	f.say(`已装好：sdk\` + c.Target)
	res.Status = PlayStoreState(r)
	return res, nil
}

// A source inside staging would be deleted by clearStaging before it is read.
// Compare real ancestor identities to cover symlinks and case aliases too.
// This must run before any cleanup defer is registered.
func (f *fetcher) checkImportSources(files []ImportFile) error {
	staging, err := os.Stat(f.stagingRoot())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.Internal, err, "读不了组件暂存目录")
	}
	for _, in := range files {
		current, err := filepath.EvalSymlinks(in.Path)
		if err != nil {
			return errs.Newf(errs.NotFound, "找不到 %s", in.name())
		}
		current, err = filepath.Abs(current)
		if err != nil {
			return errs.Wrap(errs.Internal, err, "解析不了导入文件路径")
		}
		for {
			info, err := os.Stat(current)
			if err != nil {
				return errs.Wrap(errs.Internal, err, "读不了导入文件的目录")
			}
			if os.SameFile(info, staging) {
				return importRejected(ImportReasonUnknown, "%s 位于组件暂存目录内，不能作为导入源文件", in.name()).
					WithAdvice("请先把源文件移到 sdk/.staging 之外，再重新导入；原文件未改动")
			}
			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
	}
	return nil
}

// verifyImportArchive checks the bytes read, and on macOS retains that checked
// copy in private staging so extraction never reopens the user's mutable path.
func (f *fetcher) verifyImportArchive(ctx context.Context, it importItem, c root.SDKComponent, copyTo string) error {
	var writer io.Writer
	var strong hash.Hash
	if c.SHA256 != "" {
		strong = sha256.New()
		writer = strong
	}
	var out *os.File
	if copyTo != "" {
		var err error
		out, err = os.OpenFile(copyTo, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return errs.Wrap(errs.Internal, err, "写不了导入镜像的暂存副本")
		}
		writer = out
		if strong != nil {
			writer = io.MultiWriter(out, strong)
		}
	}
	sum, err := f.readHashed(ctx, it.Path, sha1.New(), writer, it.name(), c.Size)
	if out != nil {
		if cerr := out.Close(); err == nil && cerr != nil {
			err = errs.Wrap(errs.Internal, cerr, "写不了导入镜像的暂存副本")
		}
	}
	if err != nil {
		return err
	}
	if sum != c.SHA1 {
		return importRejected(ImportReasonMismatch, "%s 的 SHA-1 是 %s，应为 %s：不是 Google 仓库里那份 Google Play 版镜像（%s）",
			it.name(), sum, c.SHA1, filepath.Base(c.URL))
	}
	if strong != nil {
		if got := hex.EncodeToString(strong.Sum(nil)); !strings.EqualFold(got, c.SHA256) {
			return importRejected(ImportReasonMismatch, "%s 的 SHA-256 是 %s，应为 %s", it.name(), got, c.SHA256)
		}
	}
	return nil
}

// emptyDir empties dir (out of a zip import: files given beside the zip were copied there and are not used).
func emptyDir(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// classifyImport says what each given file is by its size alone (the sizes of the zip and the five files differ):
// anything else, or what is not a regular file, refuses the import before any file is read.
func classifyImport(files []ImportFile, c root.SDKComponent) ([]importItem, error) {
	var items []importItem
	for _, in := range files {
		fi, err := os.Stat(in.Path)
		if err != nil {
			return nil, errs.Newf(errs.NotFound, "找不到 %s", in.name())
		}
		if !fi.Mode().IsRegular() {
			return nil, importRejected(ImportReasonUnknown, "%s 不是普通文件", in.name())
		}
		it := importItem{ImportFile: in, size: fi.Size(), slot: -1}
		if c.Size > 0 && fi.Size() == c.Size {
			it.zip = true
		} else {
			for k, f := range playStoreFiles {
				if f.Size == fi.Size() {
					it.slot = k
					break
				}
			}
		}
		if !it.zip && it.slot < 0 {
			return nil, importRejected(ImportReasonUnknown,
				"%s（%d 字节）不是 Google Play 版镜像的压缩包 %s，也不是商店的五个文件（%s）之一",
				in.name(), fi.Size(), filepath.Base(c.URL), strings.Join(playStoreNames(), "、"))
		}
		items = append(items, it)
	}
	return items, nil
}

// importSpace: imports are staged on the SDK's volume on macOS, which can differ
// from the root's volume. TestFreeSpaceMB is treated as in a fetch.
func (f *fetcher) importSpace(need int64) error {
	drive := filepath.VolumeName(f.r.Dir) + `\`
	spacePath := f.r.Dir
	if f.darwin() {
		drive, spacePath = f.sdk, f.sdk
	}
	f.where = "查 " + drive + " 的剩余空间"
	freeBytes, err := diskFree(spacePath)
	if err != nil {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("查不了 %s 的剩余空间：%s", drive, errs.As(err).Message))
	}
	free := int64(min(freeBytes, 1<<62))
	if mb := f.o.TestFreeSpaceMB; mb != nil {
		free = clampMB(*mb)
	}
	if free < need {
		return errs.Newf(errs.PrecheckFailed, "磁盘空间不够：还需要约 %s，%s 只剩 %s", formatSize(float64(need)), drive, formatSize(float64(free)))
	}
	return nil
}

// readHashed is the hex hash h of the file at p, read with progress (verifying)
// and a stop at cancel. Native imports also bound the copy to its pinned size.
func (f *fetcher) readHashed(ctx context.Context, p string, h hash.Hash, w io.Writer, name string, expected int64) (string, error) {
	in, err := os.Open(p)
	if err != nil {
		return "", errs.Wrap(errs.Internal, err, "读不了 "+name+"："+err.Error())
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return "", errs.Wrap(errs.Internal, err, "读不了 "+name+"："+err.Error())
	}
	if f.darwin() && (!st.Mode().IsRegular() || st.Size() != expected) {
		return "", importRejected(ImportReasonMismatch, "%s 在导入时已变化，类型或大小与组件清单不符", name)
	}
	m := newMeter(f.o.Progress, "verifying", name, max(st.Size(), 1), 0)
	dst := io.Writer(h)
	if w != nil {
		dst = io.MultiWriter(h, w)
	}
	buf := make([]byte, 1<<20)
	var done int64
	for {
		if ctx.Err() != nil {
			return "", canceled(ctx)
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if f.darwin() && done+int64(n) > expected {
				return "", importRejected(ImportReasonMismatch, "%s 在导入时超过组件清单的大小", name)
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return "", errs.Wrap(errs.Internal, err, "写不了 "+name+" 的副本："+err.Error())
			}
			done += int64(n)
			m.add(done)
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return "", errs.Wrap(errs.Internal, rerr, "读不了 "+name+"："+rerr.Error())
		}
	}
	if f.darwin() && done != expected {
		return "", importRejected(ImportReasonMismatch, "%s 在导入时已变化，大小与组件清单不符", name)
	}
	m.finish(done)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyHashed copies src to dst (new) and returns the SHA-256 of what was written: the copy is what gets checked, so a
// file that changes meanwhile cannot slip in.
func (f *fetcher) copyHashed(ctx context.Context, src, dst, name string, expected int64) (string, error) {
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", errs.Wrap(errs.Internal, err, "写不了 "+dst+"："+err.Error())
	}
	sum, err := f.readHashed(ctx, src, sha256.New(), out, name, expected)
	if cerr := out.Close(); err == nil && cerr != nil {
		err = errs.Wrap(errs.Internal, cerr, "写不了 "+dst+"："+cerr.Error())
	}
	return sum, err
}
