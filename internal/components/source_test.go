package components

import (
	"fmt"
	"strings"
	"testing"

	"modavd/internal/errs"
)

// The fetch says where it downloads from before it starts: the default order, the one --mirror named, or --mirror-url.
func TestSourceLineNamesWhereFromAndWhy(t *testing.T) {
	for _, c := range []struct {
		name   string
		mirror string
		urls   []string
		want   []string
		absent []string
	}{
		{"default", "", nil,
			[]string{"下载源：自动", "Google 中国下载（googledownloads.cn）、腾讯云镜像（mirrors.cloud.tencent.com）、Google 官方（dl.google.com）", "--mirror googledownloads_cn / tencent / google"},
			[]string{"--mirror-url"}},
		{"one mirror", " Google ", nil,
			[]string{"下载源：只用Google 官方（dl.google.com）", "（--mirror Google）"},
			[]string{"自动", "googledownloads.cn", "tencent"}},
		{"urls win", "tencent", []string{"https://a.example/x, https://b.example/y/"},
			[]string{"自定义镜像（https://a.example/x/）、自定义镜像（https://b.example/y/）", "（--mirror-url），按顺序试"},
			[]string{"自动", "腾讯云"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			mirrors, err := ChooseMirrors(c.mirror, c.urls)
			if err != nil {
				t.Fatal(err)
			}
			got := sourceLine(Options{Mirror: c.mirror, MirrorURLs: c.urls}, mirrors)
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("%q lacks %q", got, w)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(got, a) {
					t.Errorf("%q should not say %q", got, a)
				}
			}
		})
	}
}

// The 下载源 line also says where the choice was made: the saved setting or the manager instead of an option, and
// "默认" when nothing chose (ModAVD#120).
func TestSourceLineSaysWhereTheChoiceWasMade(t *testing.T) {
	for _, c := range []struct {
		o      Options
		want   string
		absent string
	}{
		{Options{}, "下载源：自动（默认，还没有设置过），依次试", "保存的设置"},
		{Options{SourceOrigin: SourceFromSettings}, "下载源：自动（保存的设置），依次试", "默认"},
		{Options{Mirror: "tencent", SourceOrigin: SourceFromSettings}, "（保存的设置）", "--mirror"},
		{Options{MirrorURLs: []string{"https://m.example/sdk/"}, SourceOrigin: SourceFromSettings}, "自定义镜像（https://m.example/sdk/）（保存的设置），按顺序试", "--mirror-url"},
		{Options{Mirror: "google", SourceOrigin: SourceFromManager}, "下载源：只用Google 官方（dl.google.com）（管理器里选的）", "--mirror"},
	} {
		mirrors, err := ChooseMirrors(c.o.Mirror, c.o.MirrorURLs)
		if err != nil {
			t.Fatal(err)
		}
		if got := sourceLine(c.o, mirrors); !strings.Contains(got, c.want) || strings.Contains(got, c.absent) {
			t.Errorf("%+v: %q, want %q without %q", c.o, got, c.want, c.absent)
		}
	}
}

// A custom address is a plain http(s) directory URL with a host; a user name or password is refused without being
// repeated in the message; at most 16 addresses of at most 2048 characters.
func TestCustomMirrorURLShapes(t *testing.T) {
	for _, u := range []string{"https://", "http://", "https:///x", "https://:80/x", "ftp://h/x", "file:///C:/x", "h/x",
		"https://h/x?q=1", "https://h/x?", "https://h/x#f", "https://h/x#", "https://h/ x", "https://h/a\tb", "mailto:a@b",
		"https://h/" + strings.Repeat("a", 2048)} {
		if _, err := ChooseMirrors("", []string{u}); errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%q: %v", u, err)
		}
	}
	_, err := ChooseMirrors("", []string{"https://user:secret@h/x"})
	if errs.CodeOf(err) != errs.InvalidArgument || strings.Contains(err.Error(), "secret") {
		t.Errorf("user info: %v", err)
	}
	for _, u := range []string{"https://h/x", "HTTP://H:8080/a/b/", "https://[::1]:9/x", "http://127.0.0.1:1/"} {
		if _, err := ChooseMirrors("", []string{u}); err != nil {
			t.Errorf("%q refused: %v", u, err)
		}
	}
	many := make([]string, 17)
	for i := range many {
		many[i] = fmt.Sprintf("https://m%d.example/x/", i)
	}
	if _, err := ChooseMirrors("", many[:16]); err != nil {
		t.Errorf("16 addresses: %v", err)
	}
	if _, err := ChooseMirrors("", []string{strings.Join(many, ",")}); errs.CodeOf(err) != errs.InvalidArgument {
		t.Errorf("17 addresses: %v", err)
	}
}
