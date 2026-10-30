//go:build windows

// Package ui is the server side of the ModAVD desktop manager (ModAVD.exe): a local HTTP server that serves the
// embedded page (web/) and the API described in API.md over emu.Manager, and decides when the manager exits.
//
// One server manages one root. Every request must carry our own Host (127.0.0.1 or localhost with our port), so a
// DNS-rebound name never reaches the API; /api/* also needs the login cookie and, unless GET, our own Origin. Long
// operations (start, stop, restart, install) run as jobs and report through the event stream (/api/events).
//
// Lifetime: the server exits once its window is gone (the Edge process it started exited, or no event stream has
// been connected for IdleExit) and the running jobs are done.
package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"modavd/internal/root"
)

// Options of Run.
type Options struct {
	Root *root.Root
	// Log is the manager's own log (ModAVD.exe writes it to <root>\logs\modavd-ui.log); nil discards.
	Log *logx.Logger
	// Addr to listen on, "127.0.0.1:<port>"; "" = "127.0.0.1:0" (a free port).
	Addr string
	// OpenWindow opens the page in an Edge app window (the default browser when Edge is missing) once the server
	// listens.
	OpenWindow bool
	// Dev also serves web/dev/ (the page's mock backend).
	Dev bool
	// Ready, when set, is called once the server listens with its address ("http://127.0.0.1:<port>/") and the login
	// token; the launcher keeps both in its state file so a second launch can reopen a window.
	Ready func(baseURL, token string)
	// IdleExit: exit after this long without any event stream connected; 0 = 60 s, negative = never.
	IdleExit time.Duration

	// Test hooks.
	newBackend func(log *logx.Logger) backend
	token      string
	web        fs.FS
	openPath   func(path string) error
	launch     func(url string) (*Window, error)
}

const (
	cookieBase       = "modavd_ui"
	defaultAddr      = "127.0.0.1:0"
	defaultIdleExit  = 60 * time.Second
	pollInterval     = 3 * time.Second
	pingInterval     = 15 * time.Second
	exitTick         = time.Second
	windowCloseGrace = 5 * time.Second
	// An Edge process that exits this soon after launch handed the window to an Edge already running on the same
	// profile; that window is not ours to wait for, so the event-stream rule decides instead.
	edgeHandoffWindow = 10 * time.Second
	// How long a stop waits for the start job it cancelled to return before stopping anyway.
	startStopWait = 15 * time.Second
	// How long Run waits for jobs after its context ended (the process is going away).
	jobDrainOnCancel = 10 * time.Second
)

// server is one running manager. Fields above mu never change after newServer.
type server struct {
	root       *root.Root
	log        *logx.Logger // the file log
	mgr        backend      // requests outside jobs; its messages also go to the event stream
	poll       backend      // the instance poller: file log only, it runs every few seconds
	newBackend func(log *logx.Logger) backend
	token      string
	dev        bool
	files      map[string]*staticFile
	openPath   func(path string) error
	launch     func(url string) (*Window, error)
	mux        *http.ServeMux

	pollEvery, pingEvery, exitEvery, idleExit, closeGrace, stopWait time.Duration

	// ctx ends with the server (event streams, screenshot captures); jobCtx only when Run's own context ends, since
	// a closed window waits for running jobs.
	ctx       context.Context
	cancel    context.CancelFunc
	jobCtx    context.Context
	jobCancel context.CancelFunc

	port int // set once before serving

	hub   *hub
	jobs  jobTable
	shots shotCache
	check checkCache
}

func newServer(opt Options) (*server, error) {
	if opt.Root == nil {
		return nil, errs.New(errs.Usage, "没有指定魔改 AVD 根目录")
	}
	lg := opt.Log
	if lg == nil {
		lg = logx.Nop()
	}
	newBackend := opt.newBackend
	if newBackend == nil {
		r := opt.Root
		newBackend = func(l *logx.Logger) backend { return managerBackend{emu.New(r, l)} }
	}
	token := opt.token
	if token == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, errs.Wrap(errs.Internal, err, "生成登录令牌失败")
		}
		token = hex.EncodeToString(b)
	}
	web := opt.web
	if web == nil {
		sub, err := fs.Sub(embedded, "web")
		if err != nil {
			return nil, errs.Wrap(errs.Internal, err, "页面文件没有打包进程序")
		}
		web = sub
	}
	files, err := loadStatic(web)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, err, "读不出打包的页面文件")
	}
	idle := opt.IdleExit
	if idle == 0 {
		idle = defaultIdleExit
	}
	s := &server{
		root:       opt.Root,
		log:        lg,
		poll:       newBackend(lg),
		newBackend: newBackend,
		token:      token,
		dev:        opt.Dev,
		files:      files,
		openPath:   opt.openPath,
		launch:     opt.launch,
		pollEvery:  pollInterval,
		pingEvery:  pingInterval,
		exitEvery:  exitTick,
		idleExit:   idle,
		closeGrace: windowCloseGrace,
		stopWait:   startStopWait,
		hub:        newHub(),
	}
	if s.openPath == nil {
		s.openPath = shellOpen
	}
	if s.launch == nil {
		s.launch = LaunchWindow
	}
	s.mgr = newBackend(lg.Tee(s.eventLogSink("")))
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.jobCtx, s.jobCancel = context.WithCancel(context.Background())
	s.routes()
	return s, nil
}

