// Package components is `modavd components fetch`: it downloads and installs the Google components a ModAVD release
// leaves out on purpose (adb and the platform's system image, root.SDKComponents), the way the package's
// fetch-components.ps1 did, rule for rule. The Android SDK license from the mirror's repository2-3.xml is shown and
// has to be accepted first. Mirrors are tried in order (googledownloads.cn, Tencent, dl.google.com). Zips download
// into downloads\*.part, resume after an interruption, and are checked for size and SHA-1 before the rename; they
// unpack into sdk\.staging\<id> and move into place only when complete. An old incomplete directory is set aside,
// never deleted.
//
// It writes only downloads\, logs\fetch-components.log (and its .lock), the two component directories under sdk\ and
// sdk\.staging\: no adb server, no registry, no administrator rights. Windows and native Apple Silicon macOS have
// catalogues; other hosts return errs.Unsupported before anything is read or written.
package components

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"

	"modavd/internal/buildinfo"
	"modavd/internal/download"
	"modavd/internal/errs"
	"modavd/internal/host"
	"modavd/internal/logx"
	"modavd/internal/playstore"
	"modavd/internal/root"
)

// Mirror is one download source: a base URL the component paths are relative to.
type Mirror struct {
	ID, Name, Base string
}

var defaultMirrors = []Mirror{
	{ID: "googledownloads_cn", Name: "Google 中国下载（googledownloads.cn）", Base: "https://googledownloads.cn/android/repository/"},
	{ID: "tencent", Name: "腾讯云镜像（mirrors.cloud.tencent.com）", Base: "https://mirrors.cloud.tencent.com/AndroidSDK/"},
	{ID: "google", Name: "Google 官方（dl.google.com）", Base: "https://dl.google.com/android/repository/"},
}

// Variables for tests: the host, the catalogue (the table check and components status read) and the free space of a
// drive (DriveInfo.AvailableFreeSpace).
var (
	fetchGOOS          = runtime.GOOS
	componentCatalogue = root.SDKComponents
	diskFree           = host.DiskFree
	// playStoreCatalogue is the optional Google Play Store (Options.PlayStore); playStoreExtract takes its files out of
	// the Play image's system.img.
	playStoreCatalogue = root.PlayStoreComponent
	playStoreExtract   = playstore.Extract
)

// License returns the Android SDK license text and the mirror it came from, read from the mirrors of source the
// way a fetch with that source reads it (an empty source is the default order). It only reads the repository
// manifest; it does not touch a ModAVD root or install any component. The manager shows this text and passes its
// SHA-256 as Options.LicenseSHA256 to the fetch the user agreed to. On macOS the
// text includes both catalogue licenses, in the same order on every call.
func License(ctx context.Context, source SourceSettings) (text, from string, err error) {
	if fetchGOOS != "windows" && !(fetchGOOS == "darwin" && nativeDarwinComponents) {
		return "", "", errs.New(errs.Unsupported, "这个系统还没有组件清单").WithAdvice("目前支持 Windows 和 Apple Silicon macOS")
	}
	o := Options{}
	if err := o.CheckTimeouts(); err != nil {
		return "", "", err
	}
	o.Log = logx.Nop()
	mirrors, err := ChooseMirrors(source.Mirror, source.MirrorURLs)
	if err != nil {
		return "", "", err
	}
	f := &fetcher{o: o, mirrors: mirrors}
	if f.darwin() {
		licenses, err := f.componentLicenses(ctx, componentCatalogue())
		if err != nil {
			return "", "", err
		}
		text, from = licenseBundle(licenses)
		return text, from, nil
	}
	return f.license(ctx)
}

