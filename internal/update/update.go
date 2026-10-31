//go:build windows

// Package update implements the opt-in updater for standalone downloads. It deliberately treats every ambiguous
// installation as a ModAVD package member: a bundled manager must only report that updates arrive with ModAVD.
package update

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"modavd/internal/buildinfo"
	"modavd/internal/errs"
)

const releaseAPI = "https://api.github.com/repos/ModAVD/ModAVDManager/releases/latest"

var allowedHosts = map[string]bool{
	"api.github.com": true, "github.com": true, "release-assets.githubusercontent.com": true, "objects.githubusercontent.com": true,
}

type Options struct {
	APIURL string
	HTTP   *http.Client
	Now    func() time.Time
}

type Result struct {
	Available  bool   `json:"available"`
	Updated    bool   `json:"updated"`
	Version    string `json:"version,omitempty"`
	Message    string `json:"message"`
	Standalone bool   `json:"standalone"`
}

type marker struct {
	Version  string            `json:"version"`
	Protocol int               `json:"protocol"`
	Files    map[string]string `json:"files"`
}

type release struct {
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Immutable  bool    `json:"immutable"`
	Assets     []asset `json:"assets"`
}
type asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type processUse struct {
	PID         uint32
	CommandLine string
}

var processScanner = scanProcesses

func executableDir() (string, error) {
	e, err := os.Executable()
	if err != nil {
		return "", err
	}
	e, err = filepath.EvalSymlinks(e)
	if err != nil {
		return "", err
	}
	return filepath.Dir(e), nil
}

func standalone(dir string) (bool, marker, error) {
	var m marker
	b, err := os.ReadFile(filepath.Join(dir, "modavd-manager.json"))
	if err != nil {
		return false, m, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m.Version == "" || len(m.Files) == 0 {
		return false, m, errors.New("invalid standalone marker")
	}
	for _, name := range []string{"ModAVD.exe", filepath.Join("tools", "modavd.exe")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return false, m, err
		}
		h, err := fileHash(filepath.Join(dir, name))
		if err != nil {
			return false, m, err
		}
		if want := m.Files[strings.ReplaceAll(name, "\\", "/")]; want == "" || !strings.EqualFold(want, h) {
			return false, m, errors.New("standalone hash mismatch")
		}
	}
	for _, name := range []string{"modavd-manifest.json", "mas-avd.json", "sdk", "avd", "components", filepath.Join("tools", "modavd-bridge.ps1")} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return false, m, nil
		}
		if !os.IsNotExist(err) {
			return false, m, err
		}
	}
	return true, m, nil
}

func classify(dir string) (bool, marker, string) {
	ok, m, err := standalone(dir)
	if err != nil {
		return false, m, "有新版本时随 ModAVD 更新"
	}
	if !ok {
		return false, m, "有新版本时随 ModAVD 更新"
	}
	return true, m, ""
}

func fileHash(path string) (string, error) {
	f, err := os.Open(path)
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

func versionCompare(a, b string) int {
	parse := func(s string) [3]int {
		var v [3]int
		p := strings.Split(strings.TrimPrefix(s, "v"), ".")
		for i := range v {
			if i < len(p) {
				fmt.Sscanf(p[i], "%d", &v[i])
			}
		}
		return v
	}
	x, y := parse(a), parse(b)
	for i := range x {
		if x[i] < y[i] {
			return -1
		}
		if x[i] > y[i] {
			return 1
		}
	}
	return 0
}

func hostOK(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && allowedHosts[strings.ToLower(u.Hostname())]
}

func clientFor(opt Options) *http.Client {
	if opt.HTTP != nil {
		return opt.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !hostOK(req.URL.String()) {
			return errors.New("不允许的下载跳转")
		}
		return nil
	}}
}

func get(ctx context.Context, c *http.Client, raw string, max int64) ([]byte, error) {
	if !hostOK(raw) {
		return nil, errors.New("下载地址主机不在允许名单")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ModAVDManager/"+buildinfo.Version)
	req.Header.Set("Accept", "application/vnd.github+json")
	r, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if r.StatusCode == http.StatusForbidden || r.StatusCode == http.StatusTooManyRequests {
		return nil, errors.New("GitHub 暂时限流")
	}
	if r.StatusCode < 200 || r.StatusCode >= 300 {
		return nil, fmt.Errorf("下载失败：HTTP %d", r.StatusCode)
	}
	if r.ContentLength > max {
		return nil, errors.New("下载内容过大")
	}
	return io.ReadAll(io.LimitReader(r.Body, max+1))
}

func Check(ctx context.Context, opt Options) (*Result, error) {
	dir, err := executableDir()
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "读不了程序路径")
	}
	ok, m, msg := classify(dir)
	result := &Result{Standalone: ok}
	if !ok {
		result.Message = msg
		return result, nil
	}
	api := opt.APIURL
	if api == "" {
		api = releaseAPI
	}
	b, err := get(ctx, clientFor(opt), api, 1<<20)
	if err != nil {
		return result, nil
	}
	var rel release
	if json.Unmarshal(b, &rel) != nil || rel.Draft || rel.Prerelease || !rel.Immutable {
		return result, nil
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	if version == "" || versionCompare(version, buildinfo.Version) <= 0 {
		return result, nil
	}
	result.Available, result.Version, result.Message = true, version, "有新版本 "+version
	_ = m
	return result, nil
}

