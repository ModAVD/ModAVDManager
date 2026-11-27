package report

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/root"
)

// Sections collect-info.ps1 never had, so that whoever sends the problem package is not asked for these separately:
// what the manager and the boot watch logged, every adb.exe with the adb server's own log, and the TCP ports Windows
// keeps for itself. The parity tests leave these sections and their files out (goOnlySections, goOnlyEntry).
const (
	sectionManagerLogs  = "管理器日志"
	sectionAdbProcesses = "adb.exe 进程与 adb server 日志"
	sectionPortExcluded = "Windows 保留的 TCP 端口（netsh）"
)

var (
	// managerLogTailBytes: modavd-ui.log starts over at 5 MB (one .1 backup), the boot watch's log at 4 MB.
	managerLogTailBytes int64 = 5 << 20
	// adbLogTailBytes: adb.log is shared by every adb server of the Windows user and only ever appended to.
	adbLogTailBytes int64 = 2 << 20
)

const netshTimeout = 15 * time.Second

// managerLogs are the manager's own logs in logs\, with the private adb servers' log (modavd before 0.7 had none); a
// missing quiet one is not mentioned.
var managerLogs = []struct {
	name, what string
	quiet      bool
}{
	{"modavd-ui.log", "管理器日志", false},
	{"modavd-ui.log.1", "管理器日志（上一份）", true},
	{"modavd-bootwatch.log", "开机守护日志", false},
	{"modavd-bootwatch.log.1", "开机守护日志（上一份）", true},
	{"adb-server.log", "私有 adb server 日志", true},
	{"adb-server.log.1", "私有 adb server 日志（上一份）", true},
}

// extra runs one of these sections like step (a failure or a panic is a line and a problem), without a timing line.
func (c *collector) extra(name string, fn func() error) {
	if c.ctx.Err() != nil {
		return
	}
	c.progress("正在收集：" + name)
	err := func() (err error) {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("内部错误：%v", p)
			}
		}()
		return fn()
	}()
	if err != nil {
		msg := errText(err)
		c.L("[本节出错] " + name + "：" + msg)
		c.problems = append(c.problems, name+"："+msg)
	}
}

// managerLogFiles copies modavd-ui.log, modavd-bootwatch.log, adb-server.log and their .1 backups (the last
// managerLogTailBytes of each) into logs\ of the package.
func (c *collector) managerLogFiles() error {
	c.section(sectionManagerLogs)
	logDir := filepath.Join(c.root, root.LogsDir)
	for _, f := range managerLogs {
		src := filepath.Join(logDir, f.name)
		if !isFile(src) {
			if !f.quiet {
				c.L(f.what + "（" + f.name + "）：没有")
			}
			continue
		}
		c.copyTail(f.what+"（"+f.name+"）", src, filepath.Join(c.stage, root.LogsDir, f.name), managerLogTailBytes)
	}
	return nil
}

// copyTail copies the last tail bytes of src to dst and says how much in one line.
func (c *collector) copyTail(label, src, dst string, tail int64) {
	n, err := trueLength(src)
	var got int64
	if err == nil {
		got, err = copyShared(src, dst, tail)
	}
	switch {
	case err != nil:
		c.L(label + "：复制失败：" + errText(err))
	case n > tail:
		c.L(fmt.Sprintf("%s：%s，只取了最后 %s", label, formatSize(float64(n)), formatSize(float64(got))))
	default:
		c.L(fmt.Sprintf("%s：%s", label, formatSize(float64(got))))
	}
}

