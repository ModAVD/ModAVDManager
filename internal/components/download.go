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
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"modavd/internal/errs"
	"modavd/internal/hostproxy"
	"modavd/internal/root"
)

const (
	userAgent        = "ModAVD-fetch-components/1.0"
	licenseManifest  = "repository2-3.xml"
	licenseID        = "android-sdk-license"
	maxManifestBytes = 16 << 20 // the real one is about 420 KB
	maxRedirects     = 10
	downloadAttempts = 3
)

// retryDelay is the script's pause between two attempts on one mirror (a variable for tests).
var retryDelay = 3 * time.Second

// fetchError is a failed attempt. next: try the next mirror at once instead of this one again, the script's
// '大小和预期不符|HTTP 4\d\d|压缩过的内容|^超时' (the .part stays, the next mirror resumes it).
type fetchError struct {
	msg    string
	next   bool
	status int // HTTP status of a non-2xx answer, 0 otherwise
}

func (e *fetchError) Error() string { return e.msg }

func timeoutError(format string, args ...any) error {
	return &fetchError{msg: fmt.Sprintf(format, args...), next: true}
}

func sizeError(format string, args ...any) error {
	return &fetchError{msg: fmt.Sprintf(format, args...), next: true}
}

func canceled(ctx context.Context) error { return errs.Wrap(errs.Canceled, ctx.Err(), "已取消") }

// switchMirror: the error says to go on with the next mirror now.
func switchMirror(err error) bool {
	var fe *fetchError
	return errors.As(err, &fe) && fe.next
}

func (f *fetcher) httpClient() *http.Client {
	if f.client == nil {
		d := f.o.ConnectTimeout
		f.client = &http.Client{
			Transport: &http.Transport{
				Proxy:                 hostproxy.ForRequest,
				DialContext:           (&net.Dialer{Timeout: d}).DialContext,
				TLSHandshakeTimeout:   d,
				ResponseHeaderTimeout: d,
				// The bytes as the server has them (see get), and no connection reused: a mirror that keeps sending after
				// a refused or short answer would otherwise make net/http print "Unsolicited response received on idle
				// HTTP channel" on stderr, in the middle of the --json log lines.
				DisableCompression: true,
				DisableKeepAlives:  true,
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxRedirects {
					return fmt.Errorf("重定向超过 %d 次", maxRedirects)
				}
				return nil
			},
		}
	}
	return f.client
}

// response is a 2xx answer whose body is read with a watchdog: one timer per request, reset for each read.
type response struct {
	*http.Response
	cancel context.CancelFunc
	timer  *time.Timer
	fired  atomic.Bool // the timer ended the request
}

// read is one Read of the body, given at most d to return.
func (r *response) read(buf []byte, d time.Duration) (int, error) {
	r.timer.Reset(d)
	n, err := r.Body.Read(buf)
	r.timer.Stop()
	return n, err
}

func (r *response) close() {
	r.timer.Stop()
	r.Body.Close()
	r.cancel()
}

// get sends a GET for the raw bytes (Accept-Encoding: identity: dl.google.com gzips the zip again for a client that
// accepts gzip, and a range resume then lands in the compressed stream) from offset when > 0. Connect, TLS handshake,
// redirects and the response headers together get ConnectTimeout (the script's Get-Response). Any answer but a 2xx
// without Content-Encoding is an error and its connection is closed.
func (f *fetcher) get(ctx context.Context, rawURL string, offset int64, what string) (*response, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, &fetchError{msg: "地址不对：" + rawURL}
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", userAgent)
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp := &response{cancel: cancel}
	resp.timer = time.AfterFunc(f.o.ConnectTimeout, func() { resp.fired.Store(true); cancel() })
	hr, err := f.httpClient().Do(req)
	resp.timer.Stop()
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		var ne net.Error
		if resp.fired.Load() || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, timeoutError("超时：%s（等服务器回应），%s 秒内没有进展", what, seconds(f.o.ConnectTimeout))
		}
		return nil, &fetchError{msg: "连接失败：" + netErrText(err)}
	}
	resp.Response = hr
	if hr.StatusCode < 200 || hr.StatusCode > 299 {
		resp.close()
		code := hr.StatusCode
		return nil, &fetchError{msg: fmt.Sprintf("服务器返回 HTTP %d", code), next: code >= 400 && code <= 499, status: code}
	}
	if ce := hr.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		resp.close()
		return nil, &fetchError{msg: "服务器返回了压缩过的内容（Content-Encoding: " + ce + "），没法校验和续传", next: true}
	}
	return resp, nil
}

// netErrText is a transport error without Go's "Get <url>:" prefix.
func netErrText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return err.Error()
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