// Run serves until the lifetime rules say to exit (see the package comment) or ctx ends (running jobs are then
// cancelled). The error is for startup only: errs.PortConflict when the address cannot be bound.
func Run(ctx context.Context, opt Options) error {
	s, err := newServer(opt)
	if err != nil {
		return err
	}
	defer s.cancel()
	defer s.jobCancel()

	addr := opt.Addr
	if addr == "" {
		addr = defaultAddr
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host != "127.0.0.1" {
		return errs.Newf(errs.Usage, "管理器只能监听 127.0.0.1（给的是 %s）", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return listenError(addr, err)
	}
	s.port = ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
		ErrorLog:          log.New(logWriter{s.log}, "", 0),
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Errorf("管理器的 HTTP 服务出错: %v", err)
		}
	}()
	go s.pollLoop()
	cleanUploads(s.log)

	base := fmt.Sprintf("http://127.0.0.1:%d/", s.port)
	s.log.Infof("管理器已启动: %s（根目录 %s）", base, s.root.Dir)
	if opt.Ready != nil {
		opt.Ready(base, s.token)
	}
	var w *Window
	if opt.OpenWindow {
		if w, err = s.launch(base + "?t=" + s.token); err != nil {
			s.log.Errorf("打不开管理器窗口: %v", err)
		} else if w.Edge {
			s.log.Infof("已用 Edge 应用窗口打开管理器")
		} else {
			s.log.Infof("没找到 Edge，已用默认浏览器打开管理器")
		}
	}

	s.waitForExit(ctx, w)

	s.log.Infof("管理器退出")
	s.cancel() // ends the event streams, which Shutdown would otherwise wait for
	s.hub.closeAll()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	<-served
	return nil
}

// waitForExit returns when the manager should exit: ctx ended (jobs cancelled, waited for briefly), or the window is
// gone and no job runs. A window counts as gone when the Edge process we started exited (unless it was a hand-off to
// a running Edge) and no event stream is connected for closeGrace, or when no event stream has been connected for
// idleExit. A page that connects again while jobs finish keeps the manager running.
func (s *server) waitForExit(ctx context.Context, w *Window) {
	var edgeDone <-chan struct{}
	if w != nil {
		edgeDone = w.Done
	}
	windowClosed := false
	waitingLogged := false
	tick := time.NewTicker(s.exitEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			if n := s.jobs.running(); n > 0 {
				s.log.Warnf("管理器被要求退出，取消 %d 个正在跑的任务", n)
				s.jobCancel()
				s.jobs.waitIdle(jobDrainOnCancel)
			}
			return
		case <-edgeDone:
			edgeDone = nil
			if time.Since(w.Started) < edgeHandoffWindow {
				s.log.Infof("Edge 把窗口交给了已在运行的 Edge；之后按页面连接判断窗口是否关闭")
			} else {
				s.log.Infof("管理器窗口已关闭")
				windowClosed = true
			}
		case <-tick.C:
		}
		clients, idle := s.hub.idle()
		if clients > 0 {
			waitingLogged = false
			continue
		}
		gone := (windowClosed && idle >= s.closeGrace) || (s.idleExit > 0 && idle >= s.idleExit)
		if !gone {
			continue
		}
		if n := s.jobs.running(); n > 0 {
			if !waitingLogged {
				s.log.Infof("窗口已关闭，等 %d 个正在跑的任务做完再退出", n)
				waitingLogged = true
			}
			continue
		}
		return
	}
}

// listenError turns a bind failure into the message the launcher shows.
func listenError(addr string, err error) error {
	const wsaeacces, wsaeaddrinuse = syscall.Errno(10013), syscall.Errno(10048)
	_, port, _ := net.SplitHostPort(addr)
	if errors.Is(err, wsaeaddrinuse) || errors.Is(err, wsaeacces) {
		return errs.Wrap(errs.PortConflict, err, "管理器用不了端口 "+port+"（被别的程序占用，或被系统保留）").
			WithAdvice("换一个端口（--port），或者不带 --port 让系统挑一个空闲端口")
	}
	return errs.Wrap(errs.Internal, err, "管理器监听 "+addr+" 失败: "+err.Error())
}

// hostOK: the Host header names us (127.0.0.1 or localhost with our port).
func (s *server) hostOK(host string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != strconv.Itoa(s.port) {
		return false
	}
	return h == "127.0.0.1" || strings.EqualFold(h, "localhost")
}

// originOK: a non-GET request comes from our own page (browsers send Origin on every non-GET fetch).
func (s *server) originOK(origin string) bool {
	p := strconv.Itoa(s.port)
	return origin == "http://127.0.0.1:"+p || strings.EqualFold(origin, "http://localhost:"+p)
}

// logWriter feeds net/http's own error log (bad requests, handler panics) into the file log.
type logWriter struct{ l *logx.Logger }

func (w logWriter) Write(p []byte) (int, error) {
	w.l.Warnf("HTTP: %s", strings.TrimSpace(string(p)))
	return len(p), nil
}

// checkCache keeps the last host check: it takes seconds (acceleration check, Vulkan probe), and the page asks for it
// on every visit of its check view. sem serializes runs; a request waits for a run in progress instead of starting
// another one.
type checkCache struct {
	once sync.Once
	sem  chan struct{}
	res  *emu.CheckResult
	at   time.Time
}

func (c *checkCache) lock(ctx context.Context) error {
	c.once.Do(func() { c.sem = make(chan struct{}, 1) })
	select {
	case c.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return errs.Wrap(errs.Canceled, ctx.Err(), "已取消")
	}
}

func (c *checkCache) unlock() { <-c.sem }