func SelfUpdate(ctx context.Context, opt Options) (*Result, error) {
	dir, err := executableDir()
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "读不了程序路径")
	}
	ok, m, msg := classify(dir)
	result := &Result{Standalone: ok}
	if !ok {
		result.Message = msg
		return result, nil
	}
	api := opt.APIURL
	if api == "" {
		api = releaseAPI
	}
	c := clientFor(opt)
	raw, err := get(ctx, c, api, 1<<20)
	if err != nil {
		return result, nil
	}
	var rel release
	if err := json.Unmarshal(raw, &rel); err != nil || rel.Draft || rel.Prerelease || !rel.Immutable {
		return result, nil
	}
	version := strings.TrimPrefix(rel.TagName, "v")
	if version == "" || versionCompare(version, buildinfo.Version) <= 0 {
		result.Message = "当前已是最新版本"
		return result, nil
	}
	zipName, sumName := "ModAVDManager-"+version+"-win-x64.zip", "SHA256SUMS.txt"
	var za, sa *asset
	for i := range rel.Assets {
		if rel.Assets[i].Name == zipName {
			za = &rel.Assets[i]
		}
		if rel.Assets[i].Name == sumName {
			sa = &rel.Assets[i]
		}
	}
	if za == nil || sa == nil || !hostOK(za.URL) || !hostOK(sa.URL) {
		return result, errors.New("Release 缺少固定名称的下载包")
	}
	sums, err := get(ctx, c, sa.URL, 1<<20)
	if err != nil {
		return result, err
	}
	wantZip, err := sumFor(string(sums), zipName)
	if err != nil {
		return result, err
	}
	zipBytes, err := get(ctx, c, za.URL, 200<<20)
	if err != nil {
		return result, err
	}
	if digest(zipBytes) != wantZip {
		return result, errors.New("下载包哈希不匹配")
	}
	files, err := unpack(zipBytes, m, version)
	if err != nil {
		return result, err
	}
	if err := verifyBinaries(ctx, files, version); err != nil {
		return result, err
	}
	if uses, err := processScanner(dir); err != nil {
		return result, err
	} else if len(uses) > 0 {
		return result, fmt.Errorf("程序正在使用中：%s", uses[0].CommandLine)
	}
	if err := recoverUpdate(dir); err != nil {
		return result, err
	}
	if err := replace(dir, files); err != nil {
		return result, err
	}
	result.Available, result.Updated, result.Version, result.Message = true, true, version, "已更新，请重新打开管理器"
	return result, nil
}

func verifyBinaries(ctx context.Context, files map[string][]byte, version string) error {
	td, err := os.MkdirTemp("", "modavd-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(td)
	if err := os.WriteFile(filepath.Join(td, "tools", "modavd.exe"), files["tools/modavd.exe"], 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(td, "tools"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(td, "tools", "modavd.exe"), files["tools/modavd.exe"], 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(td, "ModAVD.exe"), files["ModAVD.exe"], 0o755); err != nil {
		return err
	}
	for _, item := range []struct {
		path string
		args []string
	}{{filepath.Join(td, "tools", "modavd.exe"), []string{"version", "--json"}}, {filepath.Join(td, "ModAVD.exe"), []string{"__version"}}} {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		out, e := exec.CommandContext(cctx, item.path, item.args...).Output()
		cancel()
		if e != nil {
			return errors.New("下载的程序版本查询失败")
		}
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Version  string `json:"version"`
				Protocol int    `json:"protocol"`
			} `json:"data"`
		}
		if json.Unmarshal(out, &env) != nil || !env.OK || env.Data.Version != version || env.Data.Protocol != buildinfo.Protocol {
			return errors.New("下载的程序版本或协议不符")
		}
	}
	return nil
}

func digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func sumFor(text, name string) (string, error) {
	rx := regexp.MustCompile(`(?m)^([0-9a-fA-F]{64})  ` + regexp.QuoteMeta(strings.ReplaceAll(name, "\\", "/")) + `$`)
	m := rx.FindStringSubmatch(text)
	if m == nil {
		return "", errors.New("校验清单缺少 zip")
	}
	return strings.ToLower(m[1]), nil
}