// LicenseSHA256 is the hex SHA-256 of a license text, as Options.LicenseSHA256 wants it.
func LicenseSHA256(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Defaults and limits of the three timeouts (the script's -ConnectTimeoutSec, -StallTimeoutSec, -ManifestTimeoutSec).
const (
	DefaultConnectTimeout  = download.DefaultConnectTimeout
	DefaultStallTimeout    = download.DefaultStallTimeout
	DefaultManifestTimeout = 40 * time.Second
	MaxConnectTimeout      = 600 * time.Second
	MaxStallTimeout        = 600 * time.Second
	MaxManifestTimeout     = 1800 * time.Second
)

// Options of a fetch.
type Options struct {
	// AcceptLicense: do not ask (the script's -AcceptLicense, for automation). The license is still fetched and shown,
	// and nothing is downloaded when it cannot be fetched.
	AcceptLicense bool
	// JSON: nobody can answer a question. Without AcceptLicense the fetch stops with errs.Usage before going online.
	JSON bool
	// Input is where the answer to the license question comes from (os.Stdin when nil).
	Input io.Reader
	// KeepDownloads keeps the zips in downloads\ after installing (deleted by default: 1.5 GB).
	KeepDownloads bool
	// PlayStore also fetches the optional Google Play Store (root.PlayStoreComponent, ModAVD#125): Google's Play image,
	// out of whose system.img only the store's files are kept.
	PlayStore bool
	// Mirror keeps only that built-in mirror (googledownloads_cn / tencent / google, any case). MirrorURLs replace the
	// built-in list, tried in order (each entry may hold several, comma-separated), and win over Mirror.
	Mirror     string
	MirrorURLs []string
	// LicenseSHA256, when set, is the SHA-256 (hex) of the license text the user agreed to in the manager (with
	// AcceptLicense): the fetch stops with errs.ComponentsRejected before saving or downloading anything when the
	// mirror now serves another text.
	LicenseSHA256 string
	// SourceOrigin says where Mirror / MirrorURLs were chosen, for the 下载源 line: SourceFromSettings,
	// SourceFromManager, or "" for the command line (an option, or the default).
	SourceOrigin string
	// ConnectTimeout bounds connect, TLS handshake and response headers; StallTimeout a read that brings nothing;
	// ManifestTimeout the whole license manifest. Zero is the default (20, 30, 40 s).
	ConnectTimeout, StallTimeout, ManifestTimeout time.Duration
	// TestFreeSpaceMB, when set, is taken as the free space of the root's drive (the script's -TestFreeSpaceMB, to
	// show the "not enough space" refusal).
	TestFreeSpaceMB *int64
	// Log gets the script's messages (Info) and warnings (Warn); Progress the download and unpack progress (at most
	// every 500 ms, plus a final report each); Notice what must be seen whatever the log level: the license text and
	// the question (Log at Info when nil).
	Log      *logx.Logger
	Progress func(Progress)
	Notice   func(text string)
}

// ComponentResult is one component after the fetch.
type ComponentResult struct {
	ID         string `json:"id"` // platform-tools / system-image
	Name       string `json:"name"`
	Version    string `json:"version"`    // Pkg.Revision now in sdk\, "" when not installed
	Installed  bool   `json:"installed"`  // complete now
	Downloaded bool   `json:"downloaded"` // installed by this run
	Skipped    bool   `json:"skipped"`    // was complete already
	SetAside   string `json:"set_aside"`  // where the old incomplete directory went (sdk\...incomplete-<time>), "" when none
}

// Result of `components fetch`. Every field is always present.
type Result struct {
	Root          string            `json:"root"`
	License       string            `json:"license"`        // the license text, "" when nothing had to be downloaded
	LicenseSource string            `json:"license_source"` // the mirror it came from
	Components    []ComponentResult `json:"components"`
	Downloads     []string          `json:"downloads"` // zips this run left in downloads\ (only with KeepDownloads)
}

type fetcher struct {
	r       *root.Root
	o       Options
	sdk     string
	mirrors []Mirror
	client  *http.Client
	logFile *os.File
	where   string        // what the run was doing, for the log when it fails (the script's $Where)
	input   *bufio.Reader // shared by the separate component-license questions
	asides  []string      // old directories moved during this run, also included in errors
}

// CheckTimeouts fills in the defaults and checks the script's ranges (1-600 s, 1-600 s, 1-1800 s).
func (o *Options) CheckTimeouts() error {
	for _, t := range []struct {
		d        *time.Duration
		def, max time.Duration
		flag     string
	}{
		{&o.ConnectTimeout, DefaultConnectTimeout, MaxConnectTimeout, "connect-timeout"},
		{&o.StallTimeout, DefaultStallTimeout, MaxStallTimeout, "stall-timeout"},
		{&o.ManifestTimeout, DefaultManifestTimeout, MaxManifestTimeout, "manifest-timeout"},
	} {
		if *t.d == 0 {
			*t.d = t.def
		}
		if *t.d < 0 || *t.d > t.max {
			return errs.Newf(errs.InvalidArgument, "--%s 要在 1–%s 秒之间，收到 %s", t.flag, seconds(t.max), seconds(*t.d))
		}
	}
	return nil
}

// ChooseMirrors is the mirror list of a run: the built-in three in order, only the one named by mirror (any case), or
// urls instead (each may hold several, comma-separated; a missing final "/" is added), which win over mirror. An
// unknown mirror is refused even with urls (the script's ValidateSet). errs.InvalidArgument.
func ChooseMirrors(mirror string, urls []string) ([]Mirror, error) {
	list := append([]Mirror(nil), defaultMirrors...)
	if mirror = strings.TrimSpace(mirror); mirror != "" {
		list = nil
		for _, m := range defaultMirrors {
			if strings.EqualFold(m.ID, mirror) {
				list = []Mirror{m}
			}
		}
		if list == nil {
			return nil, errs.Newf(errs.InvalidArgument, "没有叫 %s 的镜像（只能是 googledownloads_cn、tencent 或 google）", mirror)
		}
	}
	if len(urls) == 0 {
		return list, nil
	}
	var custom []Mirror
	for _, raw := range urls {
		for _, u := range strings.Split(raw, ",") {
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			if err := validateMirrorURL(u); err != nil {
				return nil, err
			}
			if !strings.HasSuffix(u, "/") {
				u += "/"
			}
			if len(custom) == maxMirrorURLs {
				return nil, errs.Newf(errs.InvalidArgument, "自定义下载地址最多 %d 个", maxMirrorURLs)
			}
			custom = append(custom, Mirror{ID: "custom", Name: "自定义镜像（" + u + "）", Base: u})
		}
	}
	if len(custom) == 0 {
		return nil, errs.New(errs.InvalidArgument, "--mirror-url 里没有地址")
	}
	return custom, nil
}

// Limits of the custom addresses (--mirror-url, the saved setting, the manager's 自定义地址). The zips are checked
// against the SHA-1 and size built into this program whichever mirror they come from; these only keep the
// addresses plain directory URLs.
const (
	maxMirrorURLs      = 16
	maxMirrorURLLength = 2048
)

// validateMirrorURL accepts an http(s) directory address with a host and nothing else: no user name or password
// (they would end up in logs\fetch-components.log), no ?query or #fragment (the file names are appended to it), no
// white space. A rejected address with a user name is not repeated in the message.
func validateMirrorURL(raw string) error {
	if len(raw) > maxMirrorURLLength {
		return errs.Newf(errs.InvalidArgument, "自定义下载地址太长（最多 %d 个字符）", maxMirrorURLLength)
	}
	u, err := url.Parse(raw)
	if err == nil && u.User != nil {
		return errs.New(errs.InvalidArgument, "自定义下载地址里不能带用户名或密码").
			WithAdvice("需要登录的镜像请改用不带账号的地址")
	}
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.Hostname() == "" ||
		strings.ContainsAny(raw, "?#") || strings.IndexFunc(raw, unicode.IsSpace) >= 0 {
		return errs.Newf(errs.InvalidArgument, "自定义下载地址要写 http:// 或 https:// 开头、带主机名的目录地址，不能带 ? 或 # 后面的部分，也不能有空格，收到「%s」", raw)
	}
	return nil
}

// Where the source of a fetch was chosen (Options.SourceOrigin). Empty is the command line: --mirror / --mirror-url
// when one was given, else the default order.
const (
	SourceFromSettings = "settings" // the saved per-user setting (components source, 设置 → 组件下载源)
	SourceFromManager  = "manager"  // picked in the manager's license dialog
)

// sourceLine says, before anything is downloaded, where it comes from and why: the built-in mirrors in order, only
// one of them, or custom addresses; and whether that was the default, a command-line option, the saved setting or
// the manager's choice (ModAVD#120).
func sourceLine(o Options, mirrors []Mirror) string {
	names := strings.Join(mirrorNames(mirrors), "、")
	mirror := strings.TrimSpace(o.Mirror)
	var why string
	switch o.SourceOrigin {
	case SourceFromSettings:
		why = "保存的设置"
	case SourceFromManager:
		why = "管理器里选的"
	default:
		if len(o.MirrorURLs) > 0 {
			why = "--mirror-url"
		} else if mirror != "" {
			why = "--mirror " + mirror
		} else {
			why = "默认，还没有设置过"
		}
	}
	switch {
	case len(o.MirrorURLs) > 0:
		return "下载源：" + names + "（" + why + "），按顺序试"
	case mirror != "":
		return "下载源：只用" + names + "（" + why + "）"
	default:
		return "下载源：自动（" + why + "），依次试" + names + "，前一个不通或校验不对就换下一个；只用其中一个时加 --mirror googledownloads_cn / tencent / google"
	}
}

func mirrorNames(mirrors []Mirror) []string {
	names := make([]string, len(mirrors))
	for i, m := range mirrors {
		names[i] = m.Name
	}
	return names
}

// JSONNeedsLicense is the errs.Usage error of a fetch with JSON but without AcceptLicense: nobody can answer the
// license question then. The CLI gives it before looking for the root; Fetch gives it again before going online.
func JSONNeedsLicense() *errs.Error {
	if fetchGOOS == "darwin" && nativeDarwinComponents {
		return errs.New(errs.Usage, "--json 模式必须同时给 --accept-license，不能进入交互").
			WithAdvice("先让用户看过并同意所选组件的全部 Google 许可协议，再带 --accept-license 运行")
	}
	return errs.New(errs.Usage, "--json 模式必须同时给 --accept-license，不能进入交互").
		WithAdvice("先让用户看过并同意《Android 软件开发套件许可协议》，再带 --accept-license 运行")
}

// Fetch downloads and installs the components of r that are not complete (see the package comment). Error codes:
// Unsupported (no catalogue on this host), InvalidArgument (options), PrecheckFailed (a non-ASCII path, not enough
// space), NotReady (not a ModAVD root, or sdkRoot set), Busy (another fetch runs), Usage (JSON without
// AcceptLicense), LicenseDeclined, ComponentsUnavailable / ComponentsRejected (see errs), Canceled, Internal.
func Fetch(ctx context.Context, r *root.Root, o Options) (*Result, error) {
	if fetchGOOS != "windows" && !(fetchGOOS == "darwin" && nativeDarwinComponents) {
		return nil, errs.New(errs.Unsupported, "这个系统还没有组件清单").WithAdvice("目前支持 Windows 和 Apple Silicon macOS")
	}
	if r == nil {
		return nil, errs.New(errs.NotFound, "找不到魔改 AVD 根目录")
	}
	if err := o.CheckTimeouts(); err != nil {
		return nil, err
	}
	mirrors, err := ChooseMirrors(o.Mirror, o.MirrorURLs)
	if err != nil {
		return nil, err
	}
	if o.Log == nil {
		o.Log = logx.Nop()
	}
	f := &fetcher{r: r, o: o, sdk: r.SDK(), mirrors: mirrors}
	return f.run(ctx)
}

func (f *fetcher) run(ctx context.Context) (res *Result, err error) {
	r := f.r
	_, statErr := os.Stat(filepath.Join(f.sdk, "emulator", root.CurrentPlatform().Emulator))
	isRoot := statErr == nil
	if isRoot && f.darwin() && r.LocalSDKRoot() == "" {
		if err := f.platformPaths(); err != nil {
			return nil, err
		}
	}
	if isRoot {
		// Only in a ModAVD root: a refused run elsewhere leaves no logs\ behind.
		f.openLog()
		defer f.closeLog()
	}
	var lock *host.FileLock
	defer func() {
		if err != nil {
			if len(f.asides) > 0 {
				e := *errs.As(err)
				e.Advice = strings.TrimSuffix(e.Advice, "。")
				if e.Advice != "" {
					e.Advice += "。"
				}
				e.Advice += "原有目录已保留在：" + strings.Join(f.asides, "、")
				err = &e
			}
			f.failed(err, lock != nil)
		}
		lock.Unlock()
		f.note(fmt.Sprintf("退出码 %d", errs.ExitCode(err)))
	}()
	androidVersion := "14"
	if f.darwin() {
		androidVersion = "15"
	}
	what := "adb、Android " + androidVersion + " 系统镜像"
	if f.o.PlayStore {
		what += "、Google Play 商店"
	}
	f.say("魔改 AVD：补全 Google 组件（" + what + "）")
	f.say("文件夹：" + r.Dir)
	free := "-1"
	if f.o.TestFreeSpaceMB != nil {
		free = fmt.Sprint(*f.o.TestFreeSpaceMB)
	}
	f.note(fmt.Sprintf("modavd %s，参数：AcceptLicense=%t Mirror=%s MirrorUrl=%s KeepDownloads=%t ConnectTimeoutSec=%s "+
		"StallTimeoutSec=%s ManifestTimeoutSec=%s TestFreeSpaceMB=%s PlayStore=%t", buildinfo.Version, f.o.AcceptLicense,
		f.o.Mirror, strings.Join(f.o.MirrorURLs, ","), f.o.KeepDownloads, seconds(f.o.ConnectTimeout),
		seconds(f.o.StallTimeout), seconds(f.o.ManifestTimeout), free, f.o.PlayStore))

	if !isASCII(r.Dir) {
		advice := `请把整个文件夹挪到只含英文、数字的路径下（例如 D:\ModAVD），再重新运行`
		if f.darwin() {
			advice = "请把整个文件夹挪到只含英文、数字的路径下（例如 /Users/you/ModAVD），再重新运行"
		}
		return nil, errs.New(errs.PrecheckFailed, "这个文件夹的路径里有中文或其他非英文字符，模拟器在这种路径下起不来").
			WithAdvice(advice)
	}
	if !isRoot {
		advice := "请把 Release 压缩包完整解压后，在管理器里点「补全组件」，或运行 tools\\modavd.exe components fetch"
		if f.darwin() {
			advice = "请完整解压 Apple Silicon 发行包，再在包根目录运行 modavd components fetch"
		}
		return nil, errs.Newf(errs.NotReady, "这里没有 %s，不像是魔改 AVD 的文件夹：%s", filepath.Join("sdk", "emulator", root.CurrentPlatform().Emulator), r.Dir).
			WithAdvice(advice)
	}
	if r.LocalSDKRoot() != "" {
		// check and components status look at that SDK; fetch only ever writes this root's sdk\.
		return nil, errs.New(errs.NotReady, "这个根目录在 mas-avd.json 里设了 sdkRoot（开发用的本地 SDK），不补全组件").
			WithAdvice(`组件检查看的是那份 SDK；要给这个根目录的 sdk\ 补全组件，先去掉 sdkRoot`)
	}
	if lock, err = f.lock(); err != nil {
		return nil, err
	}
	if f.darwin() {
		if err := f.platformPaths(); err != nil {
			return nil, err
		}
	}
	f.clearStaging()

	specs := componentCatalogue()
	if f.o.PlayStore {
		specs = append(specs, playStoreCatalogue())
	}
	res = &Result{Root: r.Dir, Components: []ComponentResult{}, Downloads: []string{}}
	var todo []root.SDKComponent
	for _, c := range specs {
		cr := ComponentResult{ID: c.ID, Name: c.FetchName}
		if dir := c.Dir(f.sdk); c.Complete(dir) {
			rev := root.Revision(dir)
			shown := rev
			if shown == "" {
				shown = "未知"
			}
			f.say(fmt.Sprintf("已有：%s（版本 %s），跳过", c.FetchName, shown))
			if rev != "" && !strings.EqualFold(rev, c.Version) {
				f.warn(fmt.Sprintf("  注意：这个版本和本包测试过的 %s 不同", c.Version))
			}
			cr.Version, cr.Installed, cr.Skipped = rev, true, true
		} else {
			todo = append(todo, c)
		}
		res.Components = append(res.Components, cr)
	}
	if len(todo) == 0 {
		if f.darwin() {
			f.say("组件都已齐全，不需要下载。创建或启动前仍需通过 HVF、镜像与模拟器版本检查。")
		} else {
			f.say("组件都已齐全，不需要下载。完成，可以在 AUTO-MAS 里添加这个文件夹了。")
		}
		return res, nil
	}
	if f.o.JSON && !f.o.AcceptLicense {
		// the CLI refuses this before it even looks for the root (JSONNeedsLicense); this is the second line
		return nil, JSONNeedsLicense()
	}
	if err := f.platformIdle(); err != nil {
		return nil, err
	}
	if err := f.checkSpace(todo); err != nil {
		return nil, err
	}
	f.say(sourceLine(f.o, f.mirrors))

	if err := f.acceptLicenses(ctx, todo, res); err != nil {
		return nil, err
	}

	for _, c := range todo {
		if ctx.Err() != nil {
			return nil, canceled(ctx)
		}
		f.say(fmt.Sprintf("—— %s（%s）", c.FetchName, formatSize(float64(c.Size))))
		zipFile, err := f.download(ctx, c)
		if err != nil {
			return nil, err
		}
		if f.darwin() {
			if err := f.checkSpace([]root.SDKComponent{c}); err != nil {
				return nil, err
			}
		}
		f.say("  解压安装…")
		install := f.install
		if c.ID == playStoreCatalogue().ID {
			install = f.installPlayStore
		}
		aside, err := install(ctx, c, zipFile)
		if aside != "" {
			f.asides = append(f.asides, setAsideRel(f.sdk, aside))
		}
		if err != nil {
			return nil, err
		}
		if f.darwin() {
			f.say("  已装好：" + filepath.Join(root.SDKDir, c.Target))
		} else {
			f.say(`  已装好：sdk\` + c.Target)
		}
		if f.o.KeepDownloads {
			res.Downloads = append(res.Downloads, filepath.Base(zipFile))
		} else if err := os.Remove(zipFile); err == nil {
			f.note("  已删除下载的 " + zipFile)
		}
		for k := range res.Components {
			if res.Components[k].ID == c.ID {
				res.Components[k] = ComponentResult{ID: c.ID, Name: c.FetchName, Version: root.Revision(c.Dir(f.sdk)),
					Installed: true, Downloaded: true, SetAside: setAsideRel(f.sdk, aside)}
			}
		}
	}
	if onlyLicenseLeft(filepath.Join(r.Dir, "downloads")) {
		f.note(`  downloads\ 里只剩许可协议副本`)
	}
	f.where = "最后检查"
	for _, c := range specs {
		if !c.Complete(c.Dir(f.sdk)) {
			return nil, rejected("检查发现 %s 仍不完整", c.FetchName)
		}
	}
	if f.darwin() {
		f.say("组件补全完成。创建或启动前仍需通过 HVF、镜像与模拟器版本检查。")
	} else {
		f.say("完成，可以在 AUTO-MAS 里添加这个文件夹了。")
	}
	return res, nil
}

// setAsideRel is sdk\<relative> for the result, "" for "".
func setAsideRel(sdk, p string) string {
	if p == "" {
		return ""
	}
	if rel, err := filepath.Rel(sdk, p); err == nil {
		return filepath.Join(root.SDKDir, rel)
	}
	return p
}

func onlyLicenseLeft(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.Name() != licenseFile && e.Name() != "android-sdk-arm-dbt-license.txt" {
			return false
		}
	}
	return true
}

func isASCII(s string) bool {
	for k := 0; k < len(s); k++ {
		if s[k] >= 0x80 {
			return false
		}
	}
	return true
}

// lock is logs\fetch-components.lock: one fetch per root at a time (update does the same with .update\modavd.lock).
// Only its holder clears sdk\.staging, where another run could be unpacking. The system drops it when the process
// ends.
func (f *fetcher) lock() (*host.FileLock, error) {
	p := filepath.Join(f.r.LogsDir(), "fetch-components.lock")
	l, ok, err := host.TryLock(p)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "补全组件的锁文件打不开: "+p)
	}
	if !ok {
		return nil, errs.New(errs.Busy, "这个文件夹正在补全组件").WithAdvice("另一个 modavd components fetch 还没做完，等它做完再运行")
	}
	return l, nil
}

// checkSpace: the downloads still to come (less what downloads\ holds) plus the unpacked sizes plus 500 MB must fit
// on the root's drive. A free space that cannot be read stops the run, as in the script.
func (f *fetcher) checkSpace(todo []root.SDKComponent) error {
	if f.darwin() {
		return f.platformSpace(todo)
	}
	need := int64(500 << 20)
	for _, c := range todo {
		zipFile := f.zipPath(c)
		have := int64(0)
		if st, err := os.Stat(zipFile); err == nil {
			have = st.Size()
		} else if st, err := os.Stat(zipFile + ".part"); err == nil {
			have = st.Size()
		}
		need += max(0, c.Size-have) + c.ExtractedBytes
	}
	drive := filepath.VolumeName(f.r.Dir) + `\`
	f.where = "查 " + drive + " 的剩余空间"
	freeBytes, err := diskFree(f.r.Dir)
	if err != nil {
		return errs.Wrap(errs.Internal, err, fmt.Sprintf("查不了 %s 的剩余空间：%s", drive, errs.As(err).Message))
	}
	free := int64(min(freeBytes, 1<<62))
	if mb := f.o.TestFreeSpaceMB; mb != nil {
		free = clampMB(*mb)
		f.note(fmt.Sprintf("  （测试：假装只剩 %d MB）", *mb))
	}
	if free < need {
		return errs.Newf(errs.PrecheckFailed, "磁盘空间不够：还需要约 %s，%s 只剩 %s", formatSize(float64(need)), drive, formatSize(float64(free))).
			WithAdvice("游戏数据以后也放在这个文件夹里，请换一个空间更大的盘")
	}
	return nil
}

const licenseFile = "android-sdk-license.txt"

// consent shows the license and its source and asks for y (yes) unless AcceptLicense. Only after that is the copy in
// downloads\ written: a refusal leaves no file there.
func (f *fetcher) consent(text, from string, todo []root.SDKComponent) error {
	return f.consentLicense(text, from, todo, licenseID)
}

func (f *fetcher) consentLicense(text, from string, todo []root.SDKComponent, id string) error {
	title, label := "Android Software Development Kit License Agreement", "Android 软件开发套件许可协议"
	if id == "android-sdk-arm-dbt-license" {
		title, label = "Android SDK ARM System Image License Agreement", "Android SDK ARM 系统镜像许可协议"
	}
	f.notice("\n================ " + title + " ================\n" + text +
		"\n=====================================================================================\n\n")
	names := make([]string, len(todo))
	for k, c := range todo {
		names[k] = c.FetchName
	}
	shown := fmt.Sprintf("以上是 Google《%s》全文（取自 %s）。", label, from)
	terms := fmt.Sprintf("接下来要下载的 %s 由 Google 提供、受这份协议约束，本项目不分发它们。", strings.Join(names, "、"))
	f.notice(shown + "\n" + terms + "\n")
	f.note(shown)
	f.note(terms)
	if f.o.AcceptLicense {
		if f.o.LicenseSHA256 != "" {
			f.say("已在管理器里同意许可协议。")
		} else {
			f.say("已通过 --accept-license 同意许可协议。")
		}
	} else {
		f.notice("是否同意这份许可协议？输入 y 并回车表示同意，直接回车或输入其他内容则退出：")
		in := f.o.Input
		if in == nil {
			in = os.Stdin
		}
		if f.input == nil {
			f.input = bufio.NewReader(in)
		}
		line, _ := f.input.ReadString('\n')
		answer := strings.TrimRight(line, "\r\n")
		f.note("用户输入：" + answer)
		if a := strings.TrimSpace(answer); !strings.EqualFold(a, "y") && !strings.EqualFold(a, "yes") {
			f.notice("\n")
			return errs.New(errs.LicenseDeclined, "没有同意许可协议，已退出，什么也没有下载")
		}
		f.say("已同意许可协议。")
	}
	if f.darwin() {
		if err := f.platformPaths(); err != nil {
			return err
		}
	}
	dir := filepath.Join(f.r.Dir, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(errs.Internal, err, "建不了 downloads 目录："+err.Error())
	}
	p := filepath.Join(dir, id+".txt")
	copyText := strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	if err := os.WriteFile(p, append([]byte("\xef\xbb\xbf"), copyText...), 0o644); err != nil {
		return errs.Wrap(errs.Internal, err, "保存许可协议失败："+err.Error())
	}
	f.say("许可协议另存了一份在：" + p)
	return nil
}

func (f *fetcher) darwin() bool { return fetchGOOS == "darwin" && nativeDarwinComponents }

type componentLicense struct {
	id, text, from string
}

// Retrieve all required texts before asking questions or downloading archives.
// The explicit --accept-license flag applies to every displayed component term.
func (f *fetcher) acceptLicenses(ctx context.Context, todo []root.SDKComponent, res *Result) error {
	f.where = "获取许可协议"
	if !f.darwin() {
		text, from, err := f.license(ctx)
		if err != nil {
			return err
		}
		if err := f.checkAgreedLicense(text); err != nil {
			return err
		}
		res.License, res.LicenseSource = text, from
		return f.consent(text, from, todo)
	}
	specs := todo
	if strings.TrimSpace(f.o.LicenseSHA256) != "" {
		// The manager reads the whole catalogue before it knows which files
		// remain complete. Verify the same bundle even when only one is missing.
		specs = componentCatalogue()
	}
	licenses, err := f.componentLicenses(ctx, specs)
	if err != nil {
		return err
	}
	text, from := licenseBundle(licenses)
	if err := f.checkAgreedLicense(text); err != nil {
		return err
	}
	for _, l := range licenses {
		var needed []root.SDKComponent
		for _, c := range todo {
			if c.LicenseID == l.id {
				needed = append(needed, c)
			}
		}
		if len(needed) != 0 {
			if err := f.consentLicense(l.text, l.from, needed, l.id); err != nil {
				return err
			}
		}
	}
	res.License, res.LicenseSource = text, from
	return nil
}

func (f *fetcher) checkAgreedLicense(text string) error {
	if want := strings.TrimSpace(f.o.LicenseSHA256); want != "" {
		if got := LicenseSHA256(text); !strings.EqualFold(want, got) {
			f.note("管理器里同意的许可协议 SHA-256 " + want + "，这次取到的是 " + got)
			return errs.New(errs.ComponentsRejected, "下载源这次给的许可协议和你在管理器里同意的那份不一样，什么也没有下载").
				WithAdvice("请重新点「补全组件」，看过新的许可协议后再决定")
		}
	}
	return nil
}

func (f *fetcher) componentLicenses(ctx context.Context, specs []root.SDKComponent) ([]componentLicense, error) {
	var licenses []componentLicense
	for _, c := range specs {
		if c.LicenseID != licenseID && c.LicenseID != "android-sdk-arm-dbt-license" {
			return nil, rejected("组件 %s 没有受支持的许可标识", c.ID)
		}
		found := false
		for _, l := range licenses {
			if l.id == c.LicenseID {
				found = true
				break
			}
		}
		if found {
			continue
		}
		text, from, err := f.licenseFor(ctx, c.LicenseManifest, c.LicenseID, c.LicenseSHA256)
		if err != nil {
			return nil, err
		}
		licenses = append(licenses, componentLicense{c.LicenseID, text, from})
	}
	return licenses, nil
}

func licenseBundle(licenses []componentLicense) (text, from string) {
	var texts, sources []string
	for _, l := range licenses {
		texts = append(texts, l.id+"\n"+l.text)
		sources = append(sources, l.id+": "+l.from)
	}
	return strings.Join(texts, "\n\n"), strings.Join(sources, "; ")
}

// ---- messages and the log file -----------------------------------------------------------------------------------

func (f *fetcher) openLog() {
	if f.darwin() && f.platformPaths() != nil {
		return
	}
	dir := f.r.LogsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f.logFile, _ = os.OpenFile(filepath.Join(dir, "fetch-components.log"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
}

func (f *fetcher) closeLog() {
	if f.logFile != nil {
		f.logFile.Close()
		f.logFile = nil
	}
}

// note is one line in logs\fetch-components.log only (the script's Write-Log): "yyyy-MM-dd HH:mm:ss text", CRLF,
// UTF-8 without BOM. A log that cannot be written is ignored, as there.
func (f *fetcher) note(text string) {
	if f.logFile != nil {
		fmt.Fprintf(f.logFile, "%s %s\r\n", time.Now().Format("2006-01-02 15:04:05"), text)
	}
}

// say is the script's Say: shown and logged. warn is its yellow Say.
func (f *fetcher) say(text string) {
	f.o.Log.Infof("%s", text)
	f.note(text)
}

func (f *fetcher) warn(text string) {
	f.o.Log.Warnf("%s", text)
	f.note(text)
}

// notice is shown whatever the log level and not logged (the script's Write-Host of the license).
func (f *fetcher) notice(text string) {
	if f.o.Notice != nil {
		f.o.Notice(text)
		return
	}
	if s := strings.TrimSpace(text); s != "" {
		f.o.Log.Infof("%s", s)
	}
}

// failed is the script's catch: where it stopped and why go to the log; the user hears that what was downloaded is
// kept and where the log is; sdk\.staging is cleared again when this run holds the lock.
func (f *fetcher) failed(err error, locked bool) {
	if f.where != "" {
		f.note("失败：" + f.where)
	}
	f.note("原因：" + errs.As(err).Error())
	if locked {
		if hasPartialDownloads(filepath.Join(f.r.Dir, "downloads")) {
			f.say(`已下载的部分保存在 downloads\ 里，再次运行会接着下。sdk\ 里没有留下装到一半的目录。`)
		}
		f.clearStaging()
	}
	if f.logFile != nil {
		f.say("详细记录：" + f.logFile.Name())
	}
}

func hasPartialDownloads(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if n := strings.ToLower(e.Name()); strings.HasSuffix(n, ".part") || strings.HasSuffix(n, ".zip") {
			return true
		}
	}
	return false
}
