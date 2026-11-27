// Copyright 2026 Ji Mao Cai
package emu

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/host"
	"modavd/internal/logx"
	"modavd/internal/root"
)

const DiagnosticArchiveArg = "__diagnostic_crashes"

func (m *Manager) waitCrashArchiveStopped(ctx context.Context, i int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l, free, err := host.Lock(m.lockPath(i, "crash-copy"), 5*time.Second)
	if err != nil {
		return err
	}
	if !free {
		return fmt.Errorf("转储复制仍在收尾")
	}
	l.Unlock()
	return nil
}

var diagnosticCrashDB = regexp.MustCompile(`(?i)^emu-crash-[0-9]+\.[0-9]+\.[0-9]+-mas\.db$`)

type crashScanState struct {
	Policy  diagnostics.State
	Through time.Time
}

type crashSource struct {
	path    string
	db      string
	info    fs.FileInfo
	parents []crashParent
}

type crashParent struct {
	path string
	info fs.FileInfo
}

func (m *Manager) crashScanPath(i int) string {
	return filepath.Join(m.Root.LogsDir(), "."+root.AvdName(i)+"-crashscan.json")
}

// startDiagnosticArchive starts bounded background work which survives a short CLI. Ordinary starts do not scan
// crashpad at all. Each helper owns only one instance and exits after this copy, rather than becoming a service.
func (m *Manager) startDiagnosticArchive(i int) {
	if !root.CurrentPlatform().Windows() {
		return
	}
	states, err := diagnostics.Read(m.Root)
	if err != nil {
		m.Log.Warnf("实例 %d 读不了诊断策略，未复制崩溃转储: %v", i, err)
		return
	}
	state := states[i]
	if !state.Enabled {
		return
	}
	_, err = host.StartOrphan(filepath.Join(m.Root.Dir, "tools", root.CurrentPlatform().CLI()), []string{DiagnosticArchiveArg,
		m.Root.Dir, strconv.Itoa(i), state.Since, state.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)},
		host.StartOptions{Env: m.ADB.Env()})
	if err != nil {
		m.Log.Warnf("实例 %d 诊断转储后台复制没起来，下次开机重试: %v", i, err)
	}
}

