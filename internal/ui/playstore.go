package ui

// The Google Play Store (ModAVD/ModAVD#125) in the manager: an instance's 「补全 / 移除 Google Play 商店」 (emu
// PlayStore), its 「复制 GSF ID」 (emu PlayStoreGSFID) and 补全组件's 「使用我自己的文件」 (components.ImportPlayStore).
// The store's state shows in GET /api/components (play_store) and on each instance (InstanceInfo.play_store,
// play_store_partial).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"

	"modavd/internal/components"
	"modavd/internal/errs"
	"modavd/internal/playstore"
)

// maxImportFiles: the five files and the zip, with room for one given twice.
const maxImportFiles = 8

var (
	// maxImportPart is the largest file worth receiving: the Play image's zip (the five files are far smaller).
	maxImportPart int64 = playstore.ImageSize
	// maxImportBody bounds a whole import upload: the zip and the five files, with room for the multipart framing.
	maxImportBody = maxImportPart + 2*playstore.ExtractedBytes() + 1<<20
)

// handlePlayStore is POST /api/instances/{i}/playstore-install and playstore-uninstall: a job running emu's
// PlayStore. What it refuses before touching the instance (no bridge, the root has not got the files, not there,
// being created) is answered at once, with errs.Reason in the details, so the page can offer the way out.
func (s *server) handlePlayStore(action string) http.HandlerFunc {
	kind, doing, done := kindPlayStoreInstall, "正在补全 Google Play 商店（会冷启动一次）", "已补全 Google Play 商店"
	if action == "uninstall" {
		kind, doing, done = kindPlayStoreUninstall, "正在移除 Google Play 商店（会冷启动一次）", "已移除 Google Play 商店"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		i, ok := pathIndex(w, r)
		if !ok || !decode(w, r, &struct{}{}) {
			return
		}
		if err := s.checkFree(kind, i); err != nil {
			writeError(w, err)
			return
		}
		if err := s.mgr.PlayStoreCheck(i, action); err != nil {
			writeError(w, err)
			return
		}
		s.respondJob(w)(s.startJob(kind, i, func(ctx context.Context, j *job, b backend) (any, error) {
			s.setProgress(j, jobProgress{Step: kind, Message: fmt.Sprintf("实例 %d %s", i, doing), Total: progressTotal})
			res, err := b.PlayStore(ctx, i, action)
			if err != nil {
				return nil, err
			}
			s.setProgress(j, jobProgress{Step: "done", Message: fmt.Sprintf("实例 %d %s", i, done), Done: progressTotal, Total: progressTotal})
			return res, nil
		}))
	}
}

