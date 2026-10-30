//go:build windows

package win

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"modavd/internal/errs"
)

// The test binary doubles as the processes the tests start: TestMain routes these first arguments before any test
// runs (SpawnHelperArg makes it the StartOrphan helper).
const (
	modeChild = "__child" // print a line (pid, cwd, $MODAVD_WIN_TEST) to stdout and one to stderr, sleep <secs>
	modeTree  = "__tree"  // start a __child with StartDetached, print "grandchild=<pid>", sleep 60 s
	modeExit  = "__exit"  // sleep 300 ms, exit with <code>
	modeLock  = "__lock"  // TryLock <path>, print "locked=<bool>"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case SpawnHelperArg:
			os.Exit(SpawnHelperMain(os.Args[2:], os.Stdout))
		case modeChild:
			cwd, _ := os.Getwd()
			fmt.Printf("child-output pid=%d cwd=%s env=%s\n", os.Getpid(), cwd, os.Getenv("MODAVD_WIN_TEST"))
			fmt.Fprintln(os.Stderr, "child-stderr")
			secs, _ := strconv.Atoi(os.Args[2])
			time.Sleep(time.Duration(secs) * time.Second)
			os.Exit(0)
		case modeTree:
			exe, _ := os.Executable()
			p, err := StartDetached(exe, []string{modeChild, "60"}, StartOptions{})
			if err != nil {
				fmt.Println("error:", err)
				os.Exit(1)
			}
			fmt.Printf("grandchild=%d\n", p.PID)
			time.Sleep(60 * time.Second)
			os.Exit(0)
		case modeExit:
			code, _ := strconv.Atoi(os.Args[2])
			time.Sleep(300 * time.Millisecond)
			os.Exit(code)
		case modeLock:
			l, ok, err := TryLock(os.Args[2])
			fmt.Printf("locked=%v err=%v\n", ok, err)
			l.Unlock()
			os.Exit(0)
		}
	}
	os.Exit(m.Run())
}