func RunDiagnosticArchive(args []string, stderr io.Writer) int {
	if len(args) != 5 {
		return 2
	}
	if !root.CurrentPlatform().Windows() {
		fmt.Fprintln(stderr, "当前平台尚不支持崩溃转储归档")
		return 1
	}
	i, err := strconv.Atoi(args[1])
	through, timeErr := time.Parse(time.RFC3339Nano, args[4])
	if err != nil || !root.ValidIndex(i) || timeErr != nil {
		return 2
	}
	r, err := root.New(args[0])
	if err != nil {
		return 1
	}
	m := New(r, logx.New(logx.JSONSink(stderr, logx.Info)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	expected := diagnostics.State{Enabled: true, Since: args[2], CreatedAt: args[3]}
	if err := m.archiveDiagnosticCrashes(ctx, i, expected, through); err != nil {
		m.Log.Warnf("实例 %d 诊断转储未复制完整，下次开机重试: %v", i, err)
		return 1
	}
	return 0
}

func (m *Manager) archiveDiagnosticCrashes(ctx context.Context, i int, expected diagnostics.State, through time.Time) error {
	lock, ok, err := host.Lock(m.lockPath(i, "crash-copy"), 5*time.Second)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	} // another boot's helper is still working; its watermark is not advanced for this request
	defer lock.Unlock()
	states, err := diagnostics.Read(m.Root)
	if err != nil {
		return err
	}
	if !expected.Enabled || states[i] != expected {
		return nil
	}
	if err := m.pruneCrashStaging(i); err != nil {
		return err
	}
	since := through.Add(-diagnostics.MaxAge)
	var previous crashScanState
	if b, err := root.ReadFileShared(m.crashScanPath(i)); err == nil {
		if err := json.Unmarshal(b, &previous); err != nil {
			return err
		}
		if previous.Policy == expected && previous.Through.After(since) {
			since = previous.Through
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sources, err := diagnosticCrashSources(since, through)
	if err != nil {
		return err
	}
	copyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watchDone:
				return
			case <-copyCtx.Done():
				return
			case <-ticker.C:
				current, err := diagnostics.Read(m.Root)
				if err != nil || current[i] != expected {
					cancel()
					return
				}
			}
		}
	}()
	for _, source := range sources {
		if err := copyCtx.Err(); err != nil {
			if m.crashPolicyChanged(i, expected) {
				return nil
			}
			return err
		}
		if source.info.Size() > diagnostics.MaxBytes {
			return fmt.Errorf("转储 %s 超过诊断保留总量，未复制", source.info.Name())
		}
		free, err := diagnosticDiskFree(m.Root.LogsDir())
		if err != nil {
			return err
		}
		if free < diagnostics.MinFreeBytes+uint64(source.info.Size()) {
			return fmt.Errorf("磁盘空间不足，转储复制暂缓")
		}
		if err := m.copyAndPublishCrash(copyCtx, i, expected, source); err != nil {
			if m.crashPolicyChanged(i, expected) {
				return nil
			}
			return err
		}
	}
	// A partial or interrupted copy never moves the watermark. Config + retention locks are short and contain
	// only publication/state writes, so a large copy cannot delay another instance's start or logcat rotation.
	config, err := m.Root.LockInstanceConfig(i)
	if err != nil {
		return err
	}
	defer config.Unlock()
	retention, err := m.retentionLock()
	if err != nil {
		return err
	}
	defer retention.Unlock()
	states, err = diagnostics.Read(m.Root)
	if err != nil {
		return err
	}
	if states[i] != expected || copyCtx.Err() != nil {
		return nil
	}
	if _, err := m.instance(i); err != nil {
		return err
	}
	if previous.Policy == expected && previous.Through.After(through) {
		return nil
	}
	b, err := json.Marshal(crashScanState{Policy: expected, Through: through})
	if err != nil {
		return err
	}
	path := m.crashScanPath(i)
	tmp := path + "." + strconv.Itoa(os.Getpid()) + ".tmp"
	defer os.Remove(tmp)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) crashPolicyChanged(i int, expected diagnostics.State) bool {
	states, err := diagnostics.Read(m.Root)
	return err == nil && states[i] != expected
}

func (m *Manager) pruneCrashStaging(i int) error {
	lock, err := m.retentionLock()
	if err != nil {
		return err
	}
	defer lock.Unlock()
	entries, err := os.ReadDir(m.Root.LogsDir())
	if err != nil {
		return err
	}
	prefix := "." + root.AvdName(i) + "-crash-copy-"
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(m.Root.LogsDir(), entry.Name())
		info, err := snapshotLog(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if busy, gone := fileInUse(path); busy || gone {
			continue
		}
		if removeLog(logFile{path: path, name: entry.Name(), size: info.Size(), mtime: info.ModTime(), info: info}, m.Log) == unlinkFailed {
			return fmt.Errorf("未完成的转储副本 %s 无法清理", entry.Name())
		}
	}
	return nil
}

