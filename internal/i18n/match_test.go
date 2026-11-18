package i18n

import (
	"bytes"
	"testing"
)

func testCatalog() *catalog {
	return newCatalog(map[string]string{
		"实例 %d 正在关机":        "Shutting down instance %d",
		"实例 %d 已整理磁盘，释放 %s": "Compacted the disk of instance %d, %s freed",
		"启动超时":              "Boot timed out",
		"再试一次":              "Try again",
		"%s 被 %s（PID %d）占用": "%[1]s is taken by %[2]s (PID %[3]d)",
		"端口 %d":             "port %d",
		"ModAVD 的 adb 端口":   "ModAVD's adb port",
		"新建中 %d%%":          "Creating %d%%",
		"完成 100%%":          "Done 100%",
		"实例 %v 已存在":         "Instance %v already exists",
		"\n用法：%s\n":         "\nUsage: %s\n",
		"实例 %d %s":          "Instance %d %s",
		"正在开启 GLES 3.2":     "turning GLES 3.2 on",
		"读不了 mas-avd.json":  "Cannot read mas-avd.json",
	})
}

func TestTranslate(t *testing.T) {
	c := testCatalog()
	for _, tc := range []struct{ in, want string }{
		{"启动超时", "Boot timed out"},
		{"  启动超时\n", "  Boot timed out\n"},
		{"实例 3 正在关机", "Shutting down instance 3"},
		{"实例 3 已整理磁盘，释放 2.0 GB", "Compacted the disk of instance 3, 2.0 GB freed"},
		// arguments are translated too, and explicit indexes reorder
		{"ModAVD 的 adb 端口 被 emulator.exe（PID 42）占用", "ModAVD's adb port is taken by emulator.exe (PID 42)"},
		{"新建中 40%", "Creating 40%"},
		{"完成 100%", "Done 100%"},
		{"实例 星铁 已存在", "Instance 星铁 already exists"},
		{"用法：modavd start", "Usage: modavd start"},
		// the most specific key wins over "实例 %d %s"
		{"实例 4 正在关机", "Shutting down instance 4"},
		{"实例 4 正在开启 GLES 3.2", "Instance 4 turning GLES 3.2 on"},
		// no key for the whole: by its parts
		{"启动超时：读不了 mas-avd.json", "Boot timed out: Cannot read mas-avd.json"},
		{"启动超时。再试一次。", "Boot timed out. Try again."},
		{"启动超时（再试一次）", "Boot timed out (Try again)"},
		{"启动超时\n再试一次", "Boot timed out\nTry again"},
		// unknown text and text without Chinese stay
		{"没见过的话", "没见过的话"},
		{"plain text", "plain text"},
		{`D:\模拟器\mas_3`, `D:\模拟器\mas_3`},
	} {
		if got := c.translate(tc.in, 0); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestParseFormat(t *testing.T) {
	segs := parseFormat("a %d b %[3]s c %%d %v %.1f%%")
	var verbs []string
	var args []int
	for _, s := range segs {
		if s.verb != "" {
			verbs = append(verbs, s.verb)
			args = append(args, s.arg)
		}
	}
	if len(verbs) != 4 || verbs[0] != "d" || args[1] != 2 || args[2] != 3 || verbs[3] != "f" || args[3] != 4 {
		t.Fatalf("verbs %v args %v", verbs, args)
	}
}

func TestMatch(t *testing.T) {
	for in, want := range map[string]string{
		"zh-CN": Chinese, "zh-TW": Chinese, "zh_Hans": Chinese, "zh": Chinese,
		"en-US": English, "en": English, "de-DE": English, "": English,
	} {
		if got := Match(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestWriterAndChinese(t *testing.T) {
	var b bytes.Buffer
	if w := Writer(&b, Chinese); w != &b {
		t.Fatal("Chinese wraps the writer")
	}
	if Translate(Chinese, "启动超时") != "启动超时" {
		t.Fatal("Chinese translated")
	}
	w := Writer(&b, English)
	n, err := w.Write([]byte("plain\n"))
	if err != nil || n != 6 || b.String() != "plain\n" {
		t.Fatalf("%d %v %q", n, err, b.String())
	}
}

func TestPreference(t *testing.T) {
	dir := t.TempDir()
	old := prefPath
	prefPath = func() string { return dir + "/language.json" }
	defer func() { prefPath = old }()
	if Preference() != PrefAuto {
		t.Fatal("default")
	}
	if err := SavePreference(English); err != nil || Preference() != English {
		t.Fatalf("save: %v %q", err, Preference())
	}
	if SavePreference("xx") == nil {
		t.Fatal("bad code saved")
	}
	if err := SavePreference(PrefAuto); err != nil || Preference() != PrefAuto {
		t.Fatal("auto")
	}
	t.Setenv(EnvVar, "en")
	Init()
	defer current.Store("")
	if Current() != English {
		t.Fatalf("env: %q", Current())
	}
}
