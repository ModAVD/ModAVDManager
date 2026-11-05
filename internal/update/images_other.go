//go:build !windows

package update

import "modavd/internal/errs"

func (u *Updater) lockSDKImages() (func(), error) {
	return nil, errs.New(errs.Unsupported, "当前平台暂不支持安全换包")
}