func diagnosticCrashSources(since, through time.Time) ([]crashSource, error) {
	var sources []crashSource
	seen := map[string]bool{}
	var bases []string
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		bases = append(bases, filepath.Join(local, "Temp", "AndroidEmulator"))
	}
	if temp := os.TempDir(); temp != "" {
		bases = append(bases, filepath.Join(temp, "AndroidEmulator"))
	}
	for _, base := range bases {
		base, _ = filepath.Abs(base)
		key := strings.ToLower(filepath.Clean(base))
		if seen[key] {
			continue
		}
		seen[key] = true
		baseInfo, err := snapshotLog(base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !baseInfo.IsDir() || baseInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		entries, err := os.ReadDir(base)
		if err != nil {
			return nil, err
		}
		for _, db := range entries {
			if !db.IsDir() || !diagnosticCrashDB.MatchString(db.Name()) || db.Type()&os.ModeSymlink != 0 {
				continue
			}
			dbPath := filepath.Join(base, db.Name())
			parents := map[string]fs.FileInfo{base: baseInfo}
			err = filepath.WalkDir(dbPath, func(path string, entry fs.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				info, err := snapshotLog(path)
				if err != nil {
					return err
				}
				if info.Mode()&os.ModeSymlink != 0 {
					if entry.IsDir() {
						return filepath.SkipDir
					}
					return nil
				}
				if info.IsDir() {
					parents[path] = info
					return nil
				}
				if !info.Mode().IsRegular() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".dmp") || info.ModTime().Before(since) || info.ModTime().After(through) {
					return nil
				}
				candidate := crashSource{path: path, db: db.Name(), info: info}
				for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
					parentInfo, ok := parents[parent]
					if !ok {
						return fmt.Errorf("转储父目录身份未知: %s", parent)
					}
					candidate.parents = append(candidate.parents, crashParent{path: parent, info: parentInfo})
					if strings.EqualFold(parent, base) {
						break
					}
				}
				sources = append(sources, candidate)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(sources, func(a, b int) bool { return sources[a].info.ModTime().Before(sources[b].info.ModTime()) })
	return sources, nil
}

func crashParentsUnchanged(source crashSource) bool {
	for _, parent := range source.parents {
		current, err := snapshotLog(parent.path)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(parent.info, current) {
			return false
		}
	}
	return true
}

type crashContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r crashContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}

var copyDiagnosticDump = func(ctx context.Context, output io.Writer, input io.Reader, size int64) (int64, error) {
	return io.Copy(output, io.LimitReader(crashContextReader{ctx: ctx, reader: input}, size))
}

func (m *Manager) copyAndPublishCrash(ctx context.Context, i int, expected diagnostics.State, source crashSource) error {
	if !crashParentsUnchanged(source) {
		return fmt.Errorf("转储目录身份变化，未复制")
	}
	input, err := root.OpenShared(source.path)
	if err != nil {
		return err
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(source.info, opened) || opened.Size() != source.info.Size() || !opened.ModTime().Equal(source.info.ModTime()) {
		return fmt.Errorf("转储身份变化，未复制")
	}
	if err := os.MkdirAll(m.Root.LogsDir(), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.Root.LogsDir(), "."+root.AvdName(i)+"-crash-copy-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	n, copyErr := copyDiagnosticDump(ctx, io.MultiWriter(tmp, hash), input, opened.Size())
	closeErr := tmp.Close()
	current, statErr := snapshotLog(source.path)
	finished, handleErr := input.Stat()
	if err := errors.Join(copyErr, closeErr, statErr, handleErr, ctx.Err()); err != nil {
		return err
	}
	if n != opened.Size() || !current.Mode().IsRegular() || !os.SameFile(opened, current) || !os.SameFile(opened, finished) ||
		current.Size() != opened.Size() || finished.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) ||
		!finished.ModTime().Equal(opened.ModTime()) || !crashParentsUnchanged(source) {
		return fmt.Errorf("复制时转储变化，未发布")
	}
	config, err := m.Root.LockInstanceConfig(i)
	if err != nil {
		return err
	}
	defer config.Unlock()
	retention, err := m.retentionLock()
	if err != nil {
		return err
	}
	defer retention.Unlock()
	states, err := diagnostics.Read(m.Root)
	if err != nil {
		return err
	}
	if states[i] != expected || ctx.Err() != nil {
		return nil
	}
	if _, err := m.instance(i); err != nil {
		return err
	}
	dir := filepath.Join(m.Root.LogsDir(), root.AvdName(i)+"-crashdumps")
	if info, err := snapshotLog(dir); err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		return fmt.Errorf("诊断转储目录不是常规目录")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dirInfo, err := snapshotLog(dir)
	if err != nil {
		return err
	}
	guard, err := holdDiagnosticDirectory(dir, dirInfo)
	if err != nil {
		return err
	}
	defer guard.Close()
	dbHash := sha256.Sum256([]byte(source.db))
	name := hex.EncodeToString(dbHash[:4]) + "-" + hex.EncodeToString(hash.Sum(nil)[:12]) + "-" + source.info.Name()
	if err := os.Chtimes(tmp.Name(), opened.ModTime(), opened.ModTime()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	guard.Close()
	plan, err := m.diagnosticCleanup(i, false)
	if err != nil {
		return err
	}
	_, _, err = applyCleanup(plan, m.Log)
	return err
}
