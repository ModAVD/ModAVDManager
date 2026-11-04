//go:build windows

package emu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"modavd/internal/errs"
)

func TestQemuUnsupportedReachesOperations(t *testing.T) {
	ctx := context.Background()
	for name, call := range map[string]func(*Manager) error{
		"show":    func(m *Manager) error { _, err := m.SetWindowVisible(ctx, 1, true); return err },
		"hide":    func(m *Manager) error { _, err := m.SetWindowVisible(ctx, 1, false); return err },
		"info":    func(m *Manager) error { _, err := m.Info(ctx, 1); return err },
		"running": func(m *Manager) error { _, err := m.Running(ctx); return err },
		"list":    func(m *Manager) error { _, err := m.List(ctx); return err },
		"stop":    func(m *Manager) error { _, err := m.Stop(ctx, 1, StopOptions{}); return err },
		"start":   func(m *Manager) error { _, err := m.Start(ctx, 1, StartOptions{NoBridge: true}); return err },
		"bridge":  func(m *Manager) error { _, err := m.Bridge(ctx, 1, "verify"); return err },
		"shell":   func(m *Manager) error { _, err := m.Shell(ctx, 1, "id", time.Second); return err },
		"update": func(m *Manager) error {
			title := "test"
			_, err := m.Update(ctx, 1, UpdateRequest{Title: &title})
			return err
		},
		"delete": func(m *Manager) error { _, err := m.Delete(ctx, 1); return err },
		"install": func(m *Manager) error {
			apk := filepath.Join(m.Root.Dir, "test.apk")
			if err := os.WriteFile(apk, []byte("PK"), 0o644); err != nil {
				return err
			}
			_, err := m.InstallAPK(ctx, 1, apk)
			return err
		},
		"resume": func(m *Manager) error {
			if err := m.setCreation(1, creationFailed, "test interrupted", ""); err != nil {
				return err
			}
			i := 1
			_, err := m.resumeCreate(ctx, CreateRequest{Index: &i, Resume: true})
			return err
		},
		"afterBoot": func(m *Manager) error {
			in, err := m.instance(1)
			if err != nil {
				return err
			}
			_, err = m.afterBoot(ctx, in, setupLaunched, time.Time{}, "", nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := New(managerTestRoot(t, 1), nil)
			failure := errs.New(errs.Unsupported, "test snapshot unsupported")
			calls := injectQemuSnapshotError(t, failure)
			if err := call(m); !errors.Is(err, failure) {
				t.Fatalf("lost snapshot unsupported: %v", err)
			}
			if *calls == 0 {
				t.Fatal("operation never reached snapshot API")
			}
		})
	}
}
