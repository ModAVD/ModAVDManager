// Copyright 2026 Ji Mao Cai
package emu

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"modavd/internal/root"
)

func (m *Manager) withRuntime(r *root.Root) *Manager {
	c := *m
	c.Root = r
	c.ADB = m.ADB.Derive(c.Log, c.logcatAfterReregister)
	c.ADB.Root = r
	c.ADB.Booting = func(serial string) bool {
		port, ok := consolePortOfSerial(serial)
		if !ok {
			return false
		}
		i, ok := r.Ports().IndexOfConsole(port)
		return ok && c.booting(i)
	}
	return &c
}

func captureRuntimeToken(r *root.Root) (string, error) {
	c, err := r.RuntimeConfig()
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func captureRuntimeFromToken(r *root.Root, token string) (*root.Root, error) {
	if len(token) > 16<<10 {
		return nil, fmt.Errorf("运行快照过长")
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var wire struct {
		SDKRoot             *string `json:"sdk_root"`
		PortBase            *int    `json:"port_base"`
		AdbServerPort       *int    `json:"adb_server_port"`
		ScriptAdbServerPort *int    `json:"script_adb_server_port"`
	}
	if err := d.Decode(&wire); err != nil {
		return nil, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("运行快照尾部不是空白")
	}
	if wire.SDKRoot == nil || wire.PortBase == nil || wire.AdbServerPort == nil || wire.ScriptAdbServerPort == nil {
		return nil, fmt.Errorf("运行快照缺少字段")
	}
	return r.WithRuntime(root.RuntimeConfig{SDKRoot: *wire.SDKRoot, PortBase: *wire.PortBase, AdbServerPort: *wire.AdbServerPort, ScriptAdbServerPort: *wire.ScriptAdbServerPort})
}