// getBytes reads a whole small answer: ConnectTimeout for the headers, then StallTimeout for each read and
// ManifestTimeout for the body as a whole (the script's Get-Bytes), at most maxManifestBytes.
func (f *fetcher) getBytes(ctx context.Context, rawURL, what string) ([]byte, error) {
	resp, err := f.get(ctx, rawURL, 0, what)
	if err != nil {
		return nil, err
	}
	defer resp.close()
	deadline := time.Now().Add(f.o.ManifestTimeout)
	var b bytes.Buffer
	buf := make([]byte, 64<<10)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nil, timeoutError("超时：%s，整个过程超过 %s 秒还没取完", what, seconds(f.o.ManifestTimeout))
		}
		wait := min(f.o.StallTimeout, left)
		n, rerr := resp.read(buf, wait)
		b.Write(buf[:n])
		if b.Len() > maxManifestBytes {
			return nil, sizeError("%s 大得不正常（超过 %d MB）", licenseManifest, maxManifestBytes>>20)
		}
		if rerr == io.EOF {
			return b.Bytes(), nil
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return nil, canceled(ctx)
			}
			if resp.fired.Load() {
				if !time.Now().Before(deadline) {
					return nil, timeoutError("超时：%s，整个过程超过 %s 秒还没取完", what, seconds(f.o.ManifestTimeout))
				}
				return nil, timeoutError("超时：%s（读数据），%s 秒内没有进展", what, seconds(wait.Round(time.Millisecond)))
			}
			return nil, &fetchError{msg: "收数据出错：" + netErrText(rerr)}
		}
	}
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
	var reasons []string
	rejected := true // every mirror delivered a whole file with the wrong SHA-1
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

// downloadOne is one try (the script's Invoke-Download): it resumes part from its size, checks status and headers
// before touching it, and fails unless part ends up with exactly c.Size bytes. A part already that size is left to
// the SHA-1 check without a request.
func (f *fetcher) downloadOne(ctx context.Context, rawURL, part string, c root.SDKComponent) error {
	expected, label := c.Size, c.FetchName
	what := "下载 " + label
	existing := int64(0)
	if st, err := os.Stat(part); err == nil {
		existing = st.Size()
	}
	if existing > expected {
		os.Remove(part)
		existing = 0
	}
	if existing == expected {
		return nil
	}
	resp, err := f.get(ctx, rawURL, existing, what)
	var fe *fetchError
	if existing > 0 && errors.As(err, &fe) && fe.status == http.StatusRequestedRangeNotSatisfiable {
		// The mirror cannot continue from there (its file is shorter, or it counts differently): start over.
		f.warn(fmt.Sprintf("  这个镜像不能从 %s 处接着下（HTTP 416），删掉已下的部分，从头下载", formatSize(float64(existing))))
		os.Remove(part)
		existing = 0
		resp, err = f.get(ctx, rawURL, 0, what)
	}
	if err != nil {
		return err
	}
	defer resp.close()
	status, contentRange := resp.StatusCode, resp.Header.Get("Content-Range")
	resume := false
	if existing > 0 {
		switch {
		case status == http.StatusPartialContent && strings.HasPrefix(contentRange, fmt.Sprintf("bytes %d-", existing)):
			if contentRange != fmt.Sprintf("bytes %d-%d/%d", existing, expected-1, expected) {
				return sizeError("文件大小和预期不符（Content-Range: %s，应为 %d 字节）", contentRange, expected)
			}
			resume = true
		case status != http.StatusOK:
			return &fetchError{msg: fmt.Sprintf("断点续传的回应不对（HTTP %d，Content-Range: %s）", status, contentRange)}
		}
	} else if status != http.StatusOK {
		return &fetchError{msg: fmt.Sprintf("服务器返回 HTTP %d", status)}
	}
	if status == http.StatusOK && resp.ContentLength >= 0 && resp.ContentLength != expected {
		return sizeError("文件大小和预期不符（服务器给 %d 字节，应为 %d 字节）", resp.ContentLength, expected)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	switch {
	case resume:
		flags = os.O_WRONLY | os.O_APPEND
		f.say(fmt.Sprintf("  从 %s 处接着下", formatSize(float64(existing))))
	case existing > 0:
		f.warn("  这个镜像不支持断点续传，从头下载")
		existing = 0
	}
	fh, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return &fetchError{msg: "写不了 " + part + "：" + err.Error()}
	}
	defer fh.Close()

	f.note(fmt.Sprintf("  下载开始：从第 %d 字节起，共 %d 字节", existing, expected))
	start := time.Now()
	buf := make([]byte, 1<<20)
	done := existing
	m := newMeter(f.o.Progress, "downloading", label, expected, existing)
	for {
		n, rerr := resp.read(buf, f.o.StallTimeout)
		if n > 0 {
			if done+int64(n) > expected {
				return sizeError("文件大小和预期不符（收到的比 %d 字节多）", expected)
			}
			if _, err := fh.Write(buf[:n]); err != nil {
				return &fetchError{msg: "写不了 " + part + "：" + err.Error()}
			}
			done += int64(n)
			m.add(done)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return canceled(ctx)
			}
			if resp.fired.Load() {
				return timeoutError("超时：%s（收数据，已下 %s），%s 秒内没有进展", what, formatSize(float64(done)), seconds(f.o.StallTimeout))
			}
			return &fetchError{msg: "收数据出错：" + netErrText(rerr)}
		}
	}
	if err := fh.Close(); err != nil {
		return &fetchError{msg: "写不了 " + part + "：" + err.Error()}
	}
	m.finish(done)
	f.note(fmt.Sprintf("  下载结束：%d 字节，用时 %s 秒", done-existing, fixed(time.Since(start).Seconds(), 1)))
	if st, err := os.Stat(part); err != nil || st.Size() != expected {
		size := int64(0)
		if err == nil {
			size = st.Size()
		}
		return &fetchError{msg: fmt.Sprintf("下载不完整：应为 %d 字节，实际 %d 字节", expected, size)}
	}
	return nil
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
