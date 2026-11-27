//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package cli

import (
	"strings"
	"testing"

	"modavd/internal/emu"
	"modavd/internal/errs"
)

func TestFormatRegionalUnsetTimeZone(t *testing.T) {
	var b strings.Builder
	formatRegional(&b, &emu.RegionalResult{Supported: true, Guest: &emu.RegionalGuest{Language: "ja-JP"}})
	if out := b.String(); !strings.Contains(out, "安卓当前语言：ja-JP\n安卓当前时区：未设置\n") || strings.Contains(out, "读取失败") {
		t.Fatalf("unset time zone: %q", out)
	}
}

func TestRegionalFlags(t *testing.T) {
	create, err := buildCreateRequest(mustParse(t, "create", "--language", "zh-cn", "--timezone", "UTC"))
	if err != nil || create.Language == nil || *create.Language != "zh-CN" || create.TimeZone == nil || *create.TimeZone != "Etc/UTC" {
		t.Fatalf("create: %+v %v", create, err)
	}
	update, err := buildUpdateRequest(mustParse(t, "set", "0", "--language", "host", "--timezone", "preserve"))
	if err != nil || update.Language == nil || *update.Language != "host" || update.TimeZone == nil || *update.TimeZone != "preserve" {
		t.Fatalf("set: %+v %v", update, err)
	}
	for _, args := range [][]string{{"create", "--language", "zh_CN"}, {"create", "--timezone", "Local"}, {"set", "0", "--timezone", "Unknown/Unknown"}, {"set", "0", "--language", "en-US;id"}} {
		inv := mustParse(t, args...)
		var err error
		if args[0] == "create" {
			_, err = buildCreateRequest(inv)
		} else {
			_, err = buildUpdateRequest(inv)
		}
		if errs.CodeOf(err) != errs.InvalidArgument {
			t.Errorf("%v: %v", args, err)
		}
	}
}
