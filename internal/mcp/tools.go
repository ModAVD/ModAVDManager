//go:build windows

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"modavd/internal/avd"
	"modavd/internal/emu"
	"modavd/internal/errs"
	"modavd/internal/root"
)

// toolDef is one tool: the wire definition (exported fields, as tools/list sends it) plus how to run it.
type toolDef struct {
	Name         string         `json:"name"`
	Title        string         `json:"title,omitempty"`
	Description  string         `json:"description"`
	InputSchema  map[string]any `json:"inputSchema"`
	OutputSchema map[string]any `json:"outputSchema,omitempty"`
	Annotations  *annotations   `json:"annotations,omitempty"`
	Meta         map[string]any `json:"_meta,omitempty"`

	props    []string // accepted argument names, in schema order
	required []string
	run      func(ctx context.Context, b backend, a args, progress func(emu.Progress)) (any, error)
	// keep: the tool writes files and does not stop half way on cancellation; at shutdown it is given time to finish
	// instead of being canceled (see server.shutdown).
	keep bool
}

// finishing marks t as keep.
func finishing(t *toolDef) *toolDef {
	t.keep = true
	return t
}

// annotations are hints for the client (Claude Code runs readOnly calls in parallel, asks before destructive ones).
type annotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

func readOnly() *annotations {
	return &annotations{ReadOnlyHint: ptr(true), OpenWorldHint: ptr(false)}
}

func mutating(destructive, idempotent bool) *annotations {
	return &annotations{ReadOnlyHint: ptr(false), DestructiveHint: ptr(destructive), IdempotentHint: ptr(idempotent),
		OpenWorldHint: ptr(false)}
}

func ptr[T any](v T) *T { return &v }

// prop is one argument of a tool.
type prop struct {
	name     string
	required bool
	schema   map[string]any
}

const maxTimeoutS = 3600

func indexProp(required bool) prop {
	// The hole at ReservedIndex is left to the description and the manager's check: plain keywords only, so every
	// client's schema converter takes it.
	return prop{"index", required, map[string]any{
		"type": "integer", "minimum": 0, "maximum": root.MaxIndex,
		"description": fmt.Sprintf("Instance index, 0-%d except %d (the instance %s<index>).", root.MaxIndex,
			root.ReservedIndex, root.AvdNamePrefix),
	}}
}

func memoryProp(desc string) prop {
	return prop{"memory_mb", false, map[string]any{"type": "integer", "enum": avd.MemoryChoicesMB, "description": desc}}
}

func cpuProp(desc string) prop {
	return prop{"cpu", false, map[string]any{"type": "integer", "enum": avd.CPUChoices, "description": desc}}
}

func fpsProp(desc string) prop {
	return prop{"fps", false, map[string]any{"type": "integer", "enum": avd.FPSChoices, "description": desc}}
}

func timeoutProp(def time.Duration, desc string) prop {
	return prop{"timeout_s", false, map[string]any{"type": "integer", "minimum": 1, "maximum": maxTimeoutS,
		"default": int(def / time.Second), "description": desc}}
}

func newTool(name, title, desc string, ann *annotations, out reflect.Type, props []prop,
	run func(ctx context.Context, b backend, a args, progress func(emu.Progress)) (any, error)) *toolDef {
	properties := map[string]any{}
	t := &toolDef{Name: name, Title: title, Description: desc, Annotations: ann, run: run}
	for _, p := range props {
		properties[p.name] = p.schema
		t.props = append(t.props, p.name)
		if p.required {
			t.required = append(t.required, p.name)
		}
	}
	t.InputSchema = map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(t.required) > 0 {
		t.InputSchema["required"] = t.required
	}
	if out != nil {
		t.OutputSchema = schemaOf(out)
	}
	return t
}

