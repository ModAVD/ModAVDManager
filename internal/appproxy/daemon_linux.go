//go:build linux

package appproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"modavd/internal/buildinfo"
)

// Main is modavd-proxyd's main. start, stop and status print the Status as JSON; run is the daemon itself (start
// runs it in a session of its own).
//
//	modavd-proxyd start    start the daemon, or make the running one reload config.json; waits until its rules
//	                       are in place (at most 3 s); exit 1 when they are not
//	modavd-proxyd stop     stop it and remove the rules (also those a dead daemon left behind)
//	modavd-proxyd status
func Main(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "用法：modavd-proxyd start|stop|status|version")
		return 2
	}
	dir, err := exeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch args[0] {
	case "start":
		return cmdStart(dir)
	case "stop":
		return cmdStop(dir)
	case "status":
		return cmdStatus(dir)
	case "run":
		return runDaemon(dir)
	case "version":
		fmt.Println(buildinfo.Version)
		return 0
	}
	fmt.Fprintln(os.Stderr, "用法：modavd-proxyd start|stop|status|version")
	return 2
}

func exeDir() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(p), nil
}

// ---- the commands

const waitStart = 3 * time.Second

func cmdStart(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, ConfigName))
	if err == nil {
		_, err = ParseConfig(b)
	}
	if err != nil {
		return printStatus(Status{Error: err.Error(), Rules: liveRules(nil)}, 1)
	}
	sum := ConfigSum(b)
	if pid := runningPID(dir); pid > 0 {
		syscall.Kill(pid, syscall.SIGHUP)
		st := waitState(dir, func(s Status) bool { return s.PID == pid && s.ConfigSHA256 == sum && s.Rules != "" })
		return printStatus(st, exitFor(st))
	}
	self, err := os.Executable()
	if err != nil {
		return printStatus(Status{Error: err.Error()}, 1)
	}
	since := time.Now().Add(-time.Second)
	cmd := exec.Command(self, "run")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // stdio is /dev/null: adb shell does not wait for it
	if err := cmd.Start(); err != nil {
		return printStatus(Status{Error: "起不来：" + err.Error()}, 1)
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	st := waitState(dir, func(s Status) bool {
		at, _ := time.Parse(time.RFC3339, s.StartedAt)
		return s.PID == pid && !at.Before(since.Truncate(time.Second)) && (s.Rules != "" || s.Error != "")
	})
	return printStatus(st, exitFor(st))
}

func exitFor(s Status) int {
	if s.Running && s.Rules == RulesOK && s.Error == "" {
		return 0
	}
	return 1
}

func cmdStop(dir string) int {
	if pid := runningPID(dir); pid > 0 {
		syscall.Kill(pid, syscall.SIGTERM)
		for end := time.Now().Add(waitStart); time.Now().Before(end) && runningPID(dir) > 0; {
			time.Sleep(50 * time.Millisecond)
		}
		if runningPID(dir) > 0 {
			syscall.Kill(pid, syscall.SIGKILL)
			time.Sleep(200 * time.Millisecond)
		}
	}
	err := teardownRules()
	st := readState(dir)
	st.Running, st.Rules, st.RulesError = false, liveRules(nil), ""
	if err != nil {
		st.RulesError = err.Error()
	}
	writeState(dir, st)
	return printStatus(st, 0)
}

func cmdStatus(dir string) int {
	pid := runningPID(dir)
	if pid > 0 {
		before := readState(dir).UpdatedAt
		syscall.Kill(pid, syscall.SIGUSR1) // write the state now
		waitFor(time.Second, func() bool { return readState(dir).UpdatedAt != before })
	}
	st := readState(dir)
	st.Running = pid > 0
	if !st.Running {
		st.Rules = liveRules(nil)
	}
	return printStatus(st, 0)
}

// liveRules looks without a daemon (whose state would say): RulesOff when none of ours are there, RulesPartial
// when any are (with no daemon to take their traffic, the listed apps then have no network), RulesFailed when the
// tools do not answer.
func liveRules(uids []UIDRange) string {
	s, err := inspectRules(uids)
	switch {
	case err != nil:
		return RulesFailed
	case s == RulesOff:
		return RulesOff
	}
	return RulesPartial
}

func printStatus(s Status, code int) int {
	if s.Version == "" {
		s.Version = buildinfo.Version
	}
	b, _ := json.Marshal(s)
	fmt.Println(string(b))
	return code
}

func waitState(dir string, done func(Status) bool) Status {
	var st Status
	waitFor(waitStart, func() bool {
		st = readState(dir)
		return done(st)
	})
	st.Running = runningPID(dir) > 0
	return st
}

func waitFor(d time.Duration, ok func() bool) {
	for end := time.Now().Add(d); !ok() && time.Now().Before(end); {
		time.Sleep(50 * time.Millisecond)
	}
}

