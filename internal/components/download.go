package components

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"modavd/internal/download"
	"modavd/internal/errs"
	"modavd/internal/hostproxy"
	"modavd/internal/root"
)

const (
	licenseManifest  = "repository2-3.xml"
	licenseID        = "android-sdk-license"
	maxManifestBytes = 16 << 20 // the real one is about 420 KB
	maxRedirects     = download.MaxRedirects
	downloadAttempts = 3
)

// retryDelay is the script's pause between two attempts on one mirror (a variable for tests).
var retryDelay = 3 * time.Second

func canceled(ctx context.Context) error { return errs.Wrap(errs.Canceled, ctx.Err(), "已取消") }
func switchMirror(err error) bool {
	var e *download.Error
	return errors.As(err, &e) && (e.Kind == download.Timeout || e.Kind == download.Size || e.Kind == download.Encoding || e.Status >= 400 && e.Status <= 499)
}
func (f *fetcher) httpClient() *http.Client {
	if f.client == nil {
		f.client = download.NewHTTPClient(f.o.ConnectTimeout)
	}
	return f.client
}
func (f *fetcher) downloader() *download.Client {
	return &download.Client{HTTP: f.httpClient(), ConnectTimeout: f.o.ConnectTimeout, StallTimeout: f.o.StallTimeout}
}

// ---- license ---------------------------------------------------------------------------------------------------

// license gets the Android SDK license text from the first mirror that serves repository2-3.xml, and moves the
// mirrors that failed to the end of the list, so a dead one does not cost each component its timeout again.
func (f *fetcher) license(ctx context.Context) (text, from string, err error) {
	var reasons []string
	var failed []Mirror
	for k, m := range f.mirrors {
		f.where = "从「" + m.Name + "」获取许可协议清单 " + licenseManifest
		t, err := f.licenseFrom(ctx, m)
		if err == nil {
			text, from = t, m.Name
			break
		}
		if errs.CodeOf(err) == errs.Canceled {
			return "", "", err
		}
		f.warn(fmt.Sprintf("从 %s 取许可协议失败：%s", m.Name, err.Error()))
		reasons = append(reasons, m.Name+"："+err.Error())
		failed = append(failed, m)
		if k < len(f.mirrors)-1 {
			f.say("  换下一个镜像")
		}
	}
	if text == "" {
		f.where = "获取许可协议清单 " + licenseManifest + "，试过的镜像都不行"
		return "", "", errs.New(errs.ComponentsUnavailable, "取不到《Android SDK 许可协议》全文，什么也没有下载（各镜像的情况："+
			strings.Join(reasons, "；")+"）").WithAdvice(netAdvice("请检查网络后重试"))
	}
	if len(failed) > 0 {
		var names []string
		order := make([]Mirror, 0, len(f.mirrors))
		for _, m := range f.mirrors {
			if !containsMirror(failed, m) {
				order = append(order, m)
			}
		}
		for _, m := range failed {
			order = append(order, m)
			names = append(names, m.Name)
		}
		f.mirrors = order
		f.say(fmt.Sprintf("  下载时先用能取到清单的镜像，%s 排到最后", strings.Join(names, "、")))
	}
	return text, from, nil
}

func containsMirror(list []Mirror, m Mirror) bool {
	for _, x := range list {
		if x == m {
			return true
		}
	}
	return false
}

func (f *fetcher) licenseFrom(ctx context.Context, m Mirror) (string, error) {
	body, err := f.getBytes(ctx, m.Base+licenseManifest, "取许可协议清单 "+licenseManifest)
	if err != nil {
		return "", err
	}
	return licenseText(body, licenseID)
}

func (f *fetcher) getBytes(ctx context.Context, rawURL, what string) ([]byte, error) {
	return f.downloader().Bytes(ctx, rawURL, what, maxManifestBytes, f.o.ManifestTimeout)
}