// adbProcesses lists every adb.exe (a server that never got to listen, another program's adb) and copies the end of
// the adb server log: ANDROID_ADB_LOG_PATH, else adb.log in the temporary directory (GetTempPath: TMP, then TEMP),
// where every adb server of this Windows user writes why it started, stopped or could not listen.
func (c *collector) adbProcesses() error {
	c.section(sectionAdbProcesses)
	procs, err := c.h.Processes(func(name string) bool { return strings.EqualFold(name, "adb.exe") })
	if err != nil {
		// the same process table as 正在运行的模拟器与 adb, which already counts the failure; the log still matters
		c.L("adb.exe 进程：读不到（" + errText(err) + "）")
		procs = nil
	}
	sort.SliceStable(procs, func(i, j int) bool {
		if !procs[i].Started.Equal(procs[j].Started) {
			return procs[i].Started.Before(procs[j].Started)
		}
		return procs[i].PID < procs[j].PID
	})
	if err == nil && len(procs) == 0 {
		c.L("adb.exe 进程：没有")
	} else if len(procs) > 0 {
		c.L(fmt.Sprintf("adb.exe 进程：%d 个", len(procs)))
	}
	for _, p := range procs {
		started, path := "", p.ExePath
		if !p.Started.IsZero() {
			started = p.Started.Local().Format("2006-01-02 15:04:05")
		}
		if path == "" {
			path = "（路径读不到）"
		}
		c.L(fmt.Sprintf("  pid %d  启动于 %s  %s", p.PID, started, path))
		if p.CommandLine != "" {
			c.L("    " + p.CommandLine)
		}
	}

	log := c.opt.getenv("ANDROID_ADB_LOG_PATH")
	if log == "" {
		tmp := c.opt.getenv("TMP")
		if tmp == "" {
			tmp = c.opt.getenv("TEMP")
		}
		if tmp == "" {
			c.L("adb server 日志：找不到临时目录（TMP、TEMP 都没有）")
			return nil
		}
		log = filepath.Join(tmp, "adb.log")
	}
	if !isFile(log) {
		c.L("adb server 日志（" + log + "）：没有")
		return nil
	}
	c.copyTail("adb server 日志（"+log+"）", log, filepath.Join(c.stage, "adb", "adb.log"), adbLogTailBytes)
	return nil
}

// excludedRange is one line of `netsh interface ipv4 show excludedportrange`: start port, end port.
var excludedRange = regexp.MustCompile(`^\s*([0-9]+)\s+([0-9]+)\b`)

// portUse is a port this root needs and what for.
type portUse struct {
	port int
	what string
}

// portExclusions shows the TCP port ranges Windows keeps for itself (Hyper-V, WinNAT, Docker take ranges at boot;
// nothing else can listen there) and says which ports of this root fall into one: the two adb servers and the
// console, adb and gRPC ports of every instance.
func (c *collector) portExclusions() error {
	c.section(sectionPortExcluded)
	sys := c.opt.getenv("SystemRoot")
	if sys == "" {
		sys = c.opt.getenv("windir")
	}
	if sys == "" {
		sys = `C:\Windows`
	}
	r, err := c.h.Run(c.ctx, filepath.Join(sys, "System32", "netsh.exe"),
		[]string{"interface", "ipv4", "show", "excludedportrange", "protocol=tcp"}, netshTimeout)
	if err != nil {
		return err
	}
	if r.TimedOut {
		c.L("netsh 超时（15 秒），已放弃")
		return nil
	}
	var ranges [][2]int
	for _, line := range splitCRLF(adb.DecodeOutput(r.Stdout) + "\n" + adb.DecodeOutput(r.Stderr)) {
		if netTrim(line) == "" {
			continue
		}
		c.L("  " + netTrimEnd(line))
		if m := excludedRange.FindStringSubmatch(line); m != nil {
			lo, _ := strconv.Atoi(m[1])
			hi, _ := strconv.Atoi(m[2])
			ranges = append(ranges, [2]int{lo, hi})
		}
	}
	if r.ExitCode != 0 {
		c.L(fmt.Sprintf("netsh 退出码 %d，看不出保留了哪些端口", r.ExitCode))
		return nil
	}
	rt := &root.Root{Dir: c.root}
	uses := []portUse{{rt.AdbServerPort(), "私有 adb server"}, {rt.ScriptAdbServerPort(), "脚本 adb server"}}
	for _, n := range c.instances {
		if i, ok := diagnosticIndex(n); ok {
			uses = append(uses, portUse{c.plan.Console(i), n + " 控制台"}, portUse{c.plan.Adb(i), n + " adb"},
				portUse{c.plan.Grpc(i), n + " gRPC"})
		}
	}
	var hits []string
	for _, u := range uses {
		for _, rg := range ranges {
			if u.port >= rg[0] && u.port <= rg[1] {
				hits = append(hits, fmt.Sprintf("%d（%s）在 %d–%d 里", u.port, u.what, rg[0], rg[1]))
				break
			}
		}
	}
	if len(hits) == 0 {
		c.L("本目录要用的端口都不在保留范围里")
	} else {
		c.L("警告：本目录要用的端口被 Windows 保留了，adb server 或模拟器在这些端口上监听不了：" + strings.Join(hits, "；"))
	}
	return nil
}
