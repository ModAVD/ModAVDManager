//go:build windows

// Copyright 2026 jinghero. All rights reserved.

package cli

import (
	"testing"

	"modavd/internal/errs"
)

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
