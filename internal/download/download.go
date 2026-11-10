// Package download owns HTTP timeouts, raw-byte transfers and validated file resumption.
// Callers own retries, mirrors, hashes and installation.
package download

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/errs"
	"modavd/internal/hostproxy"
)

const (
	DefaultConnectTimeout = 20 * time.Second
	DefaultStallTimeout   = 30 * time.Second
	MaxRedirects          = 10
)

type Kind uint8

const (
	Network Kind = iota
	Timeout
	Size
	Encoding
)

type Error struct {
	Message string
	Kind    Kind
	Status  int
}

func (e *Error) Error() string { return e.Message }
func timeoutError(format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...), Kind: Timeout}
}
func sizeError(format string, args ...any) error {
	return &Error{Message: fmt.Sprintf(format, args...), Kind: Size}
}
func canceled(ctx context.Context) error { return errs.Wrap(errs.Canceled, ctx.Err(), "已取消") }
func seconds(d time.Duration) string     { return strconv.FormatFloat(d.Seconds(), 'f', -1, 64) }
func UserAgent() string                  { return "ModAVDManager/" + buildinfo.Version }

// NewHTTPClient also serves version API requests. Each request closes its connection to keep net/http's
// unsolicited-response warnings out of the CLI's JSON stream.
func NewHTTPClient(connect time.Duration) *http.Client {
	if connect == 0 {
		connect = DefaultConnectTimeout
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:               hostproxy.ForRequest,
		DialContext:         (&net.Dialer{Timeout: connect}).DialContext,
		TLSHandshakeTimeout: connect, ResponseHeaderTimeout: connect,
		DisableCompression: true, DisableKeepAlives: true,
	}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= MaxRedirects {
			return fmt.Errorf("重定向超过 %d 次", MaxRedirects)
		}
		return nil
	}}
}

// Client performs one attempt. HTTP is required; callers can share it with their version API requests.
type Client struct {
	HTTP                         *http.Client
	ConnectTimeout, StallTimeout time.Duration
}

func (c *Client) connectTimeout() time.Duration {
	if c.ConnectTimeout > 0 {
		return c.ConnectTimeout
	}
	return DefaultConnectTimeout
}
func (c *Client) stallTimeout() time.Duration {
	if c.StallTimeout > 0 {
		return c.StallTimeout
	}
	return DefaultStallTimeout
}

// NetError removes the request URL and redacts both literal and URL-encoded CDKs from nested transport errors.
func NetError(err error, cdk string) string {
	for {
		var ue *url.Error
		if !errors.As(err, &ue) {
			break
		}
		err = ue.Err
	}
	s := err.Error()
	if cdk != "" {
		for _, secret := range []string{url.QueryEscape(cdk), url.PathEscape(cdk), cdk} {
			s = strings.ReplaceAll(s, secret, "***")
		}
	}
	return s
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
func (c *Client) get(ctx context.Context, rawURL string, offset int64, what string) (*response, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		cancel()
		return nil, &Error{Message: "下载地址不对"}
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", UserAgent())
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp := &response{cancel: cancel}
	resp.timer = time.AfterFunc(c.connectTimeout(), func() { resp.fired.Store(true); cancel() })
	hr, err := c.HTTP.Do(req)
	resp.timer.Stop()
	if err != nil {
		cancel()
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		var ne net.Error
		if resp.fired.Load() || (errors.As(err, &ne) && ne.Timeout()) {
			return nil, timeoutError("超时：%s（等服务器回应），%s 秒内没有进展", what, seconds(c.connectTimeout()))
		}
		return nil, &Error{Message: "连接失败：" + NetError(err, "")}
	}
	resp.Response = hr
	if hr.StatusCode < 200 || hr.StatusCode > 299 {
		resp.close()
		code := hr.StatusCode
		return nil, &Error{Message: fmt.Sprintf("服务器返回 HTTP %d", code), Status: code}
	}
	if ce := hr.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		resp.close()
		return nil, &Error{Message: "服务器返回了压缩过的内容（Content-Encoding: " + ce + "），没法校验和续传", Kind: Encoding}
	}
	return resp, nil
}

