// Copyright 2026 jinghero. All rights reserved.

package cli

import (
	"fmt"
	"io"

	"modavd/internal/emu"
)

func runRegional(c *cmdCtx) (any, error) {
	i, m, err := c.indexAndManager()
	if err != nil {
		return nil, err
	}
	return m.Regional(c.ctx, i)
}

func formatRegional(w io.Writer, result *emu.RegionalResult) {
	if !result.Supported {
		fmt.Fprintln(w, "实例语言/时区设置只支持 Windows")
		return
	}
	fmt.Fprintf(w, "语言设置：%s\n时区设置：%s\n", result.Config.Language, result.Config.TimeZone)
	if result.Config.LastLanguage != "" {
		fmt.Fprintf(w, "上次设置的语言：%s\n", result.Config.LastLanguage)
	}
	if result.Config.PendingLanguage != "" {
		fmt.Fprintf(w, "待应用语言：%s\n", result.Config.PendingLanguage)
	}
	if result.Config.Error != "" {
		fmt.Fprintf(w, "配置错误：%s\n", result.Config.Error)
	}
	fmt.Fprintf(w, "宿主语言：%s\n宿主时区：%s\n", orDash(result.HostLanguage), orDash(result.HostTimeZone))
	for _, err := range result.HostErrors {
		fmt.Fprintf(w, "宿主读取失败：%s\n", err)
	}
	if result.Guest == nil {
		fmt.Fprintln(w, "安卓当前值：未读取")
	} else {
		fmt.Fprintf(w, "安卓当前语言：%s\n安卓当前时区：%s\n", result.Guest.Language, result.Guest.TimeZone)
	}
	if result.GuestError != "" {
		fmt.Fprintf(w, "客体读取失败：%s\n", result.GuestError)
	}
}
