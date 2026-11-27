//go:build !windows && (!darwin || !arm64)

package host

import "errors"

func MessageBox(string) error { return errors.New("当前平台暂不支持消息框") }
func InfoBox(string) error    { return errors.New("当前平台暂不支持消息框") }