// Bytes reads a small response with a body deadline and a byte limit, in addition to the connection and stall limits.
func (c *Client) Bytes(ctx context.Context, rawURL, what string, maxBytes int, overall time.Duration) ([]byte, error) {
	resp, err := c.get(ctx, rawURL, 0, what)
	if err != nil {
		return nil, err
	}
	defer resp.close()
	deadline := time.Now().Add(overall)
	var b bytes.Buffer
	buf := make([]byte, 64<<10)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return nil, timeoutError("超时：%s，整个过程超过 %s 秒还没取完", what, seconds(overall))
		}
		wait := min(c.stallTimeout(), left)
		n, rerr := resp.read(buf, wait)
		b.Write(buf[:n])
		if b.Len() > maxBytes {
			return nil, sizeError("%s 大得不正常（超过 %d MB）", what, maxBytes>>20)
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
					return nil, timeoutError("超时：%s，整个过程超过 %s 秒还没取完", what, seconds(overall))
				}
				return nil, timeoutError("超时：%s（读数据），%s 秒内没有进展", what, seconds(wait.Round(time.Millisecond)))
			}
			return nil, &Error{Message: "收数据出错：" + NetError(rerr, "")}
		}
	}
}

// FileOptions describes the expected raw file. Start runs after headers are validated and the file is opened;
// restarted means a range request received 200. Progress counts all bytes, including a validated resume prefix.
type FileOptions struct {
	URL, Path, Label string
	Size             int64
	Fresh            bool
	Start            func(offset int64, restarted bool)
	Progress         func(done int64)
	// BeforeWrite can stop a transfer before opening the output or writing the
	// next chunk (for example when the target volume runs out of space).
	BeforeWrite func() error
}

// File resumes from the existing file size and validates the response before writing. Success requires exactly
// Size bytes. A complete existing file is left for the caller's hash check, unless Fresh requires a new transfer.
func (c *Client) File(ctx context.Context, o FileOptions) error {
	rawURL, part, expected, what := o.URL, o.Path, o.Size, o.Label
	existing := int64(0)
	if st, err := os.Stat(part); err == nil {
		existing = st.Size()
	}
	if o.Fresh || existing > expected {
		if err := os.Remove(part); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		existing = 0
	}
	if !o.Fresh && existing == expected {
		return nil
	}
	resp, err := c.get(ctx, rawURL, existing, what)
	var fe *Error
	if existing > 0 && errors.As(err, &fe) && fe.Status == http.StatusRequestedRangeNotSatisfiable {
		// The response cannot establish that the local prefix is invalid. Preserve it for another source or attempt.
		return &Error{Message: fmt.Sprintf("服务器不能从 %d 字节处接着下（HTTP 416），已下的部分留着", existing), Status: fe.Status}
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
			return &Error{Message: fmt.Sprintf("断点续传的回应不对（HTTP %d，Content-Range: %s）", status, contentRange)}
		}
	} else if status != http.StatusOK {
		return &Error{Message: fmt.Sprintf("服务器返回 HTTP %d", status)}
	}
	if status == http.StatusOK && resp.ContentLength >= 0 && resp.ContentLength != expected {
		return sizeError("文件大小和预期不符（服务器给 %d 字节，应为 %d 字节）", resp.ContentLength, expected)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if o.BeforeWrite != nil {
		if err := o.BeforeWrite(); err != nil {
			return err
		}
	}
	restarted := existing > 0 && status == http.StatusOK
	switch {
	case resume:
		flags = os.O_WRONLY | os.O_APPEND
	case existing > 0:
		existing = 0
	}
	fh, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return &Error{Message: "写不了 " + part + "：" + err.Error()}
	}
	defer fh.Close()

	if o.Start != nil {
		o.Start(existing, restarted)
	}
	buf := make([]byte, 1<<20)
	done := existing

	for {
		n, rerr := resp.read(buf, c.stallTimeout())
		if n > 0 {
			if o.BeforeWrite != nil {
				if err := o.BeforeWrite(); err != nil {
					return err
				}
			}
			if done+int64(n) > expected {
				return sizeError("文件大小和预期不符（收到的比 %d 字节多）", expected)
			}
			if _, err := fh.Write(buf[:n]); err != nil {
				return &Error{Message: "写不了 " + part + "：" + err.Error()}
			}
			done += int64(n)
			if o.Progress != nil {
				o.Progress(done)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() != nil {
				return canceled(ctx)
			}
			if resp.fired.Load() {
				return timeoutError("超时：%s（收数据，已下 %s），%s 秒内没有进展", what, fmt.Sprintf("%d 字节", done), seconds(c.stallTimeout()))
			}
			return &Error{Message: "收数据出错：" + NetError(rerr, "")}
		}
	}
	if err := fh.Sync(); err != nil {
		return err
	}
	if err := fh.Close(); err != nil {
		return &Error{Message: "写不了 " + part + "：" + err.Error()}
	}

	if st, err := os.Stat(part); err != nil || st.Size() != expected {
		size := int64(0)
		if err == nil {
			size = st.Size()
		}
		return &Error{Message: fmt.Sprintf("下载不完整：应为 %d 字节，实际 %d 字节", expected, size)}
	}
	return nil
}