func testExe(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// workDir is a fresh directory, removed afterwards: under $MODAVD_TEST_TMP\win when that is set (to keep test data on a
// chosen drive), otherwise t.TempDir().
func workDir(t *testing.T) string {
	t.Helper()
	env := os.Getenv("MODAVD_TEST_TMP")
	if env == "" {
		return t.TempDir()
	}
	base := filepath.Join(env, "win")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return t.TempDir()
	}
	d, err := os.MkdirTemp(base, strings.ReplaceAll(t.Name(), "/", "_")+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func waitFor(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func fileText(path string) string {
	b, _ := os.ReadFile(path)
	return string(b)
}

// startChild starts the test binary in modeChild and kills it when the test ends.
func startChild(t *testing.T, opt StartOptions, args ...string) Process {
	t.Helper()
	p, err := StartDetached(testExe(t), append([]string{modeChild}, args...), opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Kill()
		p.Wait(5 * time.Second)
	})
	return p
}

func TestListAndFindSelf(t *testing.T) {
	procs, err := ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	self := uint32(os.Getpid())
	var me Process
	for _, p := range procs {
		if p.PID == self {
			me = p
		}
	}
	if me.PID == 0 {
		t.Fatalf("own pid %d not in %d processes", self, len(procs))
	}
	if !strings.EqualFold(me.Name, filepath.Base(testExe(t))) {
		t.Errorf("name %q, want %q", me.Name, filepath.Base(testExe(t)))
	}
	if me.PPID != uint32(os.Getppid()) {
		t.Errorf("ppid %d, want %d", me.PPID, os.Getppid())
	}
	if me.Created == 0 {
		t.Fatal("own creation time unknown")
	}
	started := me.StartedAt()
	if started.After(time.Now()) || time.Since(started) > time.Hour {
		t.Errorf("StartedAt %v implausible", started)
	}
	withCreated := 0
	for _, p := range procs {
		if p.Created != 0 {
			withCreated++
		}
	}
	t.Logf("%d processes, %d with creation time; self started %s", len(procs), withCreated, started.Format(time.RFC3339))

	f, ok := FindProcess(self)
	if !ok || f != me {
		t.Errorf("FindProcess(self) = %+v %v, want %+v", f, ok, me)
	}
	if _, ok := FindProcess(0); ok {
		t.Error("FindProcess(0) found something")
	}
	if n := ProcessName(self); n != me.Name {
		t.Errorf("ProcessName = %q", n)
	}
	if !me.Alive() {
		t.Error("self not alive")
	}
	if _, ok := me.CPUTime(); !ok {
		t.Error("own CPU time unreadable")
	}
	if _, ok := me.ExitCode(); ok {
		t.Error("running process has an exit code")
	}
	if (Process{}).Alive() || !(Process{}).Wait(0) {
		t.Error("zero Process should be gone")
	}
	if !(Process{}).StartedAt().IsZero() {
		t.Error("zero Process has a start time")
	}
}

func TestCommandLineSelf(t *testing.T) {
	me, ok := FindProcess(uint32(os.Getpid()))
	if !ok {
		t.Fatal("self not found")
	}
	args, err := me.CommandLine()
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != len(os.Args) || !slices.Equal(args[1:], os.Args[1:]) ||
		!strings.EqualFold(filepath.Base(args[0]), filepath.Base(os.Args[0])) {
		t.Errorf("cmdline %q, want %q", args, os.Args)
	}
}

func TestSplitCommandLine(t *testing.T) {
	got, err := splitCommandLine(`"C:\Program Files\x.exe" -avd mas_0 -ports 20000,20001 "a b" x\"y c:\dir\ "" end`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`C:\Program Files\x.exe`, "-avd", "mas_0", "-ports", "20000,20001", "a b", `x"y`, `c:\dir\`, "", "end"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if got, err := splitCommandLine(""); err != nil || len(got) != 0 || got == nil {
		t.Errorf("empty line: %q %v", got, err)
	}
	// Round trip through Go's own quoting (what syscall.StartProcess puts on the command line).
	in := []string{`C:\a b\q.exe`, `tail\`, `quote"in`, `two\\"x`, "", "中文 参数", strings.Repeat("y", 9000)}
	var parts []string
	for _, a := range in {
		parts = append(parts, syscall.EscapeArg(a))
	}
	got, err = splitCommandLine(strings.Join(parts, " "))
	if err != nil || !slices.Equal(got, in) {
		t.Errorf("round trip: %q %v", got, err)
	}
}

func TestStartDetached(t *testing.T) {
	dir := workDir(t)
	out := filepath.Join(dir, "child.log")
	os.WriteFile(out, []byte("before\n"), 0o644)                                 // appended to, not truncated
	env := append(os.Environ(), "MODAVD_WIN_TEST=first", "modavd_win_test=last") // last one wins, any case
	args := []string{"60", "a b", `x"y`, `c:\dir with space\`, ""}
	p := startChild(t, StartOptions{OutputPath: out, Env: env}, args...)
	if p.Created == 0 || p.PID == 0 || p.PPID != uint32(os.Getpid()) || !strings.EqualFold(p.Name, filepath.Base(testExe(t))) {
		t.Fatalf("started %+v", p)
	}
	q, ok := FindProcess(p.PID)
	if !ok || q.Created != p.Created || q.PPID != uint32(os.Getpid()) {
		t.Fatalf("FindProcess = %+v %v, started %+v", q, ok, p)
	}
	if !waitFor(10*time.Second, func() bool { return strings.Contains(fileText(out), "child-stderr") }) {
		t.Fatalf("no output in %s: %q", out, fileText(out))
	}
	text := fileText(out)
	if !strings.HasPrefix(text, "before\n") {
		t.Errorf("output file not appended to: %q", text)
	}
	wantCwd := "cwd=" + filepath.Dir(testExe(t)) + " "
	if !strings.Contains(text, wantCwd) || !strings.Contains(text, "env=last\n") {
		t.Errorf("output %q, want %q and env=last", text, wantCwd)
	}
	cmd, err := p.CommandLine()
	if err != nil {
		t.Fatal(err)
	}
	if want := append([]string{testExe(t), modeChild}, args...); !slices.Equal(cmd, want) {
		t.Errorf("cmdline %q, want %q", cmd, want)
	}
	if !p.Alive() {
		t.Fatal("child not alive")
	}
	if _, ok := p.ExitCode(); ok {
		t.Error("running child has an exit code")
	}
	if _, ok := p.CPUTime(); !ok {
		t.Error("child CPU time unreadable")
	}
	start := time.Now()
	if p.Wait(200 * time.Millisecond) {
		t.Fatal("Wait returned true for a running child")
	}
	if d := time.Since(start); d < 150*time.Millisecond {
		t.Errorf("Wait(200ms) returned after %v", d)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if !p.Wait(10 * time.Second) {
		t.Fatal("child still running after Kill")
	}
	if p.Alive() {
		t.Error("killed child alive")
	}
	if code, ok := p.ExitCode(); !ok || code != 1 {
		t.Errorf("exit code after Kill = %d %v, want 1 true", code, ok)
	}
	if err := p.Kill(); err != nil {
		t.Errorf("second Kill: %v", err)
	}
}

func TestStartDetachedNulOutput(t *testing.T) {
	dir := workDir(t)
	p := startChild(t, StartOptions{Dir: dir}, "60")
	if !p.Alive() {
		t.Fatal("child not alive")
	}
	if _, err := StartDetached(filepath.Join(dir, "no-such-program.exe"), nil, StartOptions{}); errs.CodeOf(err) != errs.LaunchFailed {
		t.Errorf("missing exe: %v (%s)", err, errs.CodeOf(err))
	} else {
		t.Logf("missing exe: %v", err)
	}
}

func TestExitCode(t *testing.T) {
	p, err := StartDetached(testExe(t), []string{modeExit, "7"}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Wait(10 * time.Second) {
		p.Kill()
		t.Fatal("did not exit")
	}
	// The handle kept by StartDetached keeps the exit code readable.
	if code, ok := p.ExitCode(); !ok || code != 7 {
		t.Errorf("exit code %d %v, want 7 true", code, ok)
	}
	if _, ok := FindProcess(p.PID); ok {
		t.Error("exited process still found")
	}
	// The exited process whose object we still hold is in no snapshot (emu scans qemu with ListProcesses).
	procs, err := ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range procs {
		if q.PID == p.PID && q.Created == p.Created {
			t.Errorf("exited process listed: %+v", q)
		}
	}
	if _, err := p.CommandLine(); err == nil || !strings.Contains(err.Error(), "已经退出") {
		t.Errorf("CommandLine of an exited process: %v", err)
	}
	// The query-access fallback agrees with the handle.
	h, err := p.open(0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if code, ok := exitCode(h); !ok || code != 7 {
		t.Errorf("exitCode %d %v, want 7 true", code, ok)
	}
	self, _ := syscall.GetCurrentProcess()
	if _, ok := exitCode(self); ok {
		t.Error("exitCode of a running process ok")
	}
}

func TestIdentityMismatch(t *testing.T) {
	p := startChild(t, StartOptions{}, "60")
	other := p
	other.Created++ // same PID, different process
	if other.Alive() {
		t.Error("mismatched identity alive")
	}
	if !other.Wait(5 * time.Second) {
		t.Error("Wait on mismatched identity did not return true")
	}
	if err := other.Kill(); err != nil {
		t.Errorf("Kill on mismatched identity: %v", err)
	}
	if _, err := other.CommandLine(); err == nil {
		t.Error("CommandLine on mismatched identity succeeded")
	}
	if _, ok := other.CPUTime(); ok {
		t.Error("CPUTime on mismatched identity succeeded")
	}
	if err := SetAboveNormalPriority(other); err == nil {
		t.Error("SetAboveNormalPriority on mismatched identity succeeded")
	}
	if KillTree(other) != nil {
		t.Error("KillTree on mismatched identity failed")
	}
	time.Sleep(200 * time.Millisecond)
	if !p.Alive() {
		t.Fatal("the real process died from calls on a mismatched identity")
	}
}

func TestKillTree(t *testing.T) {
	dir := workDir(t)
	out := filepath.Join(dir, "tree.log")
	tree, err := StartDetached(testExe(t), []string{modeTree}, StartOptions{OutputPath: out})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { KillTree(tree) })
	var gpid int
	if !waitFor(10*time.Second, func() bool {
		_, after, ok := strings.Cut(fileText(out), "grandchild=")
		if ok {
			gpid, _ = strconv.Atoi(strings.TrimSpace(after))
		}
		return gpid != 0
	}) {
		t.Fatalf("no grandchild reported: %q", fileText(out))
	}
	g, ok := FindProcess(uint32(gpid))
	if !ok || g.PPID != tree.PID {
		t.Fatalf("grandchild %+v %v, want parent %d", g, ok, tree.PID)
	}
	t.Cleanup(func() { g.Kill() })
	if err := KillTree(tree); err != nil {
		t.Fatal(err)
	}
	if !tree.Wait(10*time.Second) || !g.Wait(10*time.Second) {
		t.Fatalf("tree alive %v, grandchild alive %v", tree.Alive(), g.Alive())
	}
}

func TestStartOrphan(t *testing.T) {
	exe := testExe(t)
	dir := workDir(t)
	out := filepath.Join(dir, "sub", "orphan.log")
	old := OrphanHelper
	OrphanHelper = exe
	defer func() { OrphanHelper = old }()

	env := append(os.Environ(), "MODAVD_WIN_TEST=orphan")
	start := time.Now()
	p, err := StartOrphan(exe, []string{modeChild, "60"}, StartOptions{OutputPath: out, Env: env})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("StartOrphan took %v: %+v", time.Since(start), p)
	t.Cleanup(func() {
		p.Kill()
		p.Wait(5 * time.Second)
	})
	if p.PPID == 0 || p.PPID == uint32(os.Getpid()) || p.Created == 0 {
		t.Fatalf("orphan %+v (we are %d)", p, os.Getpid())
	}
	q, ok := FindProcess(p.PID)
	if !ok || q.PPID != p.PPID || q.Created != p.Created {
		t.Fatalf("FindProcess = %+v %v, want %+v", q, ok, p)
	}
	// The helper has exited: nothing that existed before the grandchild holds its parent PID any more.
	procs, err := ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range procs {
		if x.PID == p.PPID && (x.Created == 0 || x.Created <= p.Created) {
			t.Fatalf("helper %+v still running", x)
		}
	}
	if !waitFor(10*time.Second, func() bool { return strings.Contains(fileText(out), "child-stderr") }) {
		t.Fatalf("no output: %q", fileText(out))
	}
	if text := fileText(out); !strings.Contains(text, "env=orphan\n") || !strings.Contains(text, "cwd="+filepath.Dir(exe)+" ") {
		t.Errorf("output %q", text)
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if !p.Wait(10 * time.Second) {
		t.Fatal("orphan still running after Kill")
	}
	// The handle the helper duplicated into us keeps the exit code.
	if code, ok := p.ExitCode(); !ok || code != 1 {
		t.Errorf("exit code %d %v, want 1 true", code, ok)
	}
}

func TestStartOrphanErrors(t *testing.T) {
	old := OrphanHelper
	defer func() { OrphanHelper = old }()

	OrphanHelper = testExe(t)
	if _, err := StartOrphan(testExe(t), []string{modeChild, "1"}, StartOptions{Output: os.Stderr}); err == nil {
		t.Error("Output accepted")
	}
	_, err := StartOrphan(filepath.Join(workDir(t), "no-such-program.exe"), nil, StartOptions{})
	if errs.CodeOf(err) != errs.LaunchFailed || !strings.Contains(err.Error(), "no-such-program.exe") {
		t.Errorf("missing exe through the helper: %v", err)
	} else {
		t.Logf("missing exe through the helper: %v", err)
	}

	OrphanHelper = "" // falls back to StartDetached
	p, err := StartOrphan(testExe(t), []string{modeChild, "60"}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Kill()
	if p.PPID != uint32(os.Getpid()) {
		t.Errorf("fallback parent %d, want %d", p.PPID, os.Getpid())
	}
}

func TestSpawnHelperMainBadArgs(t *testing.T) {
	dir := workDir(t)
	missing, err := json.Marshal(map[string]string{"exe": filepath.Join(dir, "none.exe")})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{nil, {"{not json"}, {`{"exe":""}`}, {string(missing)}} {
		path := filepath.Join(dir, "reply.json")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		code := SpawnHelperMain(args, f)
		f.Close()
		text := fileText(path)
		if code != 1 || !strings.HasPrefix(text, `{"error":"`) || strings.Count(text, "\n") != 1 {
			t.Errorf("args %q: code %d, reply %q", args, code, text)
		}
	}
}

func TestChildEnv(t *testing.T) {
	got := childEnv([]string{"A=1", "=C:=C:\\x", "B=2", "a=3", "bad", "=D:=D:\\y"})
	if !slices.Contains(got, "a=3") || slices.Contains(got, "A=1") || !slices.Contains(got, "B=2") ||
		!slices.Contains(got, `=C:=C:\x`) || !slices.Contains(got, `=D:=D:\y`) || slices.Contains(got, "bad") {
		t.Errorf("childEnv = %q", got)
	}
	if i, j := slices.Index(got, "B=2"), slices.Index(got, "a=3"); i > j {
		t.Errorf("order not kept: %q", got)
	}
	hasRoot := false
	for _, kv := range got {
		if strings.HasPrefix(strings.ToUpper(kv), "SYSTEMROOT=") {
			hasRoot = true
		}
	}
	if !hasRoot {
		t.Errorf("SYSTEMROOT missing: %q", got)
	}
	if n := len(childEnv(nil)); n == 0 {
		t.Error("inherited environment empty")
	}
}

func TestFileLock(t *testing.T) {
	path := filepath.Join(workDir(t), "locks", "x.lock")
	l1, ok, err := TryLock(path)
	if err != nil || !ok {
		t.Fatalf("first TryLock: %v %v", ok, err)
	}
	// A second handle in the same process conflicts.
	if l, ok, err := TryLock(path); err != nil || ok {
		l.Unlock()
		t.Fatalf("second TryLock: %v %v", ok, err)
	}
	start := time.Now()
	if l, ok, err := Lock(path, 300*time.Millisecond); err != nil || ok {
		l.Unlock()
		t.Fatalf("Lock while held: %v %v", ok, err)
	}
	if d := time.Since(start); d < 250*time.Millisecond || d > 3*time.Second {
		t.Errorf("Lock(300ms) took %v", d)
	}
	// Another process sees it too.
	b, err := exec.Command(testExe(t), modeLock, path).CombinedOutput()
	if err != nil || !strings.Contains(string(b), "locked=false err=<nil>") {
		t.Errorf("other process: %q %v", b, err)
	}
	// A waiter gets it once released.
	got := make(chan bool, 1)
	go func() {
		l, ok, _ := Lock(path, 5*time.Second)
		l.Unlock()
		got <- ok
	}()
	time.Sleep(300 * time.Millisecond)
	l1.Unlock()
	l1.Unlock() // idempotent
	if !<-got {
		t.Fatal("waiter did not get the lock")
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file gone after Unlock: %v", err)
	}
	b, err = exec.Command(testExe(t), modeLock, path).CombinedOutput()
	if err != nil || !strings.Contains(string(b), "locked=true") {
		t.Errorf("other process after release: %q %v", b, err)
	}
	var nilLock *FileLock
	nilLock.Unlock()
}

func TestListeners(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	self := uint32(os.Getpid())

	ls, err := Listeners()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(ls, Listener{Port: port, PID: self}) {
		t.Fatalf("listener %d/%d not in %d entries", port, self, len(ls))
	}
	pid, ok, err := ListenerPID(port)
	if err != nil || !ok || pid != self {
		t.Errorf("ListenerPID = %d %v %v", pid, ok, err)
	}

	// A port nobody listens on: listen, close, then expect it gone.
	tmp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := tmp.Addr().(*net.TCPAddr).Port
	tmp.Close()
	busy, err := BusyPorts([]int{free, port, port})
	if err != nil || !slices.Equal(busy, []int{port}) {
		t.Errorf("BusyPorts = %v %v, want [%d]", busy, err, port)
	}
	if _, ok, _ := ListenerPID(free); ok {
		t.Errorf("closed port %d still listening", free)
	}

	if ln6, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		defer ln6.Close()
		port6 := ln6.Addr().(*net.TCPAddr).Port
		ls, err := Listeners()
		if err != nil || !slices.Contains(ls, Listener{Port: port6, PID: self}) {
			t.Errorf("IPv6 listener %d not found (%v)", port6, err)
		}
	} else {
		t.Logf("no IPv6 loopback: %v", err)
	}
}

func TestParseTCPTable(t *testing.T) {
	row := func(buf []byte, off, stateOff, portOff, pidOff int, state uint32, port uint16, pid uint32) {
		binary.LittleEndian.PutUint32(buf[off+stateOff:], state)
		binary.BigEndian.PutUint16(buf[off+portOff:], port) // network byte order in the low 16 bits
		binary.LittleEndian.PutUint32(buf[off+pidOff:], pid)
	}
	buf := make([]byte, 4+3*tcp4RowSize)
	binary.LittleEndian.PutUint32(buf, 3)
	row(buf, 4, tcp4State, tcp4Port, tcp4PID, mibTCPStateListen, 20089, 1234)
	row(buf, 4+tcp4RowSize, tcp4State, tcp4Port, tcp4PID, 5, 20090, 99) // ESTABLISHED: skipped
	row(buf, 4+2*tcp4RowSize, tcp4State, tcp4Port, tcp4PID, mibTCPStateListen, 5037, 42)
	if got := parseTCPTable(buf, tcp4RowSize, tcp4State, tcp4Port, tcp4PID); !slices.Equal(got,
		[]Listener{{20089, 1234}, {5037, 42}}) {
		t.Errorf("v4 %v", got)
	}
	buf6 := make([]byte, 4+tcp6RowSize)
	binary.LittleEndian.PutUint32(buf6, 2) // claims 2 rows, holds 1: no overrun
	row(buf6, 4, tcp6State, tcp6Port, tcp6PID, mibTCPStateListen, 20050, 7)
	if got := parseTCPTable(buf6, tcp6RowSize, tcp6State, tcp6Port, tcp6PID); !slices.Equal(got, []Listener{{20050, 7}}) {
		t.Errorf("v6 %v", got)
	}
	if got := parseTCPTable(nil, tcp4RowSize, tcp4State, tcp4Port, tcp4PID); got != nil {
		t.Errorf("empty %v", got)
	}
}

func TestSharedMemory(t *testing.T) {
	name := fmt.Sprintf("SHM_videmulator_modavd_test_%d", os.Getpid())
	if SharedMemoryExists(name) || SharedMemoryExists("") {
		t.Fatal("absent mapping reported present")
	}
	p, _ := syscall.UTF16PtrFromString(name)
	h, err := syscall.CreateFileMapping(syscall.InvalidHandle, nil, syscall.PAGE_READWRITE, 0, 4096, p)
	if err != nil {
		t.Fatal(err)
	}
	if !SharedMemoryExists(name) {
		t.Error("existing mapping not found")
	}
	syscall.CloseHandle(h)
	if SharedMemoryExists(name) {
		t.Error("closed mapping still found")
	}
}

func TestDiskFreeAndMemory(t *testing.T) {
	dir := workDir(t)
	free, err := DiskFree(dir)
	if err != nil || free == 0 {
		t.Fatalf("DiskFree(%s) = %d %v", dir, free, err)
	}
	deep, err := DiskFree(filepath.Join(dir, "no", "such", "dir", "file.img"))
	if err != nil || deep == 0 {
		t.Fatalf("DiskFree(missing) = %d %v", deep, err)
	}
	file := filepath.Join(dir, "f.txt")
	os.WriteFile(file, []byte("x"), 0o644)
	if n, err := DiskFree(file); err != nil || n == 0 {
		t.Errorf("DiskFree(file) = %d %v", n, err)
	}
	t.Logf("free on %s: %.1f GiB", filepath.VolumeName(dir), float64(free)/(1<<30))

	mb, err := AvailableMemoryMB()
	if err != nil || mb == 0 {
		t.Fatalf("AvailableMemoryMB = %d %v", mb, err)
	}
	t.Logf("available memory: %d MiB", mb)
}

func TestBigL3CPUs(t *testing.T) {
	raw := cacheTopology()
	groups := parseL3Groups(raw)
	mask, cpus, ok := BigL3CPUs()
	t.Logf("cache topology %d bytes, L3 groups %+v; BigL3CPUs = 0x%x %v %v", len(raw), groups, mask, cpus, ok)
	if len(raw) == 0 {
		t.Error("no cache topology")
	}
}

func TestBigL3Parse(t *testing.T) {
	entry := func(level byte, size uint32, mask uint64) []byte {
		b := make([]byte, 56)
		binary.LittleEndian.PutUint32(b[0:], relationCache)
		binary.LittleEndian.PutUint32(b[4:], 56)
		b[8] = level
		binary.LittleEndian.PutUint32(b[12:], size)
		binary.LittleEndian.PutUint64(b[40:], mask)
		return b
	}
	cat := func(parts ...[]byte) []byte { return slices.Concat(parts...) }

	x3d := cat(entry(1, 32<<10, 1), entry(3, 96<<20, 0x0000FFFF), entry(2, 1<<20, 1), entry(3, 32<<20, 0xFFFF0000))
	groups := parseL3Groups(x3d)
	if len(groups) != 2 || groups[0] != (l3Group{96 << 20, 0xFFFF}) || groups[1] != (l3Group{32 << 20, 0xFFFF0000}) {
		t.Fatalf("groups %+v", groups)
	}
	mask, cpus, ok := bigL3(groups)
	if !ok || mask != 0xFFFF || !slices.Equal(cpus, []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}) {
		t.Errorf("x3d: 0x%x %v %v", mask, cpus, ok)
	}
	// Big CCD second in the list.
	if mask, _, ok := bigL3(parseL3Groups(cat(entry(3, 32<<20, 0xFF), entry(3, 96<<20, 0xFF00)))); !ok || mask != 0xFF00 {
		t.Errorf("x3d reversed: 0x%x %v", mask, ok)
	}
	// Two equal L3 (7950X), a single L3, no L3: ordinary.
	for name, raw := range map[string][]byte{
		"equal":  cat(entry(3, 32<<20, 0xFF), entry(3, 32<<20, 0xFF00)),
		"single": cat(entry(2, 1<<20, 1), entry(3, 36<<20, 0xFFFFFF)),
		"none":   cat(entry(1, 32<<10, 1)),
		"empty":  nil,
	} {
		if _, _, ok := bigL3(parseL3Groups(raw)); ok {
			t.Errorf("%s: reported big L3", name)
		}
	}
	// A zero entry size stops the walk before that entry is read (AUTO-MAS breaks there too).
	broken := entry(3, 96<<20, 0xFF)
	binary.LittleEndian.PutUint32(broken[4:], 0)
	if g := parseL3Groups(cat(entry(3, 32<<20, 0xFF00), broken, entry(3, 16<<20, 0xF))); len(g) != 1 {
		t.Errorf("zero size entry: %+v", g)
	}
	// A truncated last entry is ignored.
	if g := parseL3Groups(cat(entry(3, 32<<20, 0xFF00), entry(3, 16<<20, 0xF)[:47])); len(g) != 1 {
		t.Errorf("truncated entry: %+v", g)
	}
}

var (
	procGetPriorityClass       = modkernel32.NewProc("GetPriorityClass")
	procGetProcessAffinityMask = modkernel32.NewProc("GetProcessAffinityMask")
	procGetModuleHandleW       = modkernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW       = moduser32.NewProc("RegisterClassExW")
	procUnregisterClassW       = moduser32.NewProc("UnregisterClassW")
	procCreateWindowExW        = moduser32.NewProc("CreateWindowExW")
	procDestroyWindow          = moduser32.NewProc("DestroyWindow")
	procDefWindowProcW         = moduser32.NewProc("DefWindowProcW")
	procIsWindowVisible        = moduser32.NewProc("IsWindowVisible")
	procPeekMessageW           = moduser32.NewProc("PeekMessageW")
	procTranslateMessage       = moduser32.NewProc("TranslateMessage")
	procDispatchMessageW       = moduser32.NewProc("DispatchMessageW")
)

func TestPriorityAndAffinity(t *testing.T) {
	p := startChild(t, StartOptions{}, "60")
	if err := SetAboveNormalPriority(p); err != nil {
		t.Fatal(err)
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, p.PID)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(h)
	if c, _, _ := procGetPriorityClass.Call(uintptr(h)); c != aboveNormalPriorityClass {
		t.Errorf("priority class 0x%x", c)
	}
	if err := SetAffinity(p, 1); err != nil { // CPU 0 always exists
		t.Fatal(err)
	}
	var got, sysMask uintptr
	if r, _, err := procGetProcessAffinityMask.Call(uintptr(h), uintptr(unsafe.Pointer(&got)), uintptr(unsafe.Pointer(&sysMask))); r == 0 {
		t.Fatal(err)
	} else if got != 1 {
		t.Errorf("affinity 0x%x, want 1", got)
	}
	if err := SetAffinity(p, 0); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("mask 0: %v", err)
	}
	p.Kill()
	p.Wait(5 * time.Second)
	if err := SetAboveNormalPriority(p); err == nil {
		t.Error("priority on a killed process succeeded")
	} else {
		t.Logf("after kill: %v", err)
	}
	if err := SetAffinity(p, 1); err == nil {
		t.Error("affinity on a killed process succeeded")
	}
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	X, Y    int32
	Private uint32
}

// testWindows creates visible 1x1 tool windows far off-screen (never activated, no taskbar button) on a thread that
// pumps messages, so ShowWindowAsync takes effect. Returns their handles in the order of specs.
func testWindows(t *testing.T, specs [][2]string) []uintptr {
	t.Helper()
	type result struct {
		hwnds []uintptr
		err   error
	}
	ready := make(chan result, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(done)
		inst, _, _ := procGetModuleHandleW.Call(0)
		classes := map[string]*uint16{}
		var hwnds []uintptr
		defer func() {
			for _, h := range hwnds {
				procDestroyWindow.Call(h)
			}
			for _, c := range classes {
				procUnregisterClassW.Call(uintptr(unsafe.Pointer(c)), inst)
			}
		}()
		for _, s := range specs {
			cls, ok := classes[s[0]]
			if !ok {
				cls, _ = syscall.UTF16PtrFromString(s[0])
				wc := wndClassEx{WndProc: procDefWindowProcW.Addr(), Instance: inst, ClassName: cls}
				wc.Size = uint32(unsafe.Sizeof(wc))
				if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
					ready <- result{err: fmt.Errorf("RegisterClassExW %s: %v", s[0], err)}
					return
				}
				classes[s[0]] = cls
			}
			title, _ := syscall.UTF16PtrFromString(s[1])
			off := -32000
			const wsExToolWindow, wsExNoActivate, wsPopup, wsVisible = 0x80, 0x08000000, 0x80000000, 0x10000000
			h, _, err := procCreateWindowExW.Call(wsExToolWindow|wsExNoActivate, uintptr(unsafe.Pointer(cls)),
				uintptr(unsafe.Pointer(title)), wsPopup|wsVisible, uintptr(off), uintptr(off), 1, 1, 0, 0, inst, 0)
			if h == 0 {
				ready <- result{err: fmt.Errorf("CreateWindowExW %q: %v", s[1], err)}
				return
			}
			hwnds = append(hwnds, h)
		}
		ready <- result{hwnds: hwnds}
		var m winMsg
		for {
			select {
			case <-stop:
				return
			default:
			}
			for {
				if r, _, _ := procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1); r == 0 {
					break
				}
				procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
				procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	res := <-ready
	if res.err != nil {
		<-done
		t.Skipf("cannot create test windows: %v", res.err)
	}
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return res.hwnds
}

func TestSetEmulatorWindowsVisible(t *testing.T) {
	self := uint32(os.Getpid())
	if n, err := SetEmulatorWindowsVisible(self, "mas_7:20070", false); err != nil || n != 0 {
		t.Fatalf("before any window: %d %v", n, err)
	}
	if n, err := SetEmulatorWindowsVisible(0, "mas_7:20070", true); err != nil || n != 0 {
		t.Fatalf("pid 0: %d %v", n, err)
	}
	qt := fmt.Sprintf("Qt5QWindowModavdTest%d", self)
	other := fmt.Sprintf("ModavdTestWindow%d", self)
	w := testWindows(t, [][2]string{
		{qt, "Android Emulator - mas_7:20070"}, // main window: touched
		{qt, "Emulator"},                       // toolbar: touched
		{qt, "Android Emulator - mas_8:20080"}, // another instance's title
		{other, "Emulator"},                    // not a Qt window
	})
	visible := func(h uintptr) bool { r, _, _ := procIsWindowVisible.Call(h); return r != 0 }
	for i, h := range w {
		if !visible(h) {
			t.Fatalf("window %d not visible at start", i)
		}
	}
	n, err := SetEmulatorWindowsVisible(self, "mas_7:20070", false)
	if err != nil || n != 2 {
		t.Fatalf("hide: %d %v, want 2", n, err)
	}
	if !waitFor(3*time.Second, func() bool { return !visible(w[0]) && !visible(w[1]) }) {
		t.Fatalf("still visible: main %v toolbar %v", visible(w[0]), visible(w[1]))
	}
	if !visible(w[2]) || !visible(w[3]) {
		t.Errorf("untargeted windows hidden: %v %v", visible(w[2]), visible(w[3]))
	}
	// Other processes' windows are never matched.
	if n, err := SetEmulatorWindowsVisible(uint32(os.Getppid()), "mas_7:20070", false); err != nil || n != 0 {
		t.Errorf("parent process: %d %v", n, err)
	}
}
