package components

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/errs"
	"modavd/internal/root"
)

func (f *fetcher) stagingRoot() string { return filepath.Join(f.sdk, ".staging") }

// removeStaging deletes one staging directory, only ever under sdk\.staging, and sdk\.staging when that is left
// empty (the script's Remove-Staging).
func (f *fetcher) removeStaging(dir string) {
	stagingRoot := f.stagingRoot()
	if !within(dir, stagingRoot) {
		return
	}
	os.RemoveAll(dir)
	if entries, err := os.ReadDir(stagingRoot); err == nil && len(entries) == 0 {
		os.Remove(stagingRoot)
	}
}

// clearStaging removes what a run that was killed left in sdk\.staging: the whole directory belongs to fetch, and only
// the holder of the lock gets here (the script's Clear-StagingRoot).
func (f *fetcher) clearStaging() {
	stagingRoot := f.stagingRoot()
	if _, err := os.Lstat(stagingRoot); err != nil {
		return
	}
	os.RemoveAll(stagingRoot)
	f.note(`  清理了上次遗留的 sdk\.staging`)
}

// within: path is strictly inside dir (case-insensitive, like the script's OrdinalIgnoreCase).
func within(path, dir string) bool {
	p, err1 := filepath.Abs(path)
	d, err2 := filepath.Abs(dir)
	if err1 != nil || err2 != nil {
		return false
	}
	d = strings.TrimRight(d, `\/`) + string(filepath.Separator)
	return len(p) > len(d) && strings.EqualFold(p[:len(d)], d)
}

func rejected(format string, args ...any) error {
	return errs.Newf(errs.ComponentsRejected, format, args...)
}