func readState(dir string) Status {
	var s Status
	if b, err := os.ReadFile(filepath.Join(dir, StateName)); err == nil {
		json.Unmarshal(b, &s)
	}
	return s
}

func writeState(dir string, s Status) error {
	b, _ := json.MarshalIndent(s, "", " ")
	p := filepath.Join(dir, StateName)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// runningPID is the daemon's pid when one runs (it holds the lock), else 0.
func runningPID(dir string) int {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return 0
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return 0
	}
	b, _ := io.ReadAll(f)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// ---- the daemon

type daemon struct {
	dir     string
	log     *log.Logger
	started time.Time
	reply   *replySockets
	core    atomic.Pointer[Core]
	c       counters

	mu       sync.Mutex
	cfg      *Config
	cfgSum   string
	server   Server
	health   *Health
	apps     []AppStatus
	uids     []int
	rules    string
	rulesErr string
	lastSave string // the state last written, without the parts that change all the time
	savedAt  time.Time
}

func runDaemon(dir string) int {
	d := &daemon{dir: dir, started: time.Now(), log: log.New(newRotating(filepath.Join(dir, LogName)), "",
		log.LstdFlags|log.LUTC)}
	fail := func(format string, args ...any) int {
		msg := fmt.Sprintf(format, args...)
		d.log.Print(msg)
		writeState(dir, Status{Version: buildinfo.Version, PID: os.Getpid(), StartedAt: d.stamp(d.started),
			UpdatedAt: d.stamp(time.Now()), Error: msg, Rules: liveRules(nil)})
		return 1
	}
	// first, so that a reload (SIGHUP) arriving while it starts does not end it
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGUSR1)
	lock, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fail("打不开锁文件：%v", err)
	}
	defer lock.Close() // held (and kept from the finalizer) for as long as the daemon runs
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		d.log.Print("已经有一个在跑")
		return 1
	}
	lock.Truncate(0)
	lock.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	// the low memory killer is not to pick it: without it the listed apps have no network
	os.WriteFile("/proc/self/oom_score_adj", []byte("-1000"), 0o644)

	b, err := os.ReadFile(filepath.Join(dir, ConfigName))
	if err != nil {
		return fail("读不了配置：%v", err)
	}
	cfg, err := ParseConfig(b)
	if err != nil {
		return fail("%v", err)
	}
	loopback := netip.MustParseAddr("127.0.0.1")
	tl, err := listenTCP(netip.AddrPortFrom(loopback, TCPPort))
	if err != nil {
		return fail("TCP 端口 %d 用不了：%v", TCPPort, err)
	}
	ul, err := listenUDP(netip.AddrPortFrom(loopback, UDPPort))
	if err != nil {
		return fail("UDP 端口 %d 用不了：%v", UDPPort, err)
	}
	d.reply = newReplySockets()
	d.useConfig(cfg, b)
	go serveTCP(tl, d.core.Load)
	go serveUDP(ul, d.core.Load)
	d.log.Printf("启动（%s，modavd %s）：%s，%d 个应用", DaemonName, buildinfo.Version, cfg.Server, len(cfg.Apps))
	d.refreshApps(true)
	d.save(true)

	pkgs := make(chan struct{}, 1)
	if err := watchPackages(pkgs); err != nil {
		d.log.Printf("盯不了 %s（%v），只靠每 10 秒查一次", PackagesList, err)
	}
	check := time.NewTicker(5 * time.Second)
	poll := time.NewTicker(10 * time.Second)
	lastList := listStamp()
	for {
		select {
		case s := <-sigs:
			switch s {
			case syscall.SIGHUP:
				d.reload()
				d.save(true)
			case syscall.SIGUSR1:
				d.save(true)
			default:
				d.log.Print("停止，撤掉规则")
				tl.Close()
				ul.Close()
				err := teardownRules()
				d.mu.Lock()
				d.rules, d.rulesErr = RulesOff, ""
				if err != nil {
					d.rulesErr = err.Error()
				}
				d.mu.Unlock()
				st := d.status()
				st.Running = false
				writeState(dir, st)
				return 0
			}
		case <-pkgs:
			lastList = listStamp()
			d.refreshApps(false)
		case <-poll.C:
			if s := listStamp(); s != lastList {
				lastList = s
				d.refreshApps(false)
			}
		case <-check.C:
			d.checkRules()
			d.save(false)
		}
	}
}

