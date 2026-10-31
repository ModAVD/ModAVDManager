//go:build windows

package update

// Where a new ModAVD comes from: GitHub's latest release of ModAVD/ModAVD, or MirrorChyan (RID ModAVD, win / x64).
// AUTO-MAS update_source.py, with the same messages; the CDK goes only into MirrorChyan's query and never into an
// error, a log line or state.json.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/errs"
	"modavd/internal/jsonord"
)

const (
	githubAPI    = "https://api.github.com/repos/ModAVD/ModAVD/releases/latest"
	mirrorAPI    = "https://mirrorchyan.com/api/resources"
	mirrorRID    = "ModAVD"
	apiTimeout   = 15 * time.Second
	maxAPIBytes  = 4 << 20
	userAgentApp = "ModAVDManager"
)

// release is a version to install and the package to download for it.
type release struct {
	source      string // SourceGitHub / SourceMirrorChyan
	tag         string // GitHub tag_name or MirrorChyan version_name
	version     Version
	minAutoMAS  string
	assetName   string
	url         string // "" when MirrorChyan gave none (no CDK)
	size        int64
	sha256      string // lower hex, "" when unknown
	topDir      string // legacy GitHub packages only
	incremental bool
	base        Version // incremental only
	pageURL     string
}

// target is the release as state.json's "target" (the MirrorChyan URL, which is good for one download, is never
// written).
func (r *release) target() *jsonord.Obj {
	asset := jsonord.NewObject().Obj()
	asset.Set("name", jsonord.NewString(r.assetName))
	if r.source == SourceMirrorChyan {
		asset.Set("url", jsonord.NewString(""))
	} else {
		asset.Set("url", jsonord.NewString(r.url))
	}
	asset.Set("size", jsonord.NewInt(r.size))
	if r.sha256 != "" {
		asset.Set("sha256", jsonord.NewString(r.sha256))
	} else {
		asset.Set("sha256", jsonord.NewNull())
	}
	asset.Set("topDir", jsonord.NewString(r.topDir))
	asset.Set("incremental", jsonord.NewBool(r.incremental))
	base := ""
	if r.incremental {
		base = r.base.String()
	}
	asset.Set("base", jsonord.NewString(base))
	t := jsonord.NewObject().Obj()
	t.Set("source", jsonord.NewString(r.source))
	t.Set("tag", jsonord.NewString(r.tag))
	t.Set("version", jsonord.NewString(r.version.String()))
	t.Set("asset", objVal(asset))
	return t
}

// sourceError: a source could not be asked or answered unusably (offline, rate limited, HTTP error).
func sourceError(format string, args ...any) *errs.Error {
	return errs.Newf(errs.UpdateUnavailable, format, args...)
}

func (u *Updater) client() *http.Client {
	if u.httpClient != nil {
		return u.httpClient
	}
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment, // HTTPS_PROXY / NO_PROXY; Go does not read the system proxy
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		ForceAttemptHTTP2:     true,
	}}
}

func userAgent() string { return userAgentApp + "/" + buildinfo.Version }

// ---- GitHub