// licenseText is the text of the first <license id="..."> anywhere in the document (the script's
// //*[local-name()='license' and @id=...]), trimmed. The whole document must parse.
func licenseText(data []byte, id string) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	var text strings.Builder
	found, depth := false, 0
	for {
		tok, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("清单不是有效的 XML：%v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth > 0 {
				depth++
			} else if !found && t.Name.Local == "license" && attr(t, "id") == id {
				found, depth = true, 1
			}
		case xml.EndElement:
			if depth > 0 {
				depth--
			}
		case xml.CharData:
			if depth > 0 {
				text.Write(t)
			}
		}
	}
	s := strings.TrimSpace(text.String())
	if s == "" {
		return "", errors.New("清单里没有许可协议 " + id)
	}
	return s, nil
}

func attr(e xml.StartElement, name string) string {
	for _, a := range e.Attr {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

// ---- component zips --------------------------------------------------------------------------------------------

func (f *fetcher) zipPath(c root.SDKComponent) string {
	if c.ZipName != "" {
		return filepath.Join(f.r.Dir, "downloads", c.ZipName)
	}
	return filepath.Join(f.r.Dir, "downloads", path.Base(c.URL))
}

// download gets c's zip into downloads\ (the script's Get-ComponentZip): a zip an earlier run left is used when its
// size and SHA-1 are right. Otherwise each mirror in turn, up to downloadAttempts tries each, into <zip>.part, which is
// checked for SHA-1 and then renamed. The .part survives a failed try and a change of mirror: the next one resumes it.
func (f *fetcher) download(ctx context.Context, c root.SDKComponent) (string, error) {
	zipFile := f.zipPath(c)
	if err := os.MkdirAll(filepath.Dir(zipFile), 0o755); err != nil {
		return "", errs.Wrap(errs.Internal, err, "建不了 downloads 目录："+err.Error())
	}
	part := zipFile + ".part"
	name := filepath.Base(zipFile)
	if st, err := os.Stat(zipFile); err == nil {
		f.where = "校验上次下好的 " + name
		f.say(fmt.Sprintf("  发现上次下好的 %s，校验中…", name))
		if sha, err := fileSHA1(zipFile); err == nil && st.Size() == c.Size && sha == c.SHA1 {
			f.say("  校验通过，直接用")
			return zipFile, nil
		}
		f.warn("  校验不通过，重新下载")
		if err := os.Remove(zipFile); err != nil {
			return "", errs.Wrap(errs.Internal, err, "删不掉 "+zipFile+"："+err.Error())
		}
	}
	// Validate an already complete local part before attributing any failure to a mirror.
	if st, err := os.Stat(part); err == nil && st.Size() == c.Size {
		sha, err := fileSHA1(part)
		if err != nil {
			return "", errs.Wrap(errs.Internal, err, "读不了 "+part+"："+err.Error())
		}
		if sha == c.SHA1 {
			f.say("  校验通过，直接用")
			if err := os.Rename(part, zipFile); err != nil {
				return "", err
			}
			return zipFile, nil
		}
		f.warn("  上次留下的 .part SHA-1 不对，丢弃后重新下载")
		if err := os.Remove(part); err != nil {
			return "", err
		}
	}
	var reasons []string
	rejected := true // every mirror delivered a whole file with the wrong SHA-1
	only416 := true  // every try was refused with HTTP 416: no mirror can continue the part
	for _, m := range f.mirrors {
		target := m.Base + c.URL
		f.say("  镜像：" + m.Name)
		f.note("  URL " + target)
		ok := false
		for attempt := 1; attempt <= downloadAttempts; attempt++ {
			f.where = "从「" + m.Name + "」下载 " + c.URL
			err := f.downloadOne(ctx, target, part, c)
			if err == nil {
				ok = true
				break
			}
			if errs.CodeOf(err) == errs.Canceled {
				return "", err
			}
			f.warn(fmt.Sprintf("  第 %d 次下载出错（%s）：%s", attempt, m.Name, err.Error()))
			reasons = append(reasons, fmt.Sprintf("%s 下载：%s", m.Name, err.Error()))
			var fe *download.Error
			if !errors.As(err, &fe) || fe.Status != http.StatusRequestedRangeNotSatisfiable {
				only416 = false
			}
			if switchMirror(err) {
				break
			}
			if attempt < downloadAttempts {
				f.say("  3 秒后重试（已下载的部分保留，接着下）")
				select {
				case <-time.After(retryDelay):
				case <-ctx.Done():
					return "", canceled(ctx)
				}
			}
		}
		if !ok {
			rejected = false
			f.warn(fmt.Sprintf("  %s 下载失败，换下一个镜像", m.Name))
			continue
		}
		f.where = "校验从「" + m.Name + "」下载的 " + c.URL
		f.say("  校验 SHA-1…")
		sha, err := fileSHA1(part)
		if err != nil {
			return "", errs.Wrap(errs.Internal, err, "读不了 "+part+"："+err.Error())
		}
		if sha != c.SHA1 {
			f.warn(fmt.Sprintf("  SHA-1 不对：%s，应为 %s；丢弃，换下一个镜像", sha, c.SHA1))
			reasons = append(reasons, fmt.Sprintf("%s 校验：SHA-1 为 %s，应为 %s", m.Name, sha, c.SHA1))
			only416 = false
			os.Remove(part)
			continue
		}
		f.say("  校验通过")
		if err := os.Rename(part, zipFile); err != nil {
			return "", errs.Wrap(errs.Internal, err, "改不了名："+part+"："+err.Error())
		}
		return zipFile, nil
	}
	f.where = "下载 " + c.URL
	code, advice := errs.ComponentsUnavailable, netAdvice("检查网络后再运行一次")
	if rejected {
		code, advice = errs.ComponentsRejected, "检查网络后再运行一次"
	}
	if only416 && len(reasons) > 0 {
		// Every mirror's file is shorter than the part: kept as it may be valid, but no run would get past it.
		advice = "所有镜像都不能从已下的部分接着下；删掉 " + part + " 后再运行一次"
	}
	return "", errs.New(code, "所有镜像都没下成："+strings.Join(reasons, "；")).WithAdvice(advice)
}

// netAdvice is the advice for mirrors that could not be reached: the HTTPS_PROXY hint when the Internet settings use
// an auto-config script (hostproxy.Advice), else base.
func netAdvice(base string) string {
	if a := hostproxy.Advice(); a != "" {
		return a
	}
	return base
}

func (f *fetcher) downloadOne(ctx context.Context, rawURL, part string, c root.SDKComponent) error {
	var m *meter
	var offset int64
	start := time.Now()
	err := f.downloader().File(ctx, download.FileOptions{URL: rawURL, Path: part, Size: c.Size, Label: "下载 " + c.FetchName,
		Start: func(n int64, restarted bool) {
			offset = n
			if restarted {
				f.warn("  这个镜像不支持断点续传，从头下载")
			} else if n > 0 {
				f.say(fmt.Sprintf("  从 %s 处接着下", formatSize(float64(n))))
			}
			f.note(fmt.Sprintf("  下载开始：从第 %d 字节起，共 %d 字节", n, c.Size))
			m = newMeter(f.o.Progress, "downloading", c.FetchName, c.Size, n)
		}, Progress: func(done int64) { m.add(done) },
	})
	if err == nil && m != nil {
		m.finish(c.Size)
		f.note(fmt.Sprintf("  下载结束：%d 字节，用时 %s 秒", c.Size-offset, fixed(time.Since(start).Seconds(), 1)))
	}
	return err
}

// fileSHA1 is the SHA-1 of a file in lower hex.
func fileSHA1(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	h := sha1.New()
	if _, err := io.CopyBuffer(h, fh, make([]byte, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// clampMB is mb MiB in bytes without overflowing.
func clampMB(mb int64) int64 {
	if mb > math.MaxInt64>>20 {
		return math.MaxInt64
	}
	return mb << 20
}
