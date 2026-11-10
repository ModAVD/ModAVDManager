//go:build darwin && cgo

package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"modavd/internal/errs"
	"modavd/internal/host"
)

const nativeMinFreeBytes = 20 << 30

func readUpdateStateFile(path string) ([]byte, error) { return nativeReadFile(path, 1<<20) }

func (u *Updater) nativeCheckSpace() error {
	free, err := host.DiskFree(u.l.root)
	if err != nil {
		return errs.Wrap(errs.PrecheckFailed, err, "无法确认更新剩余空间，已停止写入")
	}
	if free < nativeMinFreeBytes {
		return errs.Newf(errs.PrecheckFailed, "更新需保留至少 20 GiB 可用空间（当前 %.1f GiB），请先清理", float64(free)/(1<<30))
	}
	return nil
}

func nativeCopy(out io.Writer, in io.Reader, check func() error) (int64, error) {
	buf := make([]byte, 1<<20)
	var total int64
	for {
		if check != nil {
			if err := check(); err != nil {
				return total, err
			}
		}
		n, err := in.Read(buf)
		if n > 0 {
			wrote, werr := out.Write(buf[:n])
			total += int64(wrote)
			if werr != nil {
				return total, werr
			}
			if wrote != n {
				return total, io.ErrShortWrite
			}
		}
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
	}
}

func nativeOpenRegular(p string) (*os.File, error) {
	before, err := os.Lstat(p)
	if err != nil || !before.Mode().IsRegular() {
		return nil, packageError("更新文件不是普通文件: %s", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, packageError("更新文件在读取时被替换: %s", p)
	}
	return f, nil
}

func nativeHashFile(p string) (string, error) {
	f, err := nativeOpenRegular(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func nativeReadFile(p string, limit int64) ([]byte, error) {
	f, err := nativeOpenRegular(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, packageError("更新元数据太大: %s", p)
	}
	return b, nil
}

func nativeCopyFile(src, dst string, check func() error) (err error) {
	f, err := nativeOpenRegular(src)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() > maxUnpackedBytes {
		return packageError("本地更新归档太大或不可读")
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	n, err := nativeCopy(out, io.LimitReader(f, info.Size()+1), check)
	if err == nil && n != info.Size() {
		err = packageError("本地更新归档在复制时改变了大小")
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func nativeExtractEntry(e zipEntry, dst string, check func() error) (err error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	rc, err := e.f.Open()
	if err != nil {
		return packageError("更新条目打不开: %s", e.rel)
	}
	defer rc.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	n, err := nativeCopy(out, io.LimitReader(rc, int64(e.f.UncompressedSize64)+1), check)
	if err == nil && n != int64(e.f.UncompressedSize64) {
		err = packageError("更新条目大小不符: %s", e.rel)
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// encoding/json accepts duplicate object fields. The package/journal contract
// rejects them, unknown fields, trailing values and excessively nested input.
func decodeNativeJSON(b []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	var walk func(int) error
	walk = func(depth int) error {
		if depth > 64 {
			return fmt.Errorf("JSON nesting exceeds 64")
		}
		token, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := token.(json.Delim); ok {
			switch d {
			case '{':
				seen := map[string]bool{}
				for dec.More() {
					key, err := dec.Token()
					if err != nil {
						return err
					}
					s, ok := key.(string)
					if !ok || seen[s] {
						return fmt.Errorf("duplicate or invalid JSON field")
					}
					seen[s] = true
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for dec.More() {
					if err := walk(depth + 1); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("invalid JSON delimiter")
			}
			_, err = dec.Token()
		}
		return err
	}
	if err := walk(0); err != nil {
		return packageError("更新 JSON 无效: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return packageError("更新 JSON 有多余内容")
	}
	dec = json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return packageError("更新 JSON 字段无效: %v", err)
	}
	return nil
}
