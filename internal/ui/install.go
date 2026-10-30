//go:build windows

package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/errs"
	"modavd/internal/logx"
)

const (
	// maxUpload: game packages are 1-3 GB; anything far beyond that is a wrong file, and it would fill %TEMP%.
	maxUpload = 8 << 30
	// uploadMaxAge: upload folders older than this are leftovers of a manager that did not get to delete them.
	uploadMaxAge = 24 * time.Hour
)

// uploadRoot is %TEMP%\modavd-ui.
func uploadRoot() string { return filepath.Join(os.TempDir(), "modavd-ui") }

// handleInstall is POST /api/instances/{i}/install: the multipart field "apk" is streamed to a file under
// %TEMP%\modavd-ui\ (never held in memory), installed by a job, and deleted when the job ends.
func (s *server) handleInstall(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	// refuse before the upload: a 2 GB transfer only to hear "busy" is the worst outcome
	if err := s.checkFree(kindInstall, i); err != nil {
		writeError(w, err)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		writeError(w, errs.New(errs.Usage, "安装 APK 要用 multipart/form-data 上传，文件字段名 apk"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	dir, path, err := receiveAPK(r)
	if err != nil {
		writeError(w, err)
		return
	}
	name := filepath.Base(path)
	v, err := s.startJob(kindInstall, i, func(ctx context.Context, j *job, b backend) (any, error) {
		defer os.RemoveAll(dir)
		s.setProgress(j, jobProgress{Step: "install", Message: fmt.Sprintf("实例 %d 正在安装 %s", i, name), Total: progressTotal})
		res, err := b.InstallAPK(ctx, i, path)
		if err != nil {
			return nil, err
		}
		// InstallResult.APK would name our temporary copy; the page shows the file the user picked
		res.APK = name
		s.setProgress(j, jobProgress{Step: "done", Message: fmt.Sprintf("实例 %d 已安装 %s", i, name), Done: progressTotal, Total: progressTotal})
		return res, nil
	})
	if err != nil {
		os.RemoveAll(dir)
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, v)
}

// receiveAPK streams the "apk" part into a new folder under uploadRoot and returns the folder and the file.
func receiveAPK(r *http.Request) (dir, path string, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", "", errs.Wrap(errs.Usage, err, "上传的内容不是有效的 multipart/form-data")
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return "", "", errs.New(errs.Usage, "上传里没有文件字段 apk")
		}
		if err != nil {
			return "", "", uploadError(err)
		}
		if part.FormName() != "apk" {
			part.Close()
			continue
		}
		name := safeFileName(part.FileName())
		if !strings.EqualFold(filepath.Ext(name), ".apk") {
			part.Close()
			return "", "", errs.Newf(errs.InvalidArgument, "%s 不是 .apk 安装包（.xapk、拆分安装包暂不支持）", name)
		}
		if err := os.MkdirAll(uploadRoot(), 0o755); err != nil {
			return "", "", errs.Wrap(errs.Internal, err, "建不了临时文件夹 "+uploadRoot()+": "+err.Error())
		}
		dir, err := os.MkdirTemp(uploadRoot(), "upload-")
		if err != nil {
			return "", "", errs.Wrap(errs.Internal, err, "建不了临时文件夹: "+err.Error())
		}
		path := filepath.Join(dir, name)
		n, err := saveTo(path, part)
		if err != nil {
			os.RemoveAll(dir)
			return "", "", err
		}
		if n == 0 {
			os.RemoveAll(dir)
			return "", "", errs.Newf(errs.InvalidArgument, "上传的 %s 是空文件", name)
		}
		return dir, path, nil
	}
}

func saveTo(path string, src io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, err, "写不了临时文件 "+path+": "+err.Error())
	}
	n, err := io.Copy(f, src)
	if cerr := f.Close(); err == nil && cerr != nil {
		return n, errs.Wrap(errs.Internal, cerr, "写临时文件失败: "+cerr.Error())
	}
	if err != nil {
		return n, uploadError(err)
	}
	return n, nil
}

func uploadError(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return errs.Newf(errs.InvalidArgument, "上传的文件超过 %d GB，不像是 APK", maxUpload>>30)
	}
	return errs.Wrap(errs.Usage, err, "上传没有传完: "+err.Error())
}

// safeFileName keeps the user's file name (InstallAPK and the logs show it) minus what Windows cannot store.
func safeFileName(name string) string {
	// the last path element by hand: filepath.Base would also take "a:" of "a:b.apk" for a drive
	name = name[strings.LastIndexAny(name, `\/`)+1:]
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimRight(strings.TrimSpace(name), ". ")
	if name == "" || name == "." {
		return "upload.apk"
	}
	return name
}

// cleanUploads removes upload folders a previous manager left behind (crash, power cut).
func cleanUploads(log *logx.Logger) {
	entries, err := os.ReadDir(uploadRoot())
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "upload-") {
			continue
		}
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > uploadMaxAge {
			if os.RemoveAll(filepath.Join(uploadRoot(), e.Name())) == nil {
				log.Debugf("删除了遗留的上传文件夹 %s", e.Name())
			}
		}
	}
}