// toolDefs is the fixed tool list, in the order tools/list returns it. The root is never an argument: it is fixed
// by the server's --root, so the model cannot point a tool at another root (the production one).
func toolDefs() []*toolDef {
	return []*toolDef{
		newTool("list_instances", "List instances",
			"List every ModAVD instance under the server's root (fixed by --root) with its status (offline / "+
				"starting / online), adb serial and address, ports and the options of its next boot.",
			readOnly(), reflect.TypeFor[emu.ListResult](), nil,
			func(ctx context.Context, b backend, a args, progress func(emu.Progress)) (any, error) {
				return b.List(ctx)
			}),

		newTool("get_instance", "Get instance",
			"Get one instance by index: status, adb serial and address, ports, screenshot shared memory and the "+
				"options of its next boot.",
			readOnly(), reflect.TypeFor[emu.InstanceInfo](), []prop{indexProp(true)},
			func(ctx context.Context, b backend, a args, progress func(emu.Progress)) (any, error) {
				i, err := a.reqInt("index")
				if err != nil {
					return nil, err
				}
				return b.Info(ctx, i)
			}),

		newTool("check_host", "Check host",
			"Run the host checks a boot depends on: root path, components, emulator build, CPU acceleration, "+
				"Vulkan driver, free disk and free memory. ready=false means a blocking check failed; each item "+
				"says why and what to do. Can take up to a minute.",
			readOnly(), reflect.TypeFor[emu.CheckResult](), []prop{
				{"memory_mb", false, map[string]any{"type": "integer", "minimum": 1, "default": avd.DefaultMemoryMB,
					"description": "Instance memory in MB to judge free host memory against."}},
				{"refresh", false, map[string]any{"type": "boolean", "default": false,
					"description": "Run the acceleration check and the Vulkan probe again instead of using results " +
						"cached by this server (acceleration 2 min, Vulkan 10 min)."}},
			},
			func(ctx context.Context, b backend, a args, _ func(emu.Progress)) (any, error) {
				var opt emu.CheckOptions
				mem, err := a.optInt("memory_mb")
				if err != nil {
					return nil, err
				}
				refresh, err := a.optBool("refresh")
				if err != nil {
					return nil, err
				}
				opt.Refresh = refresh != nil && *refresh
				if mem != nil {
					if *mem <= 0 {
						return nil, errs.New(errs.InvalidArgument, "memory_mb 必须是正整数")
					}
					opt.MemoryMB = *mem
				}
				return b.Check(ctx, opt)
			}),

		finishing(newTool("create_instance", "Create instance",
			fmt.Sprintf("Create a new, stopped instance. Omitted options take the defaults (%d MB memory, %d CPU "+
				"cores, %d GB data partition, %d fps); index defaults to the smallest free one. Memory, CPU and fps "+
				"only accept the listed steps; the data partition can later only be made larger (update_instance).",
				avd.DefaultMemoryMB, avd.DefaultCPU, avd.DefaultDataPartitionGB, avd.DefaultFPS),
			mutating(false, false), reflect.TypeFor[emu.InstanceInfo](), []prop{
				{"title", false, map[string]any{"type": "string",
					"description": "Display name; default: the instance name (" + root.AvdNamePrefix + "<index>)."}},
				indexProp(false),
				memoryProp("Guest memory in MB."),
				cpuProp("CPU cores."),
				{"data_partition_gb", false, map[string]any{"type": "integer", "minimum": avd.MinDataPartitionGB,
					"maximum": avd.MaxDataPartitionGB, "description": "Size of the data partition (games) in GB."}},
				fpsProp("Display refresh rate."),
			},
			func(ctx context.Context, b backend, a args, progress func(emu.Progress)) (any, error) {
				var req emu.CreateRequest
				title, err := a.optString("title")
				if err != nil {
					return nil, err
				}
				if title != nil {
					req.Title = *title
				}
				if req.Index, err = a.optInt("index"); err != nil {
					return nil, err
				}
				if req.MemoryMB, err = a.optInt("memory_mb"); err != nil {
					return nil, err
				}
				if req.CPU, err = a.optInt("cpu"); err != nil {
					return nil, err
				}
				if req.DataPartitionGB, err = a.optInt("data_partition_gb"); err != nil {
					return nil, err
				}
				if req.FPS, err = a.optInt("fps"); err != nil {
					return nil, err
				}
				req.Progress = progress
				return b.Create(ctx, req)
			})),

		finishing(newTool("update_instance", "Update instance",
			"Change the title, memory, CPU cores, frame rate or data partition size of an instance. The changes take "+
				"effect at its next boot (applies_next_boot=true when it is running now).",
			mutating(true, true), reflect.TypeFor[emu.UpdateResult](), []prop{
				indexProp(true),
				{"title", false, map[string]any{"type": "string", "description": "New display name."}},
				memoryProp("New guest memory in MB."),
				cpuProp("New CPU core count."),
				fpsProp("New display refresh rate."),
				{"data_partition_gb", false, map[string]any{"type": "integer", "minimum": avd.MinDataPartitionGB,
					"maximum": avd.MaxDataPartitionGB, "description": "New data partition size in GB; can only be " +
						"raised. Game data is kept: the emulator grows the disk at the next boot and the file " +
						"system is grown online after it."}},
			},
			func(ctx context.Context, b backend, a args, _ func(emu.Progress)) (any, error) {
				i, err := a.reqInt("index")
				if err != nil {
					return nil, err
				}
				var req emu.UpdateRequest
				if req.Title, err = a.optString("title"); err != nil {
					return nil, err
				}
				if req.MemoryMB, err = a.optInt("memory_mb"); err != nil {
					return nil, err
				}
				if req.CPU, err = a.optInt("cpu"); err != nil {
					return nil, err
				}
				if req.FPS, err = a.optInt("fps"); err != nil {
					return nil, err
				}
				if req.DataPartitionGB, err = a.optInt("data_partition_gb"); err != nil {
					return nil, err
				}
				if req.Title == nil && req.MemoryMB == nil && req.CPU == nil && req.FPS == nil && req.DataPartitionGB == nil {
					return nil, errs.New(errs.Usage, "没有要改的选项：title、memory_mb、cpu、fps、data_partition_gb 至少给一项")
				}
				return b.Update(ctx, i, req)
			})),

		func() *toolDef {
			t := finishing(newTool("delete_instance", "Delete instance",
				"Permanently delete a stopped instance together with its data disk: every installed game and all "+
					"game data in it are lost and cannot be recovered. confirm must be true. Fails while the "+
					"instance is running, starting or stopping (stop it with stop_instance first).",
				mutating(true, true), reflect.TypeFor[emu.DeleteResult](), []prop{
					indexProp(true),
					{"confirm", true, map[string]any{"type": "boolean", "const": true,
						"description": "Must be true: confirms that the instance and its game data are deleted."}},
				},
				func(ctx context.Context, b backend, a args, _ func(emu.Progress)) (any, error) {
					i, err := a.reqInt("index")
					if err != nil {
						return nil, err
					}
					confirm, err := a.optBool("confirm")
					if err != nil {
						return nil, err
					}
					if confirm == nil || !*confirm {
						return nil, errs.New(errs.Usage, "删除实例会把里面的游戏和数据一起删掉，无法恢复").
							WithAdvice("确认要删除时把 confirm 设为 true")
					}
					return b.Delete(ctx, i)
				}))
			// Claude Code asks the user before every call of a tool marked like this.
			t.Meta = map[string]any{"anthropic/requiresUserInteraction": true}
			return t
		}(),

		newTool("start_instance", "Start instance",
			fmt.Sprintf("Boot an instance (headless unless window=true) and wait until Android has booted and the "+
				"per-boot setup is done; usually 25-80 s, at most timeout_s (default %d) for Android to boot. An "+
				"instance that is already running is adopted. Sends progress notifications when the request has a "+
				"progressToken. Cancelling the call or reaching timeout_s only stops waiting: the emulator is not "+
				"killed and keeps booting. Calling start_instance again adopts it and finishes the setup of that "+
				"boot, one-time steps included (first-boot setup, guest Vulkan fix), before the deadline the "+
				"error's advice names (launch + max(timeout_s, %d s) + %d min); after it the guest Vulkan fix "+
				"waits for the next boot. Mounting it restarts zygote, ending every app, so that start mounts it "+
				"only while no app is in the foreground and no install_apk runs; otherwise it warns and sets "+
				"setup_pending=true: close the app or let the install finish and call it again before the "+
				"deadline, or the next boot mounts it.",
				int(emu.DefaultBootTimeout/time.Second), int(emu.DefaultBootTimeout/time.Second),
				int(emu.SetupMarkerGrace/time.Minute)),
			mutating(false, true), reflect.TypeFor[emu.StartResult](), []prop{
				indexProp(true),
				{"window", false, map[string]any{"type": "boolean", "default": false,
					"description": "Boot with the emulator window instead of headless."}},
				timeoutProp(emu.DefaultBootTimeout, "Seconds to wait for Android to finish booting; when they run "+
					"out the call fails with a timeout but the emulator keeps booting."),
			},
			func(ctx context.Context, b backend, a args, progress func(emu.Progress)) (any, error) {
				i, err := a.reqInt("index")
				if err != nil {
					return nil, err
				}
				opt := emu.StartOptions{Progress: progress}
				window, err := a.optBool("window")
				if err != nil {
					return nil, err
				}
				opt.Window = window != nil && *window
				if opt.Timeout, err = a.timeout(); err != nil {
					return nil, err
				}
				return b.Start(ctx, i, opt)
			}),

		newTool("stop_instance", "Stop instance",
			"Shut an instance down safely: Android shutdown, falling back to sync and console kill; force=true "+
				"skips the Android shutdown but still syncs first. Always use this instead of killing emulator or "+
				"qemu processes: a kill without sync loses recently written game data. Stopping an offline "+
				"instance is not an error. Takes up to about 30 s.",
			mutating(true, true), reflect.TypeFor[emu.StopResult](), []prop{
				indexProp(true),
				{"force", false, map[string]any{"type": "boolean", "default": false,
					"description": "Skip the Android shutdown: sync the guest disk, then kill the emulator."}},
			},
			func(ctx context.Context, b backend, a args, _ func(emu.Progress)) (any, error) {
				i, err := a.reqInt("index")
				if err != nil {
					return nil, err
				}
				force, err := a.optBool("force")
				if err != nil {
					return nil, err
				}
				return b.Stop(ctx, i, emu.StopOptions{Force: force != nil && *force})
			}),

		newTool("set_window_visible", "Show or hide window",
			"Show or hide the emulator window of an instance that was started with window=true. A headless "+
				"instance has no window (windows=0 in the result).",
			mutating(false, true), reflect.TypeFor[emu.WindowResult](), []prop{
				indexProp(true),
				{"visible", true, map[string]any{"type": "boolean", "description": "true shows, false hides."}},
			},
			func(ctx context.Context, b backend, a args, _ func(emu.Progress)) (any, error) {
				i, err := a.reqInt("index")
				if err != nil {
					return nil, err
				}
				visible, err := a.optBool("visible")
				if err != nil {
					return nil, err
				}
				if visible == nil {
					return nil, errs.New(errs.Usage, "缺少参数 visible")
				}
				return b.SetWindowVisible(ctx, i, *visible)
			}),

		newTool("shell", "Run guest shell command",
			"Run one shell command in the guest (adb shell) of a booted instance and return its exit code and "+
				"merged stdout / stderr. A non-zero exit code is a normal result, not a tool error. Do not power "+
				"off or reboot the guest with it; use stop_instance / start_instance.",
			mutating(true, false), reflect.TypeFor[emu.ShellResult](), []prop{
				indexProp(true),
				{"command", true, map[string]any{"type": "string", "minLength": 1,
					"description": "The shell command line, run by the guest's sh."}},
				timeoutProp(30*time.Second, "Seconds before the command is abandoned."),
			},
			func(ctx context.Context, b backend, a args, _ func(emu.Progress)) (any, error) {
				i, err := a.reqInt("index")
				if err != nil {
					return nil, err
				}
				command, err := a.optString("command")
				if err != nil {
					return nil, err
				}
				if command == nil || strings.TrimSpace(*command) == "" {
					return nil, errs.New(errs.InvalidArgument, "command 不能为空")
				}
				timeout, err := a.timeout()
				if err != nil {
					return nil, err
				}
				return b.Shell(ctx, i, *command, timeout)
			}),

		finishing(newTool("install_apk", "Install APK",
			"Install an APK on a booted instance with adb install -r (an installed app is replaced and keeps its "+
				"data). apk_path is an absolute path on the host; large game packages can take several minutes.",
			mutating(false, true), reflect.TypeFor[emu.InstallResult](), []prop{
				indexProp(true),
				{"apk_path", true, map[string]any{"type": "string", "minLength": 1,
					"description": `Absolute path of the .apk file on the host, e.g. D:\apk\game.apk.`}},
			},
			func(ctx context.Context, b backend, a args, _ func(emu.Progress)) (any, error) {
				i, err := a.reqInt("index")
				if err != nil {
					return nil, err
				}
				path, err := a.optString("apk_path")
				if err != nil {
					return nil, err
				}
				if path == nil || strings.TrimSpace(*path) == "" {
					return nil, errs.New(errs.InvalidArgument, "apk_path 不能为空")
				}
				// The server's working directory means nothing to the model, so relative paths are refused.
				if !filepath.IsAbs(*path) {
					return nil, errs.Newf(errs.InvalidArgument, "apk_path 必须是宿主机上的绝对路径: %s", *path)
				}
				return b.InstallAPK(ctx, i, *path)
			})),
	}
}