func (u *Updater) githubLatest(ctx context.Context) (*release, error) {
	api := u.githubAPI
	if api == "" {
		api = githubAPI
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, sourceError("连不上 GitHub（%s）", netErrText(err, ""))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		hint := ""
		if resp.StatusCode == 403 || resp.StatusCode == 429 {
			hint = "，可能是查询次数超过了 GitHub 的限制，稍后再试"
		}
		return nil, sourceError("GitHub 返回 HTTP %d%s", resp.StatusCode, hint)
	}
	var body struct {
		TagName string `json:"tag_name"`
		Body    string `json:"body"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Size   int64  `json:"size"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if err != nil || json.Unmarshal(b, &body) != nil {
		return nil, sourceError("GitHub 返回的内容不是 JSON")
	}
	v, ok := ParseVersion(body.TagName)
	if !ok {
		return nil, sourceError("认不出最新版本的 tag: %q", body.TagName)
	}
	ver := strings.TrimPrefix(strings.TrimSpace(body.TagName), "v")
	name, top := "ModAVD-"+ver+"-"+platform+".zip", ""
	if !requiresManifest(body.TagName) {
		name, top = "ModAVD-"+ver+".zip", "ModAVD-"+ver
	}
	r := &release{source: SourceGitHub, tag: strings.TrimSpace(body.TagName), version: v,
		minAutoMAS: parseMinAutoMAS(body.Body), topDir: top, pageURL: body.HTMLURL}
	for _, a := range body.Assets {
		if a.Name != name {
			continue
		}
		if a.URL == "" || a.Size <= 0 {
			return nil, sourceError("GitHub 返回的附件信息不完整: %s", a.Name)
		}
		r.assetName, r.url, r.size = a.Name, a.URL, a.Size
		if m := digestRe.FindStringSubmatch(a.Digest); m != nil {
			r.sha256 = strings.ToLower(m[1])
		}
		return r, nil
	}
	return nil, sourceError("%s 里没有本机（%s）能用的包 %s", r.tag, platform, name)
}

var digestRe = regexp.MustCompile(`^sha256:([0-9a-fA-F]{64})$`)

// ---- MirrorChyan

// mirrorMessages are MirrorChyan's error codes (AUTO-MAS MIRROR_ERROR_INFO).
var mirrorMessages = map[int]string{
	1001: "获取版本信息的URL参数不正确", 7001: "填入的 CDK 已过期", 7002: "填入的 CDK 错误",
	7003: "填入的 CDK 今日下载次数已达上限", 7004: "填入的 CDK 类型和待下载的资源不匹配", 7005: "填入的 CDK 已被封禁",
	8001: "对应架构和系统下的资源不存在", 8002: "错误的系统参数", 8003: "错误的架构参数", 8004: "错误的更新通道参数",
	1: "未知错误类型",
}

// mirrorError is a MirrorChyan failure; Code is its business code (0 for transport problems).
type mirrorError struct {
	Code int
	Msg  string
}

func (e *mirrorError) Error() string { return e.Msg }

// mirrorLatest asks MirrorChyan for the latest version. cdk "" is the check (no URL, costs nothing); with a CDK it
// is the download request, which counts against the CDK. base, when set, asks for an incremental package from it.
func (u *Updater) mirrorLatest(ctx context.Context, cdk string, base *Version) (*release, error) {
	api := u.mirrorAPI
	if api == "" {
		api = mirrorAPI
	}
	q := url.Values{}
	q.Set("user_agent", userAgentApp)
	q.Set("os", "win")
	q.Set("arch", "x64")
	q.Set("channel", "stable")
	if base != nil {
		q.Set("current_version", base.String())
	}
	if cdk != "" {
		q.Set("cdk", cdk)
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(api, "/")+"/"+mirrorRID+"/latest?"+q.Encode(), nil)
	req.Header.Set("User-Agent", userAgent())
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, &mirrorError{Msg: "连不上 Mirror 酱（" + netErrText(err, cdk) + "）"}
	}
	defer resp.Body.Close()
	var payload struct {
		Code *json.RawMessage `json:"code"`
		Msg  string           `json:"msg"`
		Data json.RawMessage  `json:"data"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBytes))
	if err != nil || json.Unmarshal(b, &payload) != nil {
		return nil, &mirrorError{Msg: fmt.Sprintf("Mirror 酱的回答读不懂（HTTP %d）", resp.StatusCode)}
	}
	code := 0
	if payload.Code == nil || json.Unmarshal(*payload.Code, &code) != nil {
		return nil, &mirrorError{Msg: "Mirror 酱响应 code 非整数"}
	}
	if code != 0 {
		msg, ok := mirrorMessages[code]
		if !ok {
			msg = fmt.Sprintf("错误码 %d", code)
		}
		return nil, &mirrorError{Code: code, Msg: msg}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &mirrorError{Msg: fmt.Sprintf("Mirror 酱 HTTP 响应异常: %d", resp.StatusCode)}
	}
	var d struct {
		VersionName string  `json:"version_name"`
		ReleaseNote string  `json:"release_note"`
		URL         string  `json:"url"`
		Filesize    *int64  `json:"filesize"`
		UpdateType  string  `json:"update_type"`
		SHA256      *string `json:"sha256"`
	}
	if json.Unmarshal(payload.Data, &d) != nil {
		return nil, &mirrorError{Msg: "Mirror 酱返回的数据不完整"}
	}
	v, ok := ParseVersion(d.VersionName)
	if !ok {
		return nil, &mirrorError{Msg: fmt.Sprintf("认不出 Mirror 酱上最新版本的版本号: %q", d.VersionName)}
	}
	r := &release{source: SourceMirrorChyan, tag: strings.TrimSpace(d.VersionName), version: v,
		minAutoMAS: parseMinAutoMAS(d.ReleaseNote)}
	if d.URL == "" {
		return r, nil
	}
	if d.Filesize == nil || *d.Filesize <= 0 {
		return nil, &mirrorError{Msg: "Mirror 酱给了下载地址，但没给包的大小"}
	}
	r.url, r.size = d.URL, *d.Filesize
	if d.SHA256 != nil && sha256Hex.MatchString(strings.ToLower(strings.TrimSpace(*d.SHA256))) {
		r.sha256 = strings.ToLower(strings.TrimSpace(*d.SHA256))
	}
	if d.UpdateType == "incremental" {
		if base == nil {
			return nil, &mirrorError{Msg: "Mirror 酱给了增量包，但请求没带当前版本"}
		}
		r.incremental, r.base = true, *base
		r.assetName = fmt.Sprintf("ModAVD-%s-to-%s-%s-mirrorchyan.zip", base.Display(), v.Display(), platform)
	} else {
		r.assetName = fmt.Sprintf("ModAVD-%s-%s-mirrorchyan.zip", v.Display(), platform)
	}
	return r, nil
}

// netErrText is a transport error without anything secret: the URL (which holds the CDK) is dropped.
func netErrText(err error, cdk string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := fmt.Sprintf("%T: %v", err, err)
	if cdk != "" {
		s = strings.ReplaceAll(s, cdk, "***")
	}
	return s
}

// ---- release notes

var machineLineRe = regexp.MustCompile(`^<!--\s*modavd:\s*(\{.*\})\s*-->$`)

// parseMinAutoMAS is the min_automas of the release note's machine line (its first line); "" when none.
func parseMinAutoMAS(note string) string {
	first, _, _ := strings.Cut(strings.ReplaceAll(note, "\r\n", "\n"), "\n")
	m := machineLineRe.FindStringSubmatch(strings.TrimSpace(first))
	if m == nil {
		return ""
	}
	var line map[string]any
	if json.Unmarshal([]byte(m[1]), &line) != nil {
		return ""
	}
	s, _ := line["min_automas"].(string)
	return strings.TrimSpace(s)
}

// ---- download

const downloadAttempts = 3

// download fetches r into .update\download\<asset> (resuming a GitHub .part; MirrorChyan links never resume), checks
// size and SHA-256, and returns the path. Every other file in download\ is removed first, as AUTO-MAS does: both
// programs use the same names, so neither ever keeps the other's.
func (u *Updater) download(ctx context.Context, r *release) (string, error) {
	l := u.l
	if err := os.MkdirAll(l.download(), 0o755); err != nil {
		return "", err
	}
	final := l.downloadAt(r.assetName)
	part := final + ".part"
	entries, _ := os.ReadDir(l.download())
	for _, e := range entries {
		if p := l.downloadAt(e.Name()); p != final && p != part {
			removeTree(p)
		}
	}
	if st, err := os.Stat(final); err == nil {
		if st.Size() == r.size && (r.sha256 == "" || hashOr(final) == r.sha256) {
			return final, nil
		}
		os.Remove(final)
	}
	mirror := r.source == SourceMirrorChyan
	label := "从 GitHub 下载 " + r.assetName
	if mirror {
		label = "从 Mirror 酱下载全量包 " + r.version.Display()
		if r.incremental {
			label = fmt.Sprintf("从 Mirror 酱下载增量包（%s → %s）", r.base.Display(), r.version.Display())
		}
	}
	var lastErr error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if mirror {
			os.Remove(part)
		}
		lastErr = u.fetch(ctx, r, part, label)
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			return "", errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
		}
		u.Log.Warnf("%s 第 %d 次没下完: %v", label, attempt, lastErr)
		if attempt < downloadAttempts {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return "", errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
			}
		}
	}
	if lastErr != nil {
		if mirror {
			os.Remove(part)
			return "", sourceError("下载中断，再更新一次会重新下载（从 Mirror 酱下载 %d 次都没成：%v）", downloadAttempts, lastErr)
		}
		return "", sourceError("下载 %s 失败: %v", r.assetName, lastErr)
	}
	st, err := os.Stat(part)
	if err != nil || st.Size() != r.size {
		got := int64(0)
		if err == nil {
			got = st.Size()
		}
		return "", sourceError("下载的 %s 大小不对（%d / %d 字节）", r.assetName, got, r.size)
	}
	if r.sha256 != "" && hashOr(part) != r.sha256 {
		os.Remove(part)
		from := "发布记录"
		if mirror {
			from = "Mirror 酱给"
		}
		return "", packageError("下载的 %s 和%s的 SHA-256 对不上，已删除，没有安装。请重试", r.assetName, from)
	}
	if r.sha256 == "" {
		u.Log.Warnf("%s 没有给出 SHA-256，只能靠 zip 校验和清单核对", r.assetName)
	}
	if err := os.Rename(part, final); err != nil {
		return "", err
	}
	return final, nil
}

func hashOr(p string) string {
	s, _ := hashFile(p)
	return s
}

// fetch downloads into part, appending to it when the server honours a range (GitHub).
func (u *Updater) fetch(ctx context.Context, r *release, part, label string) error {
	done := int64(0)
	if st, err := os.Stat(part); err == nil {
		done = st.Size()
		if done >= r.size {
			os.Remove(part)
			done = 0
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url, nil)
	if err != nil {
		return errors.New("下载地址不对")
	}
	req.Header.Set("User-Agent", userAgent())
	if done > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", done))
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return errors.New(netErrText(err, ""))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if resp.StatusCode == http.StatusPartialContent && done > 0 {
		flags = os.O_WRONLY | os.O_APPEND
	} else {
		done = 0
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	last := time.Now()
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if done+int64(n) > r.size {
				return fmt.Errorf("收到的比 %d 字节多", r.size)
			}
			if _, err := f.Write(buf[:n]); err != nil {
				return err
			}
			done += int64(n)
			if time.Since(last) >= 500*time.Millisecond || done == r.size {
				last = time.Now()
				u.progress("downloading", label, float64(done)*100/float64(r.size), done, r.size)
			}
		}
		if rerr == io.EOF {
			return f.Sync()
		}
		if rerr != nil {
			return errors.New(netErrText(rerr, ""))
		}
	}
}

// hashBytes is SHA-256 in lower hex.
func hashBytes(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