func unpack(b []byte, old marker, version string) (map[string][]byte, error) {
	r, err := zip.NewReader(strings.NewReader(string(b)), int64(len(b)))
	if err != nil {
		return nil, err
	}
	want := map[string]bool{"ModAVD.exe": true, "tools/modavd.exe": true, "modavd-manager.json": true, "LICENSE": true, "NOTICE": true, "LICENSES/Go-LICENSE.txt": true}
	out := map[string][]byte{}
	for _, f := range r.File {
		if !want[f.Name] || filepath.IsAbs(f.Name) || strings.Contains(f.Name, "..") {
			return nil, errors.New("压缩包条目不允许")
		}
		if f.UncompressedSize64 > 100<<20 {
			return nil, errors.New("压缩包条目过大")
		}
		rd, err := f.Open()
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rd, 100<<20+1))
		rd.Close()
		if err != nil || int64(len(data)) > 100<<20 {
			return nil, errors.New("压缩包条目过大")
		}
		out[f.Name] = data
	}
	for name := range want {
		if _, ok := out[name]; !ok {
			return nil, errors.New("压缩包缺少文件")
		}
	}
	var m marker
	if json.Unmarshal(out["modavd-manager.json"], &m) != nil || m.Version != version || (m.Protocol != 0 && m.Protocol != buildinfo.Protocol) {
		return nil, errors.New("清单版本不符")
	}
	for name, sum := range m.Files {
		if data, ok := out[name]; !ok || digest(data) != strings.ToLower(sum) {
			return nil, errors.New("清单哈希不符")
		}
	}
	return out, nil
}

func replace(dir string, files map[string][]byte) error {
	lock := filepath.Join(dir, "update.lock")
	f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errs.Newf(errs.Busy, "已有更新正在进行")
	}
	f.Close()
	defer os.Remove(lock)
	state := filepath.Join(dir, "update.json")
	names := []string{"ModAVD.exe", filepath.Join("tools", "modavd.exe")}
	done := []string{}
	for name, data := range files {
		if name == "modavd-manager.json" {
			continue
		}
		path := filepath.Join(dir, strings.ReplaceAll(name, "/", "\\"))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		nf, err := os.OpenFile(path+".new", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
		if err != nil {
			return err
		}
		if _, err = nf.Write(data); err == nil {
			err = nf.Sync()
		}
		nf.Close()
		if err != nil {
			return err
		}
	}
	writeState := func() error {
		b, _ := json.Marshal(map[string]any{"version": buildinfo.Version, "names": names, "done": done})
		return os.WriteFile(state, append(b, '\n'), 0o600)
	}
	if err := writeState(); err != nil {
		return err
	}
	for _, name := range names {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path + ".old"); err == nil {
			return errors.New("旧文件仍在，暂不替换")
		}
		if _, err := os.Stat(path); err == nil {
			if err := os.Rename(path, path+".old"); err != nil {
				_ = recoverUpdate(dir)
				return err
			}
		}
		if err := os.Rename(path+".new", path); err != nil {
			_ = recoverUpdate(dir)
			return err
		}
		done = append(done, name)
		if err := writeState(); err != nil {
			_ = recoverUpdate(dir)
			return err
		}
	}
	return nil
}

func recoverUpdate(dir string) error {
	state := filepath.Join(dir, "update.json")
	if _, err := os.Stat(state); os.IsNotExist(err) {
		return nil
	}
	for _, name := range []string{"ModAVD.exe", filepath.Join("tools", "modavd.exe")} {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if _, e := os.Stat(path + ".old"); e == nil {
				_ = os.Rename(path+".old", path)
			}
		}
		_ = os.Remove(path + ".new")
		_ = os.Remove(path + ".old")
	}
	return os.Remove(state)
}

func Finalize() {
	if dir, err := executableDir(); err == nil {
		_ = recoverUpdate(dir)
	}
}

func scanProcesses(exeDir string) ([]processUse, error) {
	self, _ := os.Executable()
	self, _ = filepath.EvalSymlinks(self)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", fmt.Sprintf(`Get-CimInstance Win32_Process | Where-Object {$_.ExecutablePath -and $_.ProcessId -ne %d} | Select-Object ProcessId,ExecutablePath,CommandLine | ConvertTo-Json -Compress`, os.Getpid()))
	b, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return nil, nil
	}
	var rows []struct {
		ProcessId                   uint32
		ExecutablePath, CommandLine string
	}
	if json.Unmarshal(b, &rows) != nil {
		var one struct {
			ProcessId                   uint32
			ExecutablePath, CommandLine string
		}
		if json.Unmarshal(b, &one) != nil {
			return nil, errors.New("读不了进程列表")
		}
		rows = append(rows, one)
	}
	var out []processUse
	for _, row := range rows {
		p, _ := filepath.EvalSymlinks(row.ExecutablePath)
		if strings.EqualFold(p, filepath.Join(exeDir, "ModAVD.exe")) || strings.EqualFold(p, filepath.Join(exeDir, "tools", "modavd.exe")) {
			if !strings.EqualFold(p, self) {
				out = append(out, processUse{row.ProcessId, row.CommandLine})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}
