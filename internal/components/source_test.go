package components

import (
	"strings"
	"testing"
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
