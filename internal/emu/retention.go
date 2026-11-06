// Copyright 2026 Ji Mao Cai
package emu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/logx"
	"modavd/internal/win"
)

type logKind uint8

const (
	emulatorLog logKind = iota
	guestLog
	bridgeLog
	failedBoot
	crashDump
)

var logOwner = regexp.MustCompile(`^(?:logcat-mas_|mas_|modavd-bridge-)([0-9]+)-`)
var crashDirName = regexp.MustCompile(`^mas_([0-9]+)-crashdumps$`)
var crashInternalName = regexp.MustCompile(`^\.mas_([0-9]+)-(?:crash-copy-.+|crashscan\.json)$`)
var pendingBootName = regexp.MustCompile(`^\.mas_([0-9]+)-[0-9]{8}-[0-9]{6}-bootfail-([0-9]+)-([0-9]+)-.+$`)

type retainedLog struct {
	logFile
	index int
	kind  logKind
}

// cleanupPlan is a snapshot of closed files. Preview and deletion use the same selection and byte accounting.
type cleanupPlan struct {
	files []logFile
	bytes int64
}

func classifyLog(f logFile) (retainedLog, bool) {
	m := logOwner.FindStringSubmatch(f.name)
	if m == nil {
		return retainedLog{}, false
	}
	i, err := strconv.Atoi(m[1])
	if err != nil {
		return retainedLog{}, false
	}
	kind := emulatorLog
	switch {
	case f.dir:
		kind = failedBoot
	case strings.HasPrefix(f.name, "logcat-"):
		kind = guestLog
	case strings.HasPrefix(f.name, "modavd-bridge-"):
		kind = bridgeLog
	}
	return retainedLog{logFile: f, index: i, kind: kind}, true
}

func closedLogs(files []logFile) []retainedLog {
	var closed []retainedLog
	for _, f := range files {
		if !f.dir {
			if busy, gone := fileInUse(f.path); busy || gone {
				continue
			}
		}
		if f, ok := classifyLog(f); ok {
			closed = append(closed, f)
		}
	}
	return closed
}

// planCleanup protects each diagnostic instance with its own quota. The normal root-wide quota never consumes it.
func planCleanup(files []retainedLog, i, keep int, normalCap int64, states map[int]diagnostics.State, lowDisk bool, now time.Time) cleanupPlan {
	drop := map[string]logFile{}
	add := func(f retainedLog) { drop[f.path] = f.logFile }
	var byKind = map[logKind][]retainedLog{}
	for _, f := range files {
		on := states[f.index].Enabled && !lowDisk
		if f.kind == crashDump && !on {
			add(f)
			continue
		}
		if on && f.kind != bridgeLog && f.mtime.Before(now.Add(-diagnostics.MaxAge)) {
			add(f)
			continue
		}
		if f.index == i {
			byKind[f.kind] = append(byKind[f.kind], f)
		}
	}
	for kind, group := range byKind {
		on := states[i].Enabled && !lowDisk
		limit := keep
		if on {
			if kind != failedBoot {
				continue
			}
			limit = diagnostics.BootKeep
		} else if kind == failedBoot {
			limit = bootDiagKeep
		}
		sort.Slice(group, func(a, b int) bool {
			if !group[a].mtime.Equal(group[b].mtime) {
				return group[a].mtime.After(group[b].mtime)
			}
			return group[a].name > group[b].name
		})
		if len(group) > limit {
			for _, f := range group[limit:] {
				add(f)
			}
		}
	}
	// A negative key is the ordinary shared pool; diagnostic instance numbers are their own pools.
	pools := map[int][]retainedLog{}
	for _, f := range files {
		if _, gone := drop[f.path]; gone {
			continue
		}
		pool := -1
		if states[f.index].Enabled && !lowDisk {
			// Bridge install/update records are tiny and explicitly retained throughout diagnostics.
			if f.kind == bridgeLog {
				continue
			}
			pool = f.index
		}
		pools[pool] = append(pools[pool], f)
	}
	for pool, group := range pools {
		cap := normalCap
		if pool >= 0 {
			cap = diagnostics.MaxBytes
		}
		var total int64
		for _, f := range group {
			total += f.size
		}
		sort.Slice(group, func(a, b int) bool {
			if !group[a].mtime.Equal(group[b].mtime) {
				return group[a].mtime.Before(group[b].mtime)
			}
			return group[a].name < group[b].name
		})
		for _, f := range group {
			if total <= cap {
				break
			}
			add(f)
			total -= f.size
		}
	}
	plan := cleanupPlan{}
	for _, f := range drop {
		plan.files = append(plan.files, f)
		plan.bytes += f.size
	}
	sortLogFiles(plan.files, false)
	return plan
}

var removeRetainedLog = removeLog

func applyCleanup(plan cleanupPlan, log *logx.Logger) (removed int, freed int64, err error) {
	for _, f := range plan.files {
		switch removeRetainedLog(f, log) {
		case unlinkRemoved:
			removed++
			freed += f.size
		case unlinkFailed:
			err = errors.Join(err, fmt.Errorf("日志 %s 未能删除，可重试关闭诊断模式", f.name))
		}
	}
	return removed, freed, err
}

