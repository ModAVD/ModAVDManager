package avd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"modavd/internal/errs"
	"modavd/internal/root"
)

// ---- helpers -----------------------------------------------------------------------------------------------------

func newRoot(t *testing.T) *root.Root {
	t.Helper()
	r, err := root.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if errs.CodeOf(err) != code {
		t.Fatalf("want %s, got %v (%s)", code, err, errs.CodeOf(err))
	}
}

func ip(v int) *int { return &v }

// python is the Python 3 for the reference comparisons: $MODAVD_TEST_PYTHON, otherwise python on PATH; skipped
// without one.
func python(t *testing.T) string {
	t.Helper()
	py := os.Getenv("MODAVD_TEST_PYTHON")
	if py == "" {
		found, err := exec.LookPath("python")
		if err != nil {
			t.Skipf("no python on PATH (set MODAVD_TEST_PYTHON): %v", err)
		}
		py = found
	}
	if _, err := os.Stat(py); err != nil {
		t.Skipf("no Python at %s", py)
	}
	return py
}

// isModavdLock: modavd's own lock files, which AUTO-MAS does not have (mas-avd.json.lock, avd\.mas_<i>.config.lock).
func isModavdLock(rel string) bool {
	if rel == root.MetadataFile+".lock" {
		return true
	}
	dir, name := filepath.Split(rel)
	return dir == root.AvdDir+`\` && strings.HasPrefix(name, "."+root.AvdNamePrefix) && strings.HasSuffix(name, ".config.lock")
}

// snapshot is every file under dir (relative path -> bytes), without modavd's own lock files.
func snapshot(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	tree := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		if isModavdLock(rel) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = data
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func resetTree(t *testing.T, dir string, setup map[string]string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	for rel, data := range setup {
		writeFile(t, filepath.Join(dir, rel), []byte(data))
	}
}

func diffTrees(t *testing.T, label string, got, want map[string][]byte) {
	t.Helper()
	names := map[string]bool{}
	for k := range got {
		names[k] = true
	}
	for k := range want {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		g, gok := got[k]
		w, wok := want[k]
		switch {
		case !gok:
			t.Errorf("%s: %s missing (Python wrote %q)", label, k, w)
		case !wok:
			t.Errorf("%s: %s only in Go: %q", label, k, g)
		case !bytes.Equal(g, w):
			t.Errorf("%s: %s differs\n go %q\n py %q", label, k, g, w)
		}
	}
}

// ---- comparison with AUTO-MAS (testdata/instances_ref.py) --------------------------------------------------------

type refOp map[string]any

type refResult struct {
	Result json.RawMessage   `json:"result"`
	Error  *string           `json:"error"`
	Tree   map[string]string `json:"tree"`
}

func optInt(op refOp, key string) *int {
	v, ok := op[key]
	if !ok || v == nil {
		return nil
	}
	n := v.(int)
	return &n
}

// describe is what the reference's "list" reports for every instance.
func describe(r *root.Root, list []*Instance) []map[string]any {
	out := []map[string]any{}
	for _, in := range list {
		opt := func(n int, ok bool) any {
			if !ok {
				return nil
			}
			return n
		}
		w, h, dpi := in.Display()
		keys := in.Config.Keys()
		if keys == nil {
			keys = []string{}
		}
		out = append(out, map[string]any{
			"index": in.Index, "name": in.Name, "title": in.Title,
			"memory_mb": opt(in.MemoryMB()), "cpu": opt(in.CPU()), "fps": in.FPS(),
			"data_partition_gb": opt(in.DataPartitionGB()), "display": []int{w, h, dpi},
			"memory_for": MemoryFor(r, in.Index), "keys": keys,
		})
	}
	return out
}

// goRun does op with this package, shaped like the reference's result.
func goRun(r *root.Root, op refOp) (any, error) {
	switch op["op"] {
	case "create":
		memory, cpu, data, err := ValidateOptions(optInt(op, "memory_mb"), optInt(op, "cpu"), optInt(op, "data_gb"))
		if err != nil {
			return nil, err
		}
		fps, err := ValidateFPS(optInt(op, "fps"))
		if err != nil {
			return nil, err
		}
		title, _ := op["title"].(string)
		old := nowISO
		defer func() { nowISO = old }()
		nowISO = func() string { return op["created_at"].(string) }
		in, err := Create(r, op["index"].(int), CreateOptions{Title: title, MemoryMB: memory, CPU: cpu,
			DataPartitionGB: data, FPS: fps})
		if err != nil {
			return nil, err
		}
		return strconv.Itoa(in.Index), nil
	case "update":
		ch := op["changes"].(map[string]any)
		opt := UpdateOptions{MemoryMB: optInt(ch, "memoryMb"), CPU: optInt(ch, "cpu"), FPS: optInt(ch, "fps")}
		applied, err := Update(r, op["index"].(int), opt)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		for k, v := range applied {
			if k == "memory_mb" {
				k = "memoryMb"
			}
			out[k] = v
		}
		return out, nil
	case "apply_display":
		return ApplyDisplay(r, op["index"].(int))
	case "delete":
		return nil, Delete(r, op["index"].(int))
	case "list":
		list, err := List(r)
		if err != nil {
			return nil, err
		}
		return describe(r, list), nil
	case "validate":
		m, c, d, err := ValidateOptions(optInt(op, "memory_mb"), optInt(op, "cpu"), optInt(op, "data_gb"))
		if err != nil {
			return nil, err
		}
		f, err := ValidateFPS(optInt(op, "fps"))
		if err != nil {
			return nil, err
		}
		return []int{m, c, d, f}, nil
	}
	panic(fmt.Sprint("unknown op ", op["op"]))
}

func normJSON(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// compareWithPython runs ops on the same directory twice, once through AUTO-MAS's code (the reference script) and
// once through this package, both from setup, and compares every result, error and resulting file byte for byte.
// Error texts are compared for ops marked "same_error".
func compareWithPython(t *testing.T, setup map[string]string, ops []refOp) {
	t.Helper()
	py := python(t)
	dir := t.TempDir()
	resetTree(t, dir, setup)
	jobDir := t.TempDir()
	job, err := json.Marshal(map[string]any{"root": dir, "ops": ops})
	if err != nil {
		t.Fatal(err)
	}
	jobPath, outPath := filepath.Join(jobDir, "job.json"), filepath.Join(jobDir, "out.json")
	writeFile(t, jobPath, job)
	script, _ := filepath.Abs(filepath.Join("testdata", "instances_ref.py"))
	if out, err := exec.Command(py, "-I", script, jobPath, outPath).CombinedOutput(); err != nil {
		t.Fatalf("python: %v\n%s", err, out)
	}
	var want []refResult
	if err := json.Unmarshal(readFile(t, outPath), &want); err != nil {
		t.Fatal(err)
	}
	if len(want) != len(ops) {
		t.Fatalf("python ran %d of %d ops", len(want), len(ops))
	}

	resetTree(t, dir, setup)
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	for k, op := range ops {
		label := fmt.Sprintf("op %d %v", k, op)
		got, gotErr := goRun(r, op)
		w := want[k]
		switch {
		case (gotErr != nil) != (w.Error != nil):
			t.Errorf("%s: go error %v, python error %v", label, gotErr, deref(w.Error))
		case gotErr != nil:
			if op["same_error"] == true && errs.As(gotErr).Message != *w.Error {
				t.Errorf("%s: go error %q, python %q", label, errs.As(gotErr).Message, *w.Error)
			}
		default:
			var wantResult any
			if err := json.Unmarshal(w.Result, &wantResult); err != nil {
				t.Fatal(err)
			}
			if g := normJSON(t, got); !reflect.DeepEqual(g, wantResult) {
				gj, _ := json.Marshal(g)
				t.Errorf("%s: result\n go %s\n py %s", label, gj, w.Result)
			}
		}
		wantTree := map[string][]byte{}
		for rel, b64 := range w.Tree {
			data, err := base64.StdEncoding.DecodeString(b64)
			if err != nil {
				t.Fatal(err)
			}
			wantTree[rel] = data
		}
		diffTrees(t, label, snapshot(t, dir), wantTree)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

func TestCreateUpdateDeleteMatchPython(t *testing.T) {
	ops := []refOp{
		{"op": "create", "index": 0, "created_at": "2026-10-16T07:41:39"},
		{"op": "create", "index": 1, "title": "星铁 = 测试", "memory_mb": 3072, "cpu": 2, "data_gb": 16, "fps": 30,
			"created_at": "2026-10-16T07:41:40"},
		{"op": "create", "index": 9, "title": "  spaced  ", "memory_mb": 5120, "cpu": 4, "data_gb": 512,
			"created_at": "2026-10-16T07:41:41"},
		{"op": "create", "index": 1, "created_at": "x", "same_error": true},
		{"op": "create", "index": 5, "created_at": "x"},
		{"op": "create", "index": 3, "memory_mb": 1000, "created_at": "x", "same_error": true},
		{"op": "create", "index": 3, "cpu": 3, "created_at": "x", "same_error": true},
		{"op": "create", "index": 3, "data_gb": 15, "created_at": "x", "same_error": true},
		{"op": "create", "index": 3, "data_gb": 513, "created_at": "x", "same_error": true},
		{"op": "create", "index": 3, "fps": 45, "created_at": "x", "same_error": true},
		{"op": "list"},
		{"op": "update", "index": 1, "changes": map[string]any{"memoryMb": 4096}},
		{"op": "update", "index": 0, "changes": map[string]any{"cpu": 4, "fps": 30}},
		{"op": "update", "index": 9, "changes": map[string]any{}},
		{"op": "update", "index": 7, "changes": map[string]any{"cpu": 4}, "same_error": true},
		{"op": "update", "index": 0, "changes": map[string]any{"fps": 45}, "same_error": true},
		{"op": "update", "index": 0, "changes": map[string]any{"cpu": 3}, "same_error": true},
		{"op": "update", "index": 0, "changes": map[string]any{"memoryMb": 8192}, "same_error": true},
		{"op": "apply_display", "index": 0},
		{"op": "list"},
		{"op": "delete", "index": 9},
		{"op": "delete", "index": 7},
		{"op": "list"},
		{"op": "create", "index": 9, "created_at": "2026-10-16T07:41:42"},
		{"op": "validate"},
		{"op": "validate", "memory_mb": 3072, "cpu": 2, "data_gb": 16, "fps": 30},
		{"op": "validate", "memory_mb": 6144, "cpu": 7, "same_error": true},
	}
	compareWithPython(t, map[string]string{}, ops)
}

// A root as AUTO-MAS leaves it, with hand-edited files: BOM, LF / CR line ends, comments, duplicates, a 1080p
// instance from before the fixed 720p, leftover metadata of a deleted instance.
func TestExistingRootMatchesPython(t *testing.T) {
	meta := "\xef\xbb\xbf{\n  \"components\": {\"emulator\": \"37.2.10\"},\n  \"addedAt\": \"2026-10-11T15:24:45\",\n" +
		"  \"instances\": {\n    \"0\": {\"memoryMb\": 4096, \"balloon\": true, \"initialized\": true, \"initErrors\": [],\n" +
		"      \"renderer\": \"GLES: Google (NVIDIA)\"},\n" +
		"    \"3\": {\"renderer\": \"old\", \"initialized\": true, \"createdAt\": \"2026-01-01T00:00:00\", \"x\": 1.50}\n  },\n" +
		"  \"sdkRoot\": \"\",\n  \"zz\": [1e3, {}, []]\n}\n"
	config0 := "# hand edited\nPlayStore.enabled=no\n hw.ramSize = 4G \navd.ini.displayname=方舟\n=orphan\nno equals here\n" +
		"hw.lcd.width=1920\rhw.lcd.height=1080\r\nhw.lcd.density=480\nhw.mainKeys=no\nhw.cpu.ncore=4\n" +
		"disk.dataPartition.size=65536M\nhw.cpu.ncore=6\nimage.sysdir.1=" + SystemImageSysdir + "\n"
	config2 := "\xef\xbb\xbfhw.lcd.width=720\r\nhw.lcd.height=1280\r\nhw.lcd.density=240\r\nhw.mainKeys=yes\r\n" +
		"disk.dataPartition.size=8G\r\nbad=\xff\xfe\r\n"
	setup := map[string]string{
		root.MetadataFile:          meta,
		`avd\mas_0.ini`:            "avd.ini.encoding=UTF-8\npath=X:\\elsewhere\\mas_0.avd\n",
		`avd\mas_0.avd\config.ini`: config0,
		`avd\mas_2.ini`:            "",
		`avd\mas_2.avd\config.ini`: config2,
		`avd\mas_4.ini`:            "",
		`avd\mas_4.avd\config.ini`: "",
	}
	ops := []refOp{
		{"op": "list"},
		{"op": "apply_display", "index": 0},
		{"op": "apply_display", "index": 0},
		{"op": "apply_display", "index": 2},
		{"op": "apply_display", "index": 4, "same_error": true},
		{"op": "apply_display", "index": 8, "same_error": true},
		{"op": "update", "index": 0, "changes": map[string]any{"memoryMb": 3072}},
		{"op": "update", "index": 2, "changes": map[string]any{"cpu": 2}, "same_error": true},
		{"op": "update", "index": 4, "changes": map[string]any{}, "same_error": true},
		{"op": "create", "index": 3, "title": "终末地", "created_at": "2026-10-28T16:00:00"},
		{"op": "list"},
		{"op": "delete", "index": 0},
		{"op": "delete", "index": 4},
		{"op": "list"},
	}
	compareWithPython(t, setup, ops)
}

// TestPropertiesMatchPython: how odd values in config.ini and mas-avd.json read (AvdInstance properties,
// display_changes, memory_for, the mas_*.ini scan).
func TestPropertiesMatchPython(t *testing.T) {
	ram := []string{"4096", "4096M", "4096MB", "4G", "4g", " 4 G ", "4096mb", "4096MMB", "4096BMB", "4GB", "", "abc",
		"-1G", "1_024M", "0", "4096K", "+4G", "4096 M", "M", "G", "MB", "4096\x1c", "\u30004096\u3000", "1__0M"}
	cpu := []string{"6", " 4 ", "x", "", "-2", "1_0", "6.0", "+6", "0"}
	vsync := []string{"60", "30", "0", "", "abc", "-5", "120", " 30 ", "0x1e"}
	data := []string{"64G", "64g", "65536M", "65535M", "64GB", "", "-1M", "1023M", "64", " 32 G", "-1025m", "1_6G", "G"}
	dims := [][2]string{{"1280", "720"}, {"720", "1280"}, {"1920", "1080"}, {"1080", "1920"}, {"", ""},
		{"abc", "100"}, {"720", "720"}, {"-5", "-1"}}
	titles := []string{"", "  ", "星铁", "a=b", "mas_x"}
	metas := []string{`{"memoryMb": 4096}`, `{"memoryMb": 4096.0}`, `{"memoryMb": "4096"}`, `{"memoryMb": true}`,
		`{"memoryMb": 3000}`, `{"memoryMb": null}`, `5`, `{"memoryMb": 5120}`, `{}`, `{"memoryMb": 3072, "x": 1}`,
		`{"memoryMb": 6144e0}`, `{"memoryMb": -0}`}

	setup := map[string]string{}
	var instMeta []string
	for k := 0; k < 30; k++ {
		i := 100 + k
		d := dims[k%len(dims)]
		cfg := fmt.Sprintf("avd.ini.displayname=%s\nhw.ramSize=%s\nhw.cpu.ncore=%s\nhw.lcd.vsync=%s\n"+
			"disk.dataPartition.size=%s\nhw.lcd.width=%s\nhw.lcd.height=%s\n",
			titles[k%len(titles)], ram[k%len(ram)], cpu[k%len(cpu)], vsync[k%len(vsync)], data[k%len(data)], d[0], d[1])
		setup[fmt.Sprintf(`avd\mas_%d.ini`, i)] = ""
		setup[fmt.Sprintf(`avd\mas_%d.avd\config.ini`, i)] = cfg
		instMeta = append(instMeta, fmt.Sprintf("%q: %s", strconv.Itoa(i), metas[k%len(metas)]))
	}
	// The last ram values once more, so every value is seen.
	for k, v := range ram {
		i := 200 + k
		setup[fmt.Sprintf(`avd\mas_%d.ini`, i)] = ""
		setup[fmt.Sprintf(`avd\mas_%d.avd\config.ini`, i)] = "hw.ramSize=" + v + "\ndisk.dataPartition.size=" +
			data[k%len(data)] + "\n"
	}
	// The scan: case, leading zeros, no config, config that is a directory, not a number, outside 0-9.
	for rel, content := range map[string]string{
		`avd\MAS_2.INI`: "", `avd\mas_2.avd\config.ini`: "avd.ini.displayname=upper\n",
		`avd\mas_03.ini`: "", `avd\mas_3.avd\config.ini`: "avd.ini.displayname=zero\n",
		`avd\mas_4.ini`: "",
		`avd\mas_6.ini`: "", `avd\mas_6.avd\config.ini\inner`: "x",
		`avd\mas_x.ini`: "", `avd\mas_.ini`: "", `avd\mas_7.ini.bak`: "", `avd\mas_7.avd\config.ini`: "a=b\n",
		`avd\mas_12.ini`: "", `avd\mas_12.avd\config.ini`: "hw.ramSize=3G\n",
		`avd\other.ini`:      "",
		`avd\mas_1.x.ini`:    "",
		`avd\mas_8.ini\file`: "x", `avd\mas_8.avd\config.ini`: "avd.ini.displayname=ini is a dir\n",
	} {
		setup[rel] = content
	}
	setup[root.MetadataFile] = `{"instances": {` + strings.Join(instMeta, ", ") + `, "12": {"memoryMb": 3072}}}`
	compareWithPython(t, setup, []refOp{{"op": "list"}})
}

// TestMatchesProductionInstance: an instance created here with the options of D:\ModAVD's mas_1 (made by AUTO-MAS
// on 10-16; read only) is the same bytes. The template has changed once since: hw.mainKeys no -> yes (ModAVD #1).
func TestMatchesProductionInstance(t *testing.T) {
	const prod = `D:\ModAVD`
	prodIni, err1 := os.ReadFile(prod + `\avd\mas_1.ini`)
	prodCfg, err2 := os.ReadFile(prod + `\avd\mas_1.avd\config.ini`)
	if err1 != nil || err2 != nil {
		t.Skip("no production instance mas_1")
	}
	pr, err := root.New(prod)
	if err != nil {
		t.Fatal(err)
	}
	in, err := Get(pr, 1)
	if err != nil {
		t.Fatal(err)
	}
	memory, ok1 := in.MemoryMB()
	cpu, ok2 := in.CPU()
	dataGB, ok3 := in.DataPartitionGB()
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("production options unreadable: %v", in.Config.Keys())
	}
	title := in.Title
	if title == in.Name {
		title = ""
	}
	r := newRoot(t)
	created, err := Create(r, 1, CreateOptions{Title: title, MemoryMB: memory, CPU: cpu, DataPartitionGB: dataGB,
		FPS: in.FPS()})
	if err != nil {
		t.Fatal(err)
	}
	wantCfg := bytes.Replace(prodCfg, []byte("\r\nhw.mainKeys=no\r\n"), []byte("\r\nhw.mainKeys=yes\r\n"), 1)
	if got := readFile(t, ConfigPath(r, 1)); !bytes.Equal(got, wantCfg) {
		t.Errorf("config.ini differs\n got %q\nwant %q", got, wantCfg)
	}
	wantIni := bytes.Replace(prodIni, []byte(`path=D:\ModAVD\avd\mas_1.avd`), []byte(`path=`+Dir(r, 1)), 1)
	if got := readFile(t, IniPath(r, 1)); !bytes.Equal(got, wantIni) {
		t.Errorf("mas_1.ini differs\n got %q\nwant %q", got, wantIni)
	}
	if created.Title != in.Title || created.Dir != Dir(r, 1) || created.Name != "mas_1" {
		t.Errorf("returned instance %+v", created)
	}
	// Production's instances.1 starts with the keys Create writes, in that order.
	keys := pr.InstanceMeta(1).Keys()
	if len(keys) < 3 || strings.Join(keys[:3], ",") != "memoryMb,initialized,createdAt" {
		t.Errorf("production meta keys %v", keys)
	}
	if got := strings.Join(r.InstanceMeta(1).Keys(), ","); got != "memoryMb,initialized,createdAt" {
		t.Errorf("meta keys %s", got)
	}
}

// ---- Go-only behaviour -------------------------------------------------------------------------------------------

func TestIni(t *testing.T) {
	var nilIni *Ini
	if nilIni.Len() != 0 || nilIni.Keys() != nil || nilIni.Value("x") != "" || len(nilIni.Encode()) != 0 {
		t.Fatal("nil Ini")
	}
	c := NewIni()
	c.Set("b", "1")
	c.Set("a", "2")
	c.Set("b", "3")
	if v, ok := c.Get("b"); !ok || v != "3" || c.Len() != 2 || strings.Join(c.Keys(), ",") != "b,a" {
		t.Fatalf("Set: %v", c.Keys())
	}
	if _, ok := c.Get("zz"); ok {
		t.Fatal("Get missing")
	}
	k := c.Keys()
	k[0] = "changed"
	if c.Keys()[0] != "b" {
		t.Fatal("Keys is not a copy")
	}
	if string(c.Encode()) != "b=3\na=2\n" {
		t.Fatalf("Encode %q", c.Encode())
	}
	var zero Ini
	zero.Set("k", "v")
	if zero.Value("k") != "v" {
		t.Fatal("zero Ini")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "x.ini")
	// The directory is never created (a deleted instance must not come back).
	err := WriteIni(path, c)
	wantCode(t, err, errs.NotFound)
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("directory created")
	}
	if err := os.Mkdir(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// AUTO-MAS's own temp file (write_ini: <path>.tmp) is not ours to truncate.
	writeFile(t, path+".tmp", []byte("AUTO-MAS is writing"))
	if err := WriteIni(path, c); err != nil {
		t.Fatal(err)
	}
	if got := string(readFile(t, path)); got != "b=3\r\na=2\r\n" {
		t.Fatalf("WriteIni %q", got)
	}
	if got := string(readFile(t, path+".tmp")); got != "AUTO-MAS is writing" {
		t.Fatalf("AUTO-MAS's temp file touched: %q", got)
	}
	if left, _ := filepath.Glob(path + ".modavd-*.tmp"); len(left) != 0 {
		t.Fatalf("temp file left: %v", left)
	}
	back, err := ReadIni(path)
	if err != nil || strings.Join(back.Keys(), ",") != "b,a" || back.Value("b") != "3" {
		t.Fatalf("ReadIni: %v %v", back.Keys(), err)
	}
	_, err = ReadIni(filepath.Join(dir, "missing.ini"))
	wantCode(t, err, errs.NotFound)
	_, err = ReadIni(filepath.Join(dir, "no-dir", "missing.ini"))
	wantCode(t, err, errs.NotFound)
	if !strings.Contains(err.Error(), "missing.ini 读不出：文件不存在") {
		t.Fatalf("message: %v", err)
	}
	_, err = ReadIni(dir)
	wantCode(t, err, errs.Internal)
}

func TestPaths(t *testing.T) {
	r := newRoot(t)
	if IniPath(r, 3) != r.Dir+`\avd\mas_3.ini` || Dir(r, 3) != r.Dir+`\avd\mas_3.avd` ||
		ConfigPath(r, 3) != r.Dir+`\avd\mas_3.avd\config.ini` {
		t.Fatal(IniPath(r, 3), Dir(r, 3), ConfigPath(r, 3))
	}
	for name, want := range map[string]int{"mas_0.ini": 0, "MAS_9.INI": 9, "mas_007.Ini": 7, "mas_12.ini": 12} {
		if got, ok := indexOfIniName(name); !ok || got != want {
			t.Errorf("%s: %d %v", name, got, ok)
		}
	}
	for _, name := range []string{"mas_.ini", "mas_x.ini", "mas_1.x.ini", "mas_1.ini.bak", "ma_1.ini", "maſ_1.ini",
		"mas_-1.ini", "mas_+1.ini", "mas_ 1.ini", "mas_99999999999999999999.ini", ".ini", "mas_1"} {
		if _, ok := indexOfIniName(name); ok {
			t.Errorf("%s matched", name)
		}
	}
}

func TestListAndGet(t *testing.T) {
	r := newRoot(t)
	if list, err := List(r); err != nil || list == nil || len(list) != 0 {
		t.Fatalf("no avd dir: %v %v", list, err)
	}
	writeFile(t, r.AvdHome(), []byte("a file, not a directory"))
	if list, err := List(r); err != nil || len(list) != 0 {
		t.Fatalf("avd is a file: %v %v", list, err)
	}
	os.Remove(r.AvdHome())
	_, err := Get(r, 0)
	wantCode(t, err, errs.NotFound)
	if err.Error() != "魔改 AVD 实例 0 不存在" {
		t.Fatalf("message: %v", err)
	}
	for _, i := range []int{4, 0, 9} {
		if _, err := Create(r, i, CreateOptions{MemoryMB: 4096, CPU: 4, DataPartitionGB: 64, FPS: 60}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := List(r)
	if err != nil || len(list) != 3 || list[0].Index != 0 || list[1].Index != 4 || list[2].Index != 9 {
		t.Fatalf("List: %v %v", list, err)
	}
	in, err := Get(r, 4)
	if err != nil || in.Name != "mas_4" || in.Title != "mas_4" || in.Dir != Dir(r, 4) {
		t.Fatalf("Get: %+v %v", in, err)
	}
	if m, ok := in.MemoryMB(); !ok || m != 4096 {
		t.Errorf("MemoryMB %d", m)
	}
	if c, ok := in.CPU(); !ok || c != 4 {
		t.Errorf("CPU %d", c)
	}
	if d, ok := in.DataPartitionGB(); !ok || d != 64 {
		t.Errorf("DataPartitionGB %d", d)
	}
	if w, h, dpi := in.Display(); in.FPS() != 60 || w != 1280 || h != 720 || dpi != 240 {
		t.Errorf("FPS / Display %d %d %d %d", in.FPS(), w, h, dpi)
	}
	// An instance whose config.ini cannot be read (opened elsewhere without sharing) is listed with an empty config
	// (AUTO-MAS read_ini).
	p, _ := syscall.UTF16PtrFromString(ConfigPath(r, 9))
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	list, err = List(r)
	syscall.CloseHandle(h)
	if err != nil || len(list) != 3 || list[2].Title != "mas_9" || list[2].Config.Len() != 0 {
		t.Fatalf("locked config: %v %v", list, err)
	}
	empty := &Instance{Config: nil}
	if _, ok := empty.MemoryMB(); ok || empty.FPS() != DefaultFPS {
		t.Error("nil config")
	}
	if w, h, _ := empty.Display(); w != 1280 || h != 720 {
		t.Error("nil config display")
	}
}

func TestParseRAMSizeOverflow(t *testing.T) {
	for _, s := range []string{"9223372036854775807G", "9007199254740993G", "-9007199254740993G"} {
		if v, ok := parseRAMSize(s); ok {
			t.Errorf("%s: %d", s, v)
		}
	}
	if v, ok := parseRAMSize("9007199254740991G"); !ok || v != 9007199254740991*1024 {
		t.Errorf("largest: %d %v", v, ok)
	}
	for v, want := range map[int]int{0: 0, 1023: 0, 1024: 1, -1: -1, -1024: -1, -1025: -2} {
		if got := floorDiv1024(v); got != want {
			t.Errorf("floorDiv1024(%d) = %d", v, got)
		}
	}
}

func TestParseMemoryMB(t *testing.T) {
	for in, want := range map[string]int{"4096": 4096, "4096M": 4096, "4096mb": 4096, "4G": 4096, "6gb": 6144,
		" 3072MB ": 3072, "5 G": 5120, "0": 0, "007": 7, "9999999": 9999999} {
		got, err := ParseMemoryMB(in)
		if err != nil || got != want {
			t.Errorf("ParseMemoryMB(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "  ", "G", "MB", "-4G", "+4G", "4.5G", "4K", "4096B", "4 096", "0x10",
		"99999999", "4GG", "4MG", "４G", "lots"} {
		_, err := ParseMemoryMB(in)
		wantCode(t, err, errs.InvalidArgument)
		if !strings.Contains(err.Error(), "内存要写成 6144、6144M 或 6G，收到「"+in+"」") {
			t.Errorf("message: %v", err)
		}
	}
}

func TestValidate(t *testing.T) {
	m, c, d, err := ValidateOptions(nil, nil, nil)
	if err != nil || m != 6144 || c != 6 || d != 64 {
		t.Fatalf("defaults %d %d %d %v", m, c, d, err)
	}
	for _, tc := range []struct {
		memory, cpu, data *int
		msg               string
	}{
		{ip(1000), nil, nil, "魔改 AVD 的内存只能选 3 GB / 4 GB / 5 GB / 6 GB"},
		{ip(0), ip(3), ip(1), "魔改 AVD 的内存只能选 3 GB / 4 GB / 5 GB / 6 GB"},
		{nil, ip(3), ip(1), "魔改 AVD 的 CPU 核数只能选 2 / 4 / 6"},
		{nil, nil, ip(15), "数据盘上限只能在 16–512 GB 之间"},
		{nil, nil, ip(513), "数据盘上限只能在 16–512 GB 之间"},
	} {
		_, _, _, err := ValidateOptions(tc.memory, tc.cpu, tc.data)
		wantCode(t, err, errs.InvalidArgument)
		if err.Error() != tc.msg {
			t.Errorf("got %q want %q", err.Error(), tc.msg)
		}
	}
	if _, _, d, err := ValidateOptions(nil, nil, ip(16)); err != nil || d != 16 {
		t.Error("16 GB")
	}
	if _, _, d, err := ValidateOptions(nil, nil, ip(512)); err != nil || d != 512 {
		t.Error("512 GB")
	}
	if f, err := ValidateFPS(nil); err != nil || f != 60 {
		t.Error("fps default")
	}
	if f, err := ValidateFPS(ip(30)); err != nil || f != 30 {
		t.Error("fps 30")
	}
	_, err = ValidateFPS(ip(0))
	wantCode(t, err, errs.InvalidArgument)
	if err.Error() != "魔改 AVD 的帧率只能选 30 / 60" {
		t.Errorf("fps message %q", err.Error())
	}
}

func TestMemoryFor(t *testing.T) {
	r := newRoot(t)
	if MemoryFor(r, 0) != DefaultMemoryMB {
		t.Fatal("no metadata")
	}
	writeFile(t, r.MetadataPath(), []byte(`{"instances": {"0": {"memoryMb": 3072}, "1": {"memoryMb": 4096.0},
		"2": {"memoryMb": "4096"}, "3": {"memoryMb": 2048}, "4": {"memoryMb": true}, "6": []}}`))
	for i, want := range map[int]int{0: 3072, 1: 6144, 2: 6144, 3: 6144, 4: 6144, 6: 6144, 7: 6144} {
		if got := MemoryFor(r, i); got != want {
			t.Errorf("MemoryFor(%d) = %d", i, got)
		}
	}
	writeFile(t, r.MetadataPath(), []byte(`{broken`))
	if MemoryFor(r, 0) != DefaultMemoryMB {
		t.Fatal("unreadable metadata")
	}
}

func TestFreeIndex(t *testing.T) {
	var asked [][]int
	busy := func(ports []int) []int {
		asked = append(asked, ports)
		if ports[0] == 20010 || ports[2] == 20032 {
			return ports[:1]
		}
		return nil
	}
	i, err := FreeIndex(map[int]bool{0: true, 2: true}, busy)
	if err != nil || i != 4 {
		t.Fatalf("FreeIndex = %d %v", i, err)
	}
	if fmt.Sprint(asked) != "[[20010 20011 20012] [20030 20031 20032] [20040 20041 20042]]" {
		t.Fatalf("asked %v", asked)
	}
	if i, err := FreeIndex(nil, nil); err != nil || i != 0 {
		t.Fatalf("empty: %d %v", i, err)
	}
	all := map[int]bool{}
	for k := 0; k <= 9; k++ {
		all[k] = k != 7
	}
	if i, err := FreeIndex(all, nil); err != nil || i != 7 {
		t.Fatalf("only 7 free: %d %v", i, err)
	}
	_, err = FreeIndex(all, func(ports []int) []int { return ports })
	wantCode(t, err, errs.Busy)
	if err.Error() != "魔改 AVD 实例已满（端口段 20000–20100 最多 9 台）" {
		t.Fatalf("message %v", err)
	}
	all[7] = true
	all[5] = false // 5 is never free
	_, err = FreeIndex(all, nil)
	wantCode(t, err, errs.Busy)
}

func TestCreateRefusals(t *testing.T) {
	r := newRoot(t)
	ok := CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60}
	for _, i := range []int{-1, 5, 10} {
		_, err := Create(r, i, ok)
		wantCode(t, err, errs.InvalidArgument)
	}
	_, err := Create(r, 0, CreateOptions{})
	wantCode(t, err, errs.InvalidArgument)
	bad := ok
	bad.FPS = 45
	_, err = Create(r, 0, bad)
	wantCode(t, err, errs.InvalidArgument)
	bad = ok
	bad.Title = "two\nlines"
	_, err = Create(r, 0, bad)
	wantCode(t, err, errs.InvalidArgument)
	if !strings.Contains(err.Error(), "换行") {
		t.Fatalf("title message %v", err)
	}
	if _, err := os.Stat(r.AvdHome()); !os.IsNotExist(err) {
		t.Fatal("refused creates wrote something")
	}

	// Leftovers block the index: an .ini alone, or a directory alone.
	writeFile(t, IniPath(r, 1), nil)
	_, err = Create(r, 1, ok)
	wantCode(t, err, errs.AlreadyExists)
	if err.Error() != "实例 mas_1 已存在" {
		t.Fatalf("message %v", err)
	}
	if err := os.MkdirAll(Dir(r, 2), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = Create(r, 2, ok)
	wantCode(t, err, errs.AlreadyExists)

	// An unreadable mas-avd.json stops Create before any file is written, and stays as it was.
	writeFile(t, r.MetadataPath(), []byte(`{"broken": `))
	_, err = Create(r, 3, ok)
	wantCode(t, err, errs.MetadataUnreadable)
	if _, err := os.Stat(Dir(r, 3)); !os.IsNotExist(err) {
		t.Fatal("files written despite unreadable metadata")
	}
	if string(readFile(t, r.MetadataPath())) != `{"broken": ` {
		t.Fatal("metadata overwritten")
	}
}

// TestCreateRollsBack: when instances.<i> cannot be written at the end, the new files go again.
func TestCreateRollsBack(t *testing.T) {
	r := newRoot(t)
	// A directory where the metadata temp file must go makes the final write fail.
	tmp := fmt.Sprintf("%s.modavd-%d.tmp", r.MetadataPath(), os.Getpid())
	if err := os.MkdirAll(filepath.Join(tmp, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Create(r, 0, CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60})
	if err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(Dir(r, 0)); !os.IsNotExist(err) {
		t.Error("directory left")
	}
	if _, err := os.Stat(IniPath(r, 0)); !os.IsNotExist(err) {
		t.Error(".ini left")
	}
	os.RemoveAll(tmp)
	if _, err := Create(r, 0, CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60}); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestUpdateTitleAndApplied(t *testing.T) {
	r := newRoot(t)
	if _, err := Create(r, 0, CreateOptions{Title: "旧名", MemoryMB: 4096, CPU: 4, DataPartitionGB: 64, FPS: 60}); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, ConfigPath(r, 0))
	title := "新名 = 星铁"
	applied, err := Update(r, 0, UpdateOptions{Title: &title, FPS: ip(30)})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(applied) != "map[fps:30 title:新名 = 星铁]" {
		t.Fatalf("applied %v", applied)
	}
	in, _ := Get(r, 0)
	if in.Title != title || in.FPS() != 30 {
		t.Fatalf("after update: %q %d", in.Title, in.FPS())
	}
	// Changed in place: same keys in the same order.
	after := readFile(t, ConfigPath(r, 0))
	want := strings.Replace(strings.Replace(string(before), "avd.ini.displayname=旧名\r\n",
		"avd.ini.displayname="+title+"\r\n", 1), "hw.lcd.vsync=60\r\n", "hw.lcd.vsync=30\r\n", 1)
	if string(after) != want {
		t.Fatalf("config\n got %q\nwant %q", after, want)
	}
	empty := ""
	if applied, err := Update(r, 0, UpdateOptions{Title: &empty, MemoryMB: ip(5120), CPU: ip(2)}); err != nil ||
		fmt.Sprint(applied) != "map[cpu:2 memory_mb:5120 title:mas_0]" {
		t.Fatalf("applied %v %v", applied, err)
	}
	if in, _ := Get(r, 0); in.Title != "mas_0" || MemoryFor(r, 0) != 5120 {
		t.Fatalf("empty title / memory: %q %d", in.Title, MemoryFor(r, 0))
	}
	bad := "a\u2028b"
	_, err = Update(r, 0, UpdateOptions{Title: &bad})
	wantCode(t, err, errs.InvalidArgument)
	if applied, err := Update(r, 0, UpdateOptions{}); err != nil || len(applied) != 0 {
		t.Fatalf("nothing given: %v %v", applied, err)
	}
	_, err = Update(r, 1, UpdateOptions{})
	wantCode(t, err, errs.NotFound)

	// An unreadable mas-avd.json stops Update before config.ini changes.
	cfg := readFile(t, ConfigPath(r, 0))
	writeFile(t, r.MetadataPath(), []byte(`[`))
	_, err = Update(r, 0, UpdateOptions{CPU: ip(6)})
	wantCode(t, err, errs.MetadataUnreadable)
	if !bytes.Equal(cfg, readFile(t, ConfigPath(r, 0))) {
		t.Fatal("config.ini changed")
	}
}

func TestUpdateDataPartition(t *testing.T) {
	r := newRoot(t)
	if _, err := Create(r, 0, CreateOptions{MemoryMB: 4096, CPU: 4, DataPartitionGB: 64, FPS: 60}); err != nil {
		t.Fatal(err)
	}
	dataGB := func() int {
		in, err := Get(r, 0)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := in.DataPartitionGB()
		return n
	}
	before := readFile(t, ConfigPath(r, 0))
	applied, err := Update(r, 0, UpdateOptions{DataPartitionGB: ip(96)})
	if err != nil || fmt.Sprint(applied) != "map[data_partition_gb:96]" {
		t.Fatalf("raise: %v %v", applied, err)
	}
	// changed in place, nothing else touched
	if want := strings.Replace(string(before), "disk.dataPartition.size=64G\r\n", "disk.dataPartition.size=96G\r\n", 1); string(readFile(t, ConfigPath(r, 0))) != want {
		t.Fatalf("config after raise:\n%s", readFile(t, ConfigPath(r, 0)))
	}
	if applied, err := Update(r, 0, UpdateOptions{DataPartitionGB: ip(96)}); err != nil || fmt.Sprint(applied) != "map[data_partition_gb:96]" {
		t.Fatalf("same size: %v %v", applied, err)
	}
	cfg := readFile(t, ConfigPath(r, 0))
	for _, n := range []int{64, 8, 513} {
		_, err := Update(r, 0, UpdateOptions{DataPartitionGB: ip(n)})
		wantCode(t, err, errs.InvalidArgument)
	}
	if !bytes.Equal(cfg, readFile(t, ConfigPath(r, 0))) || dataGB() != 96 {
		t.Fatal("a refused size changed config.ini")
	}

	// An old 8 GB instance is outside the range: other settings are refused, raising it is the way out.
	writeFile(t, ConfigPath(r, 0), bytes.Replace(cfg, []byte("disk.dataPartition.size=96G"), []byte("disk.dataPartition.size=8G"), 1))
	_, err = Update(r, 0, UpdateOptions{CPU: ip(6)})
	wantCode(t, err, errs.InvalidArgument)
	if _, err := Update(r, 0, UpdateOptions{CPU: ip(6), DataPartitionGB: ip(32)}); err != nil || dataGB() != 32 {
		t.Fatalf("raise an 8 GB instance: %v, now %d GB", err, dataGB())
	}
}

func TestDelete(t *testing.T) {
	r := newRoot(t)
	opt := CreateOptions{MemoryMB: 6144, CPU: 6, DataPartitionGB: 64, FPS: 60}
	for _, i := range []int{0, 1} {
		if _, err := Create(r, i, opt); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(Dir(r, 0), "data", "big.img"), []byte("disk"))
	if err := Delete(r, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(Dir(r, 0)); !os.IsNotExist(err) {
		t.Fatal("dir left")
	}
	if _, err := os.Stat(IniPath(r, 0)); !os.IsNotExist(err) {
		t.Fatal("ini left")
	}
	if r.MetadataLoose().Obj().Get("instances").Obj().Has("0") || !r.MetadataLoose().Obj().Get("instances").Obj().Has("1") {
		t.Fatal("metadata")
	}
	if err := Delete(r, 0); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	_, err := Get(r, 0)
	wantCode(t, err, errs.NotFound)
	wantCode(t, Delete(r, -1), errs.InvalidArgument)

	// Unreadable metadata: nothing is deleted.
	meta := readFile(t, r.MetadataPath())
	writeFile(t, r.MetadataPath(), []byte(`{`))
	wantCode(t, Delete(r, 1), errs.MetadataUnreadable)
	if _, err := os.Stat(ConfigPath(r, 1)); err != nil {
		t.Fatal("deleted despite unreadable metadata")
	}
	writeFile(t, r.MetadataPath(), meta)

	// A junction as the instance directory is refused; what it points to stays.
	target := filepath.Join(t.TempDir(), "elsewhere")
	writeFile(t, filepath.Join(target, "config.ini"), []byte("avd.ini.displayname=far\n"))
	if err := os.RemoveAll(Dir(r, 1)); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", Dir(r, 1), target).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v %s", err, out)
	}
	if in, err := Get(r, 1); err != nil || in.Title != "far" {
		t.Fatalf("junction instance: %v", err)
	}
	err = Delete(r, 1)
	wantCode(t, err, errs.Unsupported)
	if _, err := os.Stat(filepath.Join(target, "config.ini")); err != nil {
		t.Fatal("junction target touched")
	}
	os.Remove(Dir(r, 1))
}

func TestApplyDisplay(t *testing.T) {
	r := newRoot(t)
	_, err := ApplyDisplay(r, 0)
	wantCode(t, err, errs.NotFound)
	if !strings.HasPrefix(err.Error(), "实例 mas_0 的配置读不出") {
		t.Fatalf("message %v", err)
	}
	if _, err := os.Stat(r.AvdHome()); !os.IsNotExist(err) {
		t.Fatal("avd directory created")
	}
	// an empty config.ini is unreadable too, but there is a file: Internal
	writeFile(t, ConfigPath(r, 0), nil)
	_, err = ApplyDisplay(r, 0)
	wantCode(t, err, errs.Internal)
	writeFile(t, ConfigPath(r, 0), []byte("hw.lcd.width=1080\nhw.lcd.height=1920\nhw.lcd.density=480\nx=1\n"))
	wrote, err := ApplyDisplay(r, 0)
	if err != nil || !wrote {
		t.Fatalf("first: %v %v", wrote, err)
	}
	want := "hw.lcd.width=720\r\nhw.lcd.height=1280\r\nhw.lcd.density=240\r\nx=1\r\nhw.mainKeys=yes\r\n"
	if got := string(readFile(t, ConfigPath(r, 0))); got != want {
		t.Fatalf("got %q", got)
	}
	if wrote, err := ApplyDisplay(r, 0); err != nil || wrote {
		t.Fatalf("second: %v %v", wrote, err)
	}
	for cfg, want := range map[string]string{
		"hw.lcd.width=1280\nhw.lcd.height=720\n": "1280x720",
		"hw.lcd.width=720\nhw.lcd.height=1280\n": "720x1280",
		"hw.lcd.width=x\nhw.lcd.height=1\n":      "720x1280",
		"":                                       "1280x720",
		"hw.lcd.height=720\n":                    "720x1280",
	} {
		ch := DisplayChanges(parseIni([]byte(cfg)))
		if got := ch[0][1] + "x" + ch[1][1]; got != want || ch[2][1] != "240" || ch[3] != [2]string{"hw.mainKeys", "yes"} ||
			ch[0][0] != "hw.lcd.width" || ch[1][0] != "hw.lcd.height" || ch[2][0] != "hw.lcd.density" {
			t.Errorf("%q: %v", cfg, ch)
		}
	}
}

func TestCreateKeepsOtherMetadata(t *testing.T) {
	r := newRoot(t)
	writeFile(t, r.MetadataPath(), []byte("{\"a\": 1, \"instances\": {\"2\": {\"x\": true}}, \"b\": [1e3]}"))
	old := nowISO
	nowISO = func() string { return "2026-10-28T16:00:00" }
	defer func() { nowISO = old }()
	if _, err := Create(r, 0, CreateOptions{MemoryMB: 3072, CPU: 2, DataPartitionGB: 16, FPS: 30}); err != nil {
		t.Fatal(err)
	}
	want := "{\r\n  \"a\": 1,\r\n  \"instances\": {\r\n    \"2\": {\r\n      \"x\": true\r\n    },\r\n    \"0\": {\r\n" +
		"      \"memoryMb\": 3072,\r\n      \"initialized\": false,\r\n      \"createdAt\": \"2026-10-28T16:00:00\"\r\n" +
		"    }\r\n  },\r\n  \"b\": [\r\n    1000.0\r\n  ]\r\n}"
	if got := string(readFile(t, r.MetadataPath())); got != want {
		t.Fatalf("got %q", got)
	}
	if v, ok := r.InstanceMeta(0).Get("initialized").BoolVal(); !ok || v {
		t.Fatal("initialized")
	}
}
