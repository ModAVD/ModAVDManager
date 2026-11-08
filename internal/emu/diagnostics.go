// Copyright 2026 Ji Mao Cai
package emu

import (
	"context"
	"fmt"
	"sync"
	"time"

	"modavd/internal/diagnostics"
	"modavd/internal/errs"
)

// Runtime capture must continue in ordinary mode when metadata is temporarily unreadable. Deletion uses the
// strict diagnostics.Read directly. Warn once per failure episode, shared by this manager's derived loggers.
type operationalDiagnostics struct {
	mu     sync.Mutex
	failed bool
}

var readOperationalDiagnostics = diagnostics.Read

func (m *Manager) diagnosticPolicy(i int) diagnostics.State {
	s, _ := m.readDiagnosticPolicy(i)
	return s
}

// readDiagnosticPolicy is diagnosticPolicy that also says whether the metadata could be read: the zero (off) State it
// falls back to is a fact for capture, not for someone waiting for a switch to finish.
func (m *Manager) readDiagnosticPolicy(i int) (diagnostics.State, error) {
	states, err := readOperationalDiagnostics(m.Root)
	r := m.diagnosticReader
	if r == nil {
		r = &operationalDiagnostics{}
	} // narrow unit fixtures may construct Manager without New
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		if !r.failed {
			m.Log.Warnf("诊断策略读不了，日志采集和客体缓冲区暂按关闭处理，历史日志不删除: %v", err)
		}
		r.failed = true
		return diagnostics.State{}, err
	}
	r.failed = false
	return states[i], nil
}

// PreviewDiagnostics reports the same closed-file plan off will apply, without changing settings or deleting files.
func (m *Manager) PreviewDiagnostics(ctx context.Context, i int) (*DiagnosticsPreview, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l, err := m.retentionLock()
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	plan, err := m.diagnosticCleanup(i, true)
	if err != nil {
		return nil, err
	}
	return &DiagnosticsPreview{Index: i, DiagnosticsCleanup: DiagnosticsCleanup{Files: len(plan.files), Bytes: plan.bytes}}, nil
}

// SetDiagnostics persists desired state first. Repeating off always retries cleanup and guest restoration.
func (m *Manager) SetDiagnostics(ctx context.Context, i int, enabled bool) (*DiagnosticsCleanup, error) {
	if err := requireWindows(); err != nil {
		return nil, err
	}
	cleanup, err := m.persistDiagnostics(ctx, i, enabled)
	if err != nil {
		return cleanup, err
	}
	if !enabled {
		if err := m.waitCrashArchiveStopped(ctx, i); err != nil {
			return cleanup, errs.Wrap(errs.Internal, err, "诊断模式已关闭，但后台转储复制尚未结束").WithAdvice("重试关闭诊断模式，完成剩余清理")
		}
		files, bytes, err := m.cleanupDiagnostics(ctx, i)
		cleanup.Files += files
		cleanup.Bytes += bytes
		if err != nil {
			return cleanup, err
		}
	}
	running, err := m.applyDiagnosticGuest(ctx, i)
	if err != nil {
		return cleanup, errs.Wrap(errs.Internal, err, "诊断设置已保存，但客体日志设置没有做完").
			WithAdvice("重试相同的诊断开关命令；实例关着时，下次开机也会恢复这些设置")
	}
	if running {
		if err := m.waitDiagnosticCapture(ctx, i, 10*time.Second); err != nil {
			return cleanup, err
		}
	}
	return cleanup, nil
}

func (m *Manager) persistDiagnostics(ctx context.Context, i int, enabled bool) (*DiagnosticsCleanup, error) {
	l, err := m.Root.LockInstanceConfig(i)
	if err != nil {
		return nil, err
	}
	defer l.Unlock()
	if _, err := m.instance(i); err != nil {
		return nil, err
	}
	if _, err := diagnostics.Read(m.Root); err != nil {
		return nil, err
	}
	retention, err := m.retentionLock()
	if err != nil {
		return nil, err
	}
	err = diagnostics.Set(m.Root, i, enabled, time.Now())
	retention.Unlock()
	if err != nil {
		return nil, err
	}
	result := &DiagnosticsCleanup{}
	if enabled {
		m.Log.Infof("实例 %d 诊断模式已打开：最多多用约 4 GiB 磁盘和 48 MiB 客体内存；日志可能含账号信息，只存本机", i)
		return result, nil
	}
	result.Files, result.Bytes, err = m.cleanupDiagnostics(ctx, i)
	m.Log.Infof("实例 %d 诊断模式已关闭，清理 %d 份日志，释放 %d 字节", i, result.Files, result.Bytes)
	if err != nil {
		return result, errs.Wrap(errs.Internal, err, "诊断模式已关闭，但日志尚未清理完成").WithAdvice("重试关闭诊断模式，继续完成清理")
	}
	return result, nil
}

// applyDiagnosticGuest is shared by set, after-boot and re-registration. The config lock and fresh read prevent
// an older on request from applying after a later off. No metadata or retention lock is held across an adb call.
func (m *Manager) applyDiagnosticGuest(ctx context.Context, i int) (bool, error) {
	r, err := m.Root.SnapshotRuntime()
	if err != nil {
		m.Log.Warnf("实例 %d 无法确认 SDK/adb 目标，本次不改客体缓冲区: %v", i, err)
		return false, nil
	}
	return m.withRuntime(r).applyDiagnosticGuestRuntime(ctx, i)
}

func (m *Manager) applyDiagnosticGuestRuntime(ctx context.Context, i int) (bool, error) {
	l, err := m.Root.LockInstanceConfig(i)
	if err != nil {
		return false, err
	}
	defer l.Unlock()
	if _, err := m.instance(i); err != nil {
		return false, err
	}
	policy := m.diagnosticPolicy(i)
	_, running, err := m.qemuOf(i)
	if err != nil || !running {
		return running, err
	}
	res := m.su(ctx, i, diagnosticBufferScript(policy.Enabled), 10*time.Second)
	if res.Code != 0 {
		return true, fmt.Errorf("调整客体日志缓冲区失败，退出码 %d: %s", res.Code, tail(res.Out, 400))
	}
	return true, nil
}

func diagnosticBufferScript(enabled bool) string {
	script := `set -e; setprop persist.logd.size 16M; ` +
		`for b in main system kernel; do setprop persist.logd.size.$b ""; done; ` +
		`logcat -b all -G 16M`
	if enabled {
		script += `; for b in main system kernel; do setprop persist.logd.size.$b 32M; logcat -b "$b" -G 32M; done`
	}
	return script
}
