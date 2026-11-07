//go:build darwin && arm64

package ui

import (
	"path/filepath"
	"time"

	"modavd/internal/host"
	"modavd/internal/logx"
)

type Window struct {
	// Edge is the historical name of the owned app-window flag; macOS uses Chrome.
	Edge    bool
	Started time.Time
	Done    <-chan struct{}
}

func LaunchWindow(url string) (*Window, error) { return launchChromeWindow(url, false) }

func launchChromeWindow(url string, chrome bool) (*Window, error) {
	w, err := host.LaunchManagerWindow(url, chrome)
	if err != nil {
		return nil, err
	}
	return &Window{Edge: w.App, Started: w.Started, Done: w.Done}, nil
}

func AppDataDir() string { return host.ManagerDataDir() }

// Retained for common cleanup routing; no Edge profile is opened or removed on macOS.
func EdgeProfileDir() string                    { return filepath.Join(AppDataDir(), "ui-edge2") }
func removeOldEdgeProfile(string, *logx.Logger) {}
func FindEdge() string                          { return "" }
func shellOpen(target string) error             { return host.OpenDesktop(target) }
