//go:build !windows

package ui

import (
	"fmt"
	"modavd/internal/errs"
	"modavd/internal/logx"
	"os"
	"path/filepath"
	"time"
)

type Window struct {
	Edge    bool
	Started time.Time
	Done    <-chan struct{}
}

func LaunchWindow(string) (*Window, error) {
	return nil, errs.New(errs.Unsupported, "当前平台暂不支持窗口操作")
}
func AppDataDir() string {
	base, _ := os.UserCacheDir()
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "ModAVD")
}
func EdgeProfileDir() string                    { return filepath.Join(AppDataDir(), "ui-edge2") }
func removeOldEdgeProfile(string, *logx.Logger) {}
func FindEdge() string                          { return "" }
func shellOpen(string) error                    { return fmt.Errorf("当前平台暂不支持打开浏览器") }