func (m *Manager) retentionLock() (*win.FileLock, error) {
	if err := os.MkdirAll(m.Root.LogsDir(), 0o755); err != nil {
		return nil, err
	}
	l, ok, err := win.Lock(filepath.Join(m.Root.LogsDir(), ".modavd-retention.lock"), 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("正在清理日志，稍后重试")
	}
	return l, nil
}

var diagnosticDiskFree = win.DiskFree

// diagnosticCleanup runs while retentionLock is held. Invalid metadata or an unknown disk budget deletes nothing.
func (m *Manager) diagnosticCleanup(i int, previewOff bool) (cleanupPlan, error) {
	states, err := diagnostics.Read(m.Root)
	if err != nil {
		return cleanupPlan{}, err
	}
	free, err := diagnosticDiskFree(m.Root.LogsDir())
	if err != nil {
		return cleanupPlan{}, err
	}
	low := free < diagnostics.MinFreeBytes
	if previewOff {
		s := states[i]
		s.Enabled = false
		states[i] = s
	}
	for index, s := range states {
		if previewOff {
			break
		}
		marker := filepath.Join(m.Root.LogsDir(), fmt.Sprintf(".mas_%d-diagnostics-lowdisk", index))
		if s.Enabled && low {
			if f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
				f.Close()
				m.Log.Warnf("实例 %d 所在盘剩余不足 20 GiB，诊断日志暂按平常规则保留", index)
			}
		} else {
			os.Remove(marker)
		}
	}
	files, err := managedLogFiles(m.Root.LogsDir())
	if err != nil {
		return cleanupPlan{}, err
	}
	closed := closedLogs(files)
	entries, err := os.ReadDir(m.Root.LogsDir())
	if err != nil {
		return cleanupPlan{}, err
	}
	for _, entry := range entries {
		if match := pendingBootName.FindStringSubmatch(entry.Name()); match != nil && entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			index, _ := strconv.Atoi(match[1])
			pid, pidErr := strconv.ParseUint(match[2], 10, 32)
			created, createdErr := strconv.ParseUint(match[3], 10, 64)
			if pidErr != nil || createdErr != nil || pid == 0 || created == 0 {
				continue
			}
			if (win.Process{PID: uint32(pid), Created: created}).Alive() {
				continue
			}
			path := filepath.Join(m.Root.LogsDir(), entry.Name())
			info, err := snapshotLog(path)
			if err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
				closed = append(closed, retainedLog{logFile: logFile{path: path, name: entry.Name(), dir: true, size: dirSize(path), mtime: info.ModTime(), info: info}, index: index, kind: failedBoot})
			}
			continue
		}
		if match := crashInternalName.FindStringSubmatch(entry.Name()); match != nil && entry.Type().IsRegular() {
			index, _ := strconv.Atoi(match[1])
			if !states[index].Enabled {
				path := filepath.Join(m.Root.LogsDir(), entry.Name())
				info, err := snapshotLog(path)
				if err == nil && info.Mode().IsRegular() {
					if busy, gone := fileInUse(path); !busy && !gone {
						closed = append(closed, retainedLog{logFile: logFile{path: path, name: entry.Name(), size: info.Size(), mtime: info.ModTime(), info: info}, index: index, kind: crashDump})
					}
				}
			}
			continue
		}
		match := crashDirName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		index, _ := strconv.Atoi(match[1])
		dir := filepath.Join(m.Root.LogsDir(), entry.Name())
		info, err := snapshotLog(dir)
		if err != nil {
			m.Log.Warnf("诊断转储 %s 读不了，跳过这一项: %v", entry.Name(), err)
			continue
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			m.Log.Warnf("诊断转储 %s 不是常规目录，跳过这一项", entry.Name())
			continue
		}
		if !states[index].Enabled || low {
			closed = append(closed, retainedLog{logFile: logFile{path: dir, name: entry.Name(), dir: true, size: dirSize(dir), mtime: info.ModTime(), info: info}, index: index, kind: crashDump})
			continue
		}
		for _, f := range regularCrashFiles(dir) {
			closed = append(closed, retainedLog{logFile: f, index: index, kind: crashDump})
		}
	}
	return planCleanup(closed, i, logKeepPerInstance, logsDirMaxBytes, states, low, time.Now()), nil
}

func regularCrashFiles(dir string) []logFile {
	entries, _ := os.ReadDir(dir)
	var files []logFile
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := snapshotLog(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if busy, gone := fileInUse(p); !busy && !gone {
			files = append(files, logFile{path: p, name: e.Name(), size: info.Size(), mtime: info.ModTime(), info: info})
		}
	}
	return files
}

// Keep context in the caller-facing cleanup path; the bounded OS lock itself has no cancellation API.
func (m *Manager) cleanupDiagnostics(ctx context.Context, i int) (int, int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	l, err := m.retentionLock()
	if err != nil {
		return 0, 0, err
	}
	defer l.Unlock()
	plan, err := m.diagnosticCleanup(i, false)
	if err != nil {
		return 0, 0, err
	}
	removed, freed, err := applyCleanup(plan, m.Log)
	return removed, freed, err
}