// ---- arguments ---------------------------------------------------------------------------------------------------

// args are the tools/call arguments. JSON null counts as absent.
type args map[string]json.RawMessage

// checkArgs: unknown names (additionalProperties false) and missing required ones are usage errors.
func (t *toolDef) checkArgs(a args) error {
	var unknown []string
	for k := range a {
		if !slices.Contains(t.props, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		if len(t.props) == 0 {
			return errs.Newf(errs.Usage, "%s 不接受参数，收到了: %s", t.Name, strings.Join(unknown, ", "))
		}
		return errs.Newf(errs.Usage, "不认识的参数: %s（%s 只接受 %s）", strings.Join(unknown, ", "), t.Name,
			strings.Join(t.props, ", "))
	}
	for _, k := range t.required {
		if v, ok := a[k]; !ok || isNull(v) {
			return errs.Newf(errs.Usage, "缺少参数 %s", k)
		}
	}
	return nil
}

func (a args) optInt(name string) (*int, error) {
	raw, ok := a[name]
	if !ok || isNull(raw) {
		return nil, nil
	}
	raw = bytes.TrimSpace(raw)
	if c := raw[0]; c != '-' && (c < '0' || c > '9') {
		return nil, errs.Newf(errs.Usage, "参数 %s 必须是整数", name)
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return nil, errs.Newf(errs.Usage, "参数 %s 必须是整数", name)
	}
	if i, err := n.Int64(); err == nil && i >= math.MinInt32 && i <= math.MaxInt32 {
		v := int(i)
		return &v, nil
	}
	// 4096.0 is still an integer in JSON Schema terms.
	f, err := n.Float64()
	if err != nil || f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return nil, errs.Newf(errs.Usage, "参数 %s 必须是整数", name)
	}
	v := int(f)
	return &v, nil
}