func (d *daemon) stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// useConfig switches to cfg (raw: its bytes): a new core when the proxy or the resolver changed; connections
// already relayed stay on the old one until they end.
func (d *daemon) useConfig(cfg *Config, raw []byte) {
	srv, _ := ParseServer(cfg.Server)
	d.mu.Lock()
	same := d.cfg != nil && d.cfg.Server == cfg.Server && d.cfg.DNS == cfg.DNS
	d.cfg, d.cfgSum, d.server = cfg, ConfigSum(raw), srv
	if !same {
		d.health = &Health{}
	}
	health := d.health
	d.mu.Unlock()
	if same {
		return
	}
	old := d.core.Swap(newCore(cfg, &d.c, health, d.log, d.reply.send))
	if old != nil {
		old.Close()
	}
}

func (d *daemon) reload() {
	b, err := os.ReadFile(filepath.Join(d.dir, ConfigName))
	var cfg *Config
	if err == nil {
		cfg, err = ParseConfig(b)
	}
	if err != nil {
		d.log.Printf("新配置用不了，还用原来的：%v", err)
		return
	}
	d.log.Printf("换配置：%s，%d 个应用", cfg.Server, len(cfg.Apps))
	d.useConfig(cfg, b)
	d.refreshApps(true)
}

// listStamp tells whether packages.list changed (it is replaced by a rename).
func listStamp() string {
	st, err := os.Stat(PackagesList)
	if err != nil {
		return ""
	}
	return fmt.Sprint(st.ModTime().UnixNano(), st.Size(), st.Sys().(*syscall.Stat_t).Ino)
}

// refreshApps maps the list to uids again and applies the rules when they changed (or always when force).
// The list unreadable, the uids stay as they were: never fewer apps through the proxy because of a read error.
func (d *daemon) refreshApps(force bool) {
	b, err := os.ReadFile(PackagesList)
	if err != nil {
		d.log.Printf("读不了 %s：%v", PackagesList, err)
		d.mu.Lock()
		if d.apps == nil {
			d.rules, d.rulesErr = RulesFailed, fmt.Sprintf("读不了 %s，不知道名单里的应用是哪个 uid：%v", PackagesList, err)
		}
		d.mu.Unlock()
		return
	}
	d.mu.Lock()
	apps, uids := ResolveApps(d.cfg.Apps, ParsePackages(b))
	changed := !slices.Equal(uids, d.uids)
	d.apps, d.uids = apps, uids
	d.mu.Unlock()
	if changed || force {
		if changed && !force {
			d.log.Printf("装的应用变了，名单里的 uid 现在是 %v", uids)
		}
		d.applyRules()
	}
}

func (d *daemon) applyRules() {
	d.mu.Lock()
	r := Ranges(d.uids)
	d.mu.Unlock()
	err := setupRules(r)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.rules, d.rulesErr = RulesFailed, err.Error()
		d.log.Printf("规则挂不上：%v", err)
		return
	}
	d.rules, d.rulesErr = RulesOK, ""
}

// checkRules puts back what went missing (netd restarting flushes its built-in chains and the routing rules).
func (d *daemon) checkRules() {
	d.mu.Lock()
	r := Ranges(d.uids)
	d.mu.Unlock()
	s, err := inspectRules(r)
	if err != nil || s == RulesOK {
		return
	}
	d.log.Printf("规则少了（%s），补上", s)
	d.mu.Lock()
	d.rules = RulesPartial
	d.mu.Unlock()
	d.applyRules()
}

func (d *daemon) status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return Status{Version: buildinfo.Version, Running: true, PID: os.Getpid(), StartedAt: d.stamp(d.started),
		UpdatedAt: d.stamp(time.Now()), ConfigSHA256: d.cfgSum, Server: d.health.Status(d.server),
		Apps: d.apps, Rules: d.rules, RulesError: d.rulesErr, Counters: d.c.snapshot()}
}

// save writes state.json when something but the counters changed, or force, or every 30 s.
func (d *daemon) save(force bool) {
	st := d.status()
	key := st
	key.UpdatedAt, key.Counters = "", Counters{}
	kb, _ := json.Marshal(key)
	if !force && string(kb) == d.lastSave && time.Since(d.savedAt) < 30*time.Second {
		return
	}
	if err := writeState(d.dir, st); err != nil {
		d.log.Printf("写不了状态：%v", err)
		return
	}
	d.lastSave, d.savedAt = string(kb), time.Now()
}

// ---- the log

// rotating is the log file, moved to <name>.1 at 1 MB.
type rotating struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

const maxLog = 1 << 20

func newRotating(path string) *rotating {
	r := &rotating{path: path}
	r.open()
	return r
}

func (r *rotating) open() {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	st, _ := f.Stat()
	r.f, r.size = f, st.Size()
}

func (r *rotating) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil && r.size+int64(len(p)) > maxLog {
		r.f.Close()
		os.Rename(r.path, r.path+".1")
		r.f = nil
		r.open()
	}
	if r.f == nil {
		return len(p), nil
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return len(p), err
}
