// Copyright 2026 Ji Mao Cai
package root

import (
	"fmt"
	"path/filepath"

	"modavd/internal/jsonord"
)

// RuntimeConfig is the immutable target of one existing boot's adb operations. It contains no instance state,
// diagnostic policy or credentials. Empty SDKRoot preserves the root-local SDK meaning of LocalSDKRoot.
type RuntimeConfig struct {
	SDKRoot             string `json:"sdk_root"`
	PortBase            int    `json:"port_base"`
	AdbServerPort       int    `json:"adb_server_port"`
	ScriptAdbServerPort int    `json:"script_adb_server_port"`
}

func (r *Root) runtimeMetadata() *jsonord.Obj {
	if r.runtime != nil {
		return r.runtime
	}
	return r.MetadataLoose().Obj()
}

// SnapshotRuntime resolves all routing keys from one strict read. A running helper retains its already-validated
// snapshot even if the live metadata becomes unreadable. Policy and all deletion reads still use ReadMetadata.
func (r *Root) SnapshotRuntime() (*Root, error) {
	if r.runtime != nil {
		return r, nil
	}
	v, err := r.ReadMetadata()
	if err != nil {
		return nil, err
	}
	o := jsonord.NewObject().Obj()
	for _, key := range []string{"adbServerPort", "scriptAdbServerPort"} {
		if v.Obj().Has(key) {
			if _, ok := metadataPort(v.Obj(), key); !ok {
				return nil, fmt.Errorf("无法确认运行目标 %s", key)
			}
		}
	}
	if sdk := v.Obj().Get("sdkRoot"); v.Obj().Has("sdkRoot") && sdk.Kind() != jsonord.Null {
		if _, ok := sdk.Str(); !ok {
			return nil, fmt.Errorf("无法确认运行 SDK 路径")
		}
	}
	for _, key := range []string{"sdkRoot", MetadataPortBase, "adbServerPort", "scriptAdbServerPort"} {
		if v.Obj().Has(key) {
			o.Set(key, v.Obj().Get(key))
		}
	}
	frozen := &Root{Dir: r.Dir, runtime: o}
	if err := frozen.CheckAdbPorts(); err != nil {
		return nil, err
	}
	return r.WithRuntime(RuntimeConfig{SDKRoot: frozen.LocalSDKRoot(), PortBase: frozen.Ports().Base,
		AdbServerPort: frozen.AdbServerPort(), ScriptAdbServerPort: frozen.ScriptAdbServerPort()})
}

// WithRuntime validates a transported snapshot with the same isolation rules as ordinary private adb use.
// It deliberately permits legacy port plans for existing instances; CheckPorts still guards new launches.
func (r *Root) WithRuntime(c RuntimeConfig) (*Root, error) {
	if c.SDKRoot != "" && !filepath.IsAbs(c.SDKRoot) {
		return nil, fmt.Errorf("运行 SDK 路径必须是绝对路径")
	}
	o := jsonord.NewObject().Obj()
	o.Set("sdkRoot", jsonord.NewString(c.SDKRoot))
	o.Set(MetadataPortBase, jsonord.NewInt(int64(c.PortBase)))
	o.Set("adbServerPort", jsonord.NewInt(int64(c.AdbServerPort)))
	o.Set("scriptAdbServerPort", jsonord.NewInt(int64(c.ScriptAdbServerPort)))
	for _, key := range []string{"adbServerPort", "scriptAdbServerPort"} {
		if _, ok := metadataPort(o, key); !ok {
			return nil, fmt.Errorf("运行快照的 %s 不合规", key)
		}
	}
	if c.AdbServerPort == c.ScriptAdbServerPort {
		return nil, fmt.Errorf("运行快照的两个 adb server 端口不能相同")
	}
	frozen := &Root{Dir: r.Dir, runtime: o}
	if err := frozen.CheckAdbPorts(); err != nil {
		return nil, err
	}
	return frozen, nil
}

func (r *Root) RuntimeConfig() (RuntimeConfig, error) {
	if r.runtime == nil {
		return RuntimeConfig{}, fmt.Errorf("尚未固定运行目标")
	}
	return RuntimeConfig{SDKRoot: r.LocalSDKRoot(), PortBase: r.Ports().Base, AdbServerPort: r.AdbServerPort(), ScriptAdbServerPort: r.ScriptAdbServerPort()}, nil
}