func (a args) reqInt(name string) (int, error) {
	v, err := a.optInt(name)
	if err != nil {
		return 0, err
	}
	if v == nil {
		return 0, errs.Newf(errs.Usage, "缺少参数 %s", name)
	}
	return *v, nil
}

func (a args) optString(name string) (*string, error) {
	raw, ok := a[name]
	if !ok || isNull(raw) {
		return nil, nil
	}
	var s string
	if !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) || json.Unmarshal(raw, &s) != nil {
		return nil, errs.Newf(errs.Usage, "参数 %s 必须是字符串", name)
	}
	return &s, nil
}

func (a args) optBool(name string) (*bool, error) {
	raw, ok := a[name]
	if !ok || isNull(raw) {
		return nil, nil
	}
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return ptr(true), nil
	case "false":
		return ptr(false), nil
	}
	return nil, errs.Newf(errs.Usage, "参数 %s 必须是 true 或 false", name)
}

// timeout is timeout_s as a duration; 0 (absent) lets the manager use its default.
func (a args) timeout() (time.Duration, error) {
	s, err := a.optInt("timeout_s")
	if err != nil || s == nil {
		return 0, err
	}
	if *s < 1 || *s > maxTimeoutS {
		return 0, errs.Newf(errs.InvalidArgument, "timeout_s 必须在 1 到 %d 秒之间", maxTimeoutS)
	}
	return time.Duration(*s) * time.Second, nil
}

