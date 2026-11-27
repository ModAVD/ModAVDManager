package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"modavd/internal/components"
	"modavd/internal/errs"
	"modavd/internal/playstore"
)

// importPlayStore is components.ImportPlayStore (a variable for the tests).
var importPlayStore = components.ImportPlayStore

// runImportPlayStore is `components import-playstore <file or folder>...`: the Google Play Store's files from what the
// user has (components.ImportPlayStore), no license and no download.
func runImportPlayStore(c *cmdCtx) (any, error) {
	var given []string
	for _, f := range fetchFlags {
		if c.inv.isSet(f.name) {
			given = append(given, "--"+f.name)
		}
	}
	if len(given) > 0 {
		return nil, errs.Newf(errs.Usage, "%s 只用于 components fetch", strings.Join(given, "、")).
			WithAdvice(findCommand("components").usageAdvice())
	}
	paths := c.inv.pos[1:]
	if len(paths) == 0 {
		return nil, errs.New(errs.Usage, "components import-playstore 要给文件或文件夹：Google Play 版镜像的压缩包 "+filepath.Base(playstore.CurrentProfile().ImageURL)+"，或者商店的五个文件").
			WithAdvice(findCommand("components").usageAdvice())
	}
	files, err := components.PlayStoreImportFiles(paths)
	if err != nil {
		return nil, err
	}
	r, err := c.deps.resolveRoot(c.inv.root)
	if err != nil {
		return nil, err
	}
	var last time.Time
	o := components.Options{Log: c.log, Progress: func(p components.Progress) {
		if !p.Final && !last.IsZero() && time.Since(last) < 2*time.Second {
			return
		}
		last = time.Now()
		c.log.Infof("%s", p.Message)
	}}
	return importPlayStore(c.ctx, r, files, o)
}

// playStoreStateLine is the Google Play Store's line in components status, in componentLine's style.
func playStoreStateLine(s components.PlayStoreStatus) string {
	if s.Ready {
		line := "✓ Google Play 商店"
		if s.Version != "" {
			line += " " + s.Version
		}
		from := map[string]string{components.PlayStoreFromDownload: "，下载的", components.PlayStoreFromImport: "，导入的"}[s.Source]
		return line + "（可选" + from + "）"
	}
	var missing []string
	for _, f := range s.Files {
		if !f.Present {
			missing = append(missing, f.Name)
		}
	}
	state := " 未安装"
	switch {
	case len(missing) == 0:
		state = " 不全，source.properties 不对"
	case len(missing) < len(s.Files):
		state = " 不全，缺 " + strings.Join(missing, "、")
	}
	return "✗ Google Play 商店" + state + "（可选；用 components fetch --play-store 或 components import-playstore <文件> 补全）"
}

func formatImportPlayStore(w io.Writer, r *components.PlayStoreImportResult) {
	if r.Skipped {
		fmt.Fprintln(w, "Google Play 商店的五个文件已经齐全、校验无误，没有改动（给的文件也核对过了）")
		return
	}
	from := "商店的文件"
	if r.From == "zip" {
		from = "Google Play 版镜像的压缩包"
	}
	fmt.Fprintf(w, "已用%s补全 Google Play 商店：%s\n", from, r.Status.Dir)
	if len(r.Kept) > 0 {
		fmt.Fprintf(w, "  原来就有、校验无误留下的：%s\n", strings.Join(r.Kept, "、"))
	}
	if r.SetAside != "" {
		fmt.Fprintf(w, "  原来不完整的目录改名为 %s，确认没用后可以自己删掉\n", r.SetAside)
	}
	if runtime.GOOS == "darwin" {
		fmt.Fprintln(w, "ARM64 商店文件已准备；当前 macOS 版本尚未开放实例内安装与移除")
	} else {
		fmt.Fprintln(w, "给实例装上：modavd playstore <实例编号> install")
	}
}

func playStoreFetchHelp() string {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		return "也准备可选的 Google Play 商店：下载 API35 ARM64 r9 镜像（约 1.8 GB），校验后只保留五个文件；实例内装卸尚未开放"
	}
	return "也补全可选的 Google Play 商店：下 Google Play 版系统镜像（约 1.5 GB），只取出商店等 5 个文件（约 230 MB）"
}
