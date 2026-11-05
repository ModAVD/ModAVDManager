//go:build !windows

// Copyright 2026 Ji Mao Cai
package emu

import (
	"io/fs"
	"modavd/internal/logx"
	"os"
)

// Instance mutation is unsupported off Windows. Do not fall back to a weaker path-based delete.
func unlinkRetained(logFile, *logx.Logger) unlinkResult { return unlinkFailed }

func snapshotLog(path string) (fs.FileInfo, error) { return os.Lstat(path) }

func holdDiagnosticDirectory(string, fs.FileInfo) (*os.File, error) { return nil, os.ErrPermission }