// ---- output schemas ----------------------------------------------------------------------------------------------

// schemaOf derives the JSON Schema of what encoding/json writes for t, so the output schemas follow the emu result
// types as fields are added. Fields without omitempty are required; slices, maps and pointers may be null. No
// additionalProperties: new fields must not break clients that validate structuredContent.
func schemaOf(t reflect.Type) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		s := schemaOf(t.Elem())
		nullable(s)
		return s
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		structFields(t, props, &required)
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": []string{"array", "null"}, "items": schemaOf(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": []string{"object", "null"}}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	}
	return map[string]any{} // interface: anything
}

func structFields(t reflect.Type, props map[string]any, required *[]string) {
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			structFields(f.Type, props, required) // embedded fields are promoted, like encoding/json does
			continue
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		props[name] = schemaOf(f.Type)
		optional := false
		for _, o := range strings.Split(opts, ",") {
			if o == "omitempty" || o == "omitzero" {
				optional = true
			}
		}
		if !optional {
			*required = append(*required, name)
		}
	}
}

func nullable(s map[string]any) {
	switch typ := s["type"].(type) {
	case string:
		s["type"] = []string{typ, "null"}
	case []string:
		if !slices.Contains(typ, "null") {
			s["type"] = append(typ, "null")
		}
	}
}