// handlePlayStoreGSFID is GET /api/instances/{i}/playstore-gsfid: the running instance's GSF ID (emu PlayStoreGSFID,
// a few seconds), for 复制 GSF ID. Not booted (as a recent list says) is 409 busy like the screenshot, so is a job
// running on the instance; no ID yet is 422 not_ready with details.reason no_gsf_id.
func (s *server) handlePlayStoreGSFID(w http.ResponseWriter, r *http.Request) {
	i, ok := pathIndex(w, r)
	if !ok {
		return
	}
	if in, ok := s.hub.cachedInstance(i, statusMaxAge); ok && !in.Booted {
		writeError(w, errs.Newf(errs.Busy, "实例 %d 没有开机", i).WithAdvice("开机后才能取 GSF ID"))
		return
	}
	if err := s.instanceBusy(i); err != nil {
		writeError(w, err)
		return
	}
	res, err := s.mgr.PlayStoreGSFID(r.Context(), i)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleImportPlayStore is POST /api/components/import-playstore: multipart/form-data with the field "file" once per
// file (1 to maxImportFiles: Google's Play image zip, or the store's files). Like an APK upload the parts are streamed
// to a folder under %TEMP%\modavd-ui\ (never held in memory) and deleted when the job ends; a part larger than the zip
// is refused while it arrives. The job (a root job, kind playstore-import) checks and imports them; its result is
// components.PlayStoreImportResult, a refusal components_rejected with errs.Reason.
func (s *server) handleImportPlayStore(w http.ResponseWriter, r *http.Request) {
	// refuse before the upload: 1.5 GB only to hear "busy" is the worst outcome
	if err := s.checkFree(kindPlayStoreImport, rootJobIndex); err != nil {
		writeError(w, err)
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		writeError(w, errs.New(errs.Usage, "导入要用 multipart/form-data 上传，文件字段名 file（每个文件一个）"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxImportBody)
	dir, files, err := receiveImport(r)
	if err != nil {
		writeError(w, err)
		return
	}
	v, err := s.startRootJob(kindPlayStoreImport, func(ctx context.Context, j *job, _ backend) (any, error) {
		defer os.RemoveAll(dir)
		return s.jobDiag(j).ImportPlayStore(ctx, files, func(p DiagProgress) { s.setDiagProgress(j, p) })
	})
	if err != nil {
		os.RemoveAll(dir)
	}
	s.respondJob(w)(v, err)
}

// receiveImport streams every "file" part into a new folder under uploadRoot: dir is that folder, files what the
// import gets (each one's Name is the file name the page sent).
func receiveImport(r *http.Request) (dir string, files []components.ImportFile, err error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return "", nil, errs.Wrap(errs.Usage, err, "上传的内容不是有效的 multipart/form-data")
	}
	defer func() {
		if err != nil && dir != "" {
			os.RemoveAll(dir)
			dir, files = "", nil
		}
	}()
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			return dir, nil, importUploadError(perr)
		}
		if part.FormName() != "file" {
			part.Close()
			continue
		}
		if len(files) == maxImportFiles {
			part.Close()
			return dir, nil, errs.Newf(errs.InvalidArgument, "一次最多导入 %d 个文件", maxImportFiles)
		}
		name := safeFileName(part.FileName())
		if part.FileName() == "" {
			name = fmt.Sprintf("文件 %d", len(files)+1)
		}
		if dir == "" {
			if err := os.MkdirAll(uploadRoot(), 0o755); err != nil {
				return "", nil, errs.Wrap(errs.Internal, err, "建不了临时文件夹 "+uploadRoot()+": "+err.Error())
			}
			if dir, err = os.MkdirTemp(uploadRoot(), "upload-"); err != nil {
				return "", nil, errs.Wrap(errs.Internal, err, "建不了临时文件夹: "+err.Error())
			}
		}
		// numbered: two files of the same name may come (both are checked)
		path := filepath.Join(dir, fmt.Sprintf("%d-%s", len(files)+1, name))
		n, err := saveImportPart(path, io.LimitReader(part, maxImportPart+1))
		if err != nil {
			return dir, nil, err
		}
		if n > maxImportPart {
			return dir, nil, errs.Newf(errs.InvalidArgument, "%s 比 Google Play 版镜像的压缩包还大，不是要导入的文件", name)
		}
		if n == 0 {
			return dir, nil, errs.Newf(errs.InvalidArgument, "上传的 %s 是空文件", name)
		}
		files = append(files, components.ImportFile{Path: path, Name: name})
	}
	if len(files) == 0 {
		return dir, nil, errs.New(errs.Usage, "上传里没有文件字段 file")
	}
	return dir, files, nil
}

// saveImportPart is saveTo with the import's message for a body that is too large.
func saveImportPart(path string, src io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, err, "写不了临时文件 "+path+": "+err.Error())
	}
	n, err := io.Copy(f, src)
	cerr := f.Close()
	if err != nil {
		return n, importUploadError(err)
	}
	if cerr != nil {
		return n, errs.Wrap(errs.Internal, cerr, "写临时文件失败: "+cerr.Error())
	}
	return n, nil
}

func importUploadError(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return errs.New(errs.InvalidArgument, "上传的文件加起来太大：只要 Google Play 版镜像的压缩包，或者商店的五个文件")
	}
	return errs.Wrap(errs.Usage, err, "上传没有传完: "+err.Error())
}