// install unpacks zipPath into sdk\.staging\<id>, checks it and moves it into place (the script's Install-Component).
// An existing target directory (incomplete, or install would not run) is renamed to <target>.incomplete-<time> and
// kept for the user, never deleted; it is moved back when the new one cannot be moved in. Returns where it went.
func (f *fetcher) install(ctx context.Context, c root.SDKComponent, zipPath string) (setAside string, err error) {
	staging := filepath.Join(f.stagingRoot(), c.ID)
	f.where = "解压 " + filepath.Base(zipPath) + " 到 " + staging
	f.removeStaging(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", errs.Wrap(errs.Internal, err, "建不了 "+staging+"："+err.Error())
	}
	defer f.removeStaging(staging)
	if err := f.extract(ctx, c, zipPath, staging); err != nil {
		return "", err
	}
	if err := f.checkStaged(c, staging); err != nil {
		return "", err
	}
	extracted := filepath.Join(staging, filepath.FromSlash(c.ArchiveRoot))

	f.where = `把解压好的 ` + c.FetchName + ` 挪到 sdk\` + c.Target
	target := c.Dir(f.sdk)
	if _, err := os.Lstat(target); err == nil {
		setAside, err = asideName(target, time.Now())
		if err != nil {
			return "", errs.Wrap(errs.Internal, err, "选不了旧目录迁移位置："+err.Error())
		}
		if err := os.Rename(target, setAside); err != nil {
			return "", errs.Wrap(errs.Internal, err, `挪不开原有的 sdk\`+c.Target+"："+err.Error())
		}
		f.warn(fmt.Sprintf(`  原有的 sdk\%s 不完整，已改名为 %s，确认没用后可以自己删掉`, c.Target, filepath.Base(setAside)))
	}
	moveErr := os.MkdirAll(filepath.Dir(target), 0o755)
	if moveErr == nil {
		moveErr = os.Rename(extracted, target)
	}
	if moveErr != nil {
		if setAside != "" && os.Rename(setAside, target) == nil {
			f.warn(fmt.Sprintf(`  新的挪不进去，已把 %s 改回 sdk\%s`, filepath.Base(setAside), c.Target))
			setAside = ""
		}
		return setAside, errs.Wrap(errs.Internal, moveErr, `挪不进 sdk\`+c.Target+"："+moveErr.Error())
	}
	if !c.Complete(target) {
		return setAside, rejected(`装完后 sdk\%s 仍缺关键文件`, c.Target)
	}
	return setAside, nil
}

// asideName is <target>.incomplete-<yyyyMMdd-HHmmss>, with -2, -3 ... when that is taken.
func asideName(target string, now time.Time) (string, error) {
	base := target + ".incomplete-" + now.Format("20060102-150405")
	name := base
	for n := 2; ; n++ {
		if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
		name = fmt.Sprintf("%s-%d", base, n)
	}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// checkStaged checks the unpacked component before it goes anywhere: the archive root is there with every key file, a
// missing source.properties is written from the table, and the whole must be Complete.
func (f *fetcher) checkStaged(c root.SDKComponent, staging string) error {
	prefix := strings.TrimRight(c.ArchiveRoot, "/") + "/"
	extracted := filepath.Join(staging, filepath.FromSlash(c.ArchiveRoot))
	if !isDir(extracted) {
		return rejected("压缩包结构不对：没有 %s 目录", prefix)
	}
	for _, k := range c.KeyFiles {
		if fi, err := os.Stat(filepath.Join(extracted, k)); err != nil || !fi.Mode().IsRegular() {
			return rejected("压缩包里缺少 %s", k)
		}
	}
	props := filepath.Join(extracted, "source.properties")
	if _, err := os.Lstat(props); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(props, []byte(c.Properties), 0o644); err != nil {
			return errs.Wrap(errs.Internal, err, "写不了 "+props+"："+err.Error())
		}
		f.say("  压缩包里没有 source.properties，已补写")
	}
	if !c.Complete(extracted) {
		return rejected("解压后的关键文件或版本校验失败：%s", c.FetchName)
	}
	return nil
}

// extract unpacks every entry of zipPath into staging, with progress and a stop at Ctrl+C between two reads. Every
// entry must sit under the archive root and land inside staging; encrypted entries and methods other than store and
// deflate are refused, an entry is never written twice, and the files get the zip's times (DOS fields, local time,
// as the script sets them).
func (f *fetcher) extract(ctx context.Context, c root.SDKComponent, zipPath, staging string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return rejected("压缩包打不开：%v", err)
	}
	defer zr.Close()
	prefix := strings.TrimRight(c.ArchiveRoot, "/") + "/"
	total := int64(0)
	for _, e := range zr.File {
		total += int64(e.UncompressedSize64)
	}
	if total <= 0 {
		total = 1
	}
	m := newMeter(f.o.Progress, "extracting", c.FetchName, total, 0)
	buf := make([]byte, 1<<20)
	done := int64(0)
	for _, e := range zr.File {
		if ctx.Err() != nil {
			return canceled(ctx)
		}
		name := strings.ReplaceAll(e.Name, `\`, "/")
		if !strings.HasPrefix(name, prefix) {
			return rejected("压缩包结构不对：有不在 %s 下的条目 %s", prefix, e.Name)
		}
		dest := filepath.Join(staging, filepath.FromSlash(name))
		if !within(dest, staging) {
			return rejected("压缩包里有非法路径：%s", e.Name)
		}
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(dest, 0o755); err != nil {
				return errs.Wrap(errs.Internal, err, "建不了 "+dest+"："+err.Error())
			}
			continue
		}
		if e.Flags&1 != 0 {
			return rejected("压缩包里的 %s 是加密的", e.Name)
		}
		if e.Method != zip.Store && e.Method != zip.Deflate {
			return rejected("压缩包里 %s 用了不支持的压缩方式 %d", e.Name, e.Method)
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return errs.Wrap(errs.Internal, err, "建不了 "+filepath.Dir(dest)+"："+err.Error())
		}
		n, err := f.extractFile(ctx, e, dest, buf, done, m)
		done += n
		if err != nil {
			return err
		}
		if n != int64(e.UncompressedSize64) {
			return rejected("解压出的 %s 大小不对", e.Name)
		}
		if when, ok := dosTime(e.ModifiedDate, e.ModifiedTime); ok {
			os.Chtimes(dest, time.Time{}, when)
		}
	}
	m.finish(done)
	return nil
}

// extractFile writes one entry (read to its end, so archive/zip checks the CRC) and returns the bytes written.
func (f *fetcher) extractFile(ctx context.Context, e *zip.File, dest string, buf []byte, before int64, m *meter) (int64, error) {
	in, err := e.Open()
	if err != nil {
		return 0, rejected("压缩包里的 %s 解不开：%v", e.Name, err)
	}
	defer in.Close()
	// CreateNew, like the script: a second entry of the same name fails instead of replacing the first.
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, rejected("压缩包里的 %s 出现了两次", e.Name)
		}
		return 0, errs.Wrap(errs.Internal, err, "写不了 "+dest+"："+err.Error())
	}
	written := int64(0)
	for {
		if ctx.Err() != nil {
			out.Close()
			return written, canceled(ctx)
		}
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, err := out.Write(buf[:n]); err != nil {
				out.Close()
				return written, errs.Wrap(errs.Internal, err, "写不了 "+dest+"："+err.Error())
			}
			written += int64(n)
			m.add(before + written)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return written, rejected("压缩包里的 %s 解不开：%v", e.Name, rerr)
		}
	}
	if err := out.Close(); err != nil {
		return written, errs.Wrap(errs.Internal, err, "写不了 "+dest+"："+err.Error())
	}
	return written, nil
}

// dosTime is an entry's MS-DOS date and time as local time (the script's New-Object DateTime); ok=false when the
// fields are not a valid date, where the script falls back to now.
func dosTime(date, clock uint16) (time.Time, bool) {
	y, mo, d := int(date>>9)+1980, time.Month(date>>5&15), int(date&31)
	h, mi, s := int(clock>>11), int(clock>>5&63), int(clock&31)*2
	t := time.Date(y, mo, d, h, mi, s, 0, time.Local)
	if t.Year() != y || t.Month() != mo || t.Day() != d || t.Hour() != h || t.Minute() != mi || t.Second() != s {
		return time.Time{}, false
	}
	return t, true
}
