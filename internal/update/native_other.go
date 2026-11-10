//go:build !darwin || !cgo

package update

import (
	"context"

	"modavd/internal/errs"
	"modavd/internal/jsonord"
	"modavd/internal/root"
)

func (u *Updater) nativeUpdates() bool        { return false }
func (u *Updater) hasNativeTransaction() bool { return false }
func (u *Updater) recoverNative() (string, error) {
	return "", errs.New(errs.Unsupported, "当前平台暂不支持 macOS 原生更新")
}
func (u *Updater) nativeUpdate(context.Context, bool) (*Status, error) {
	return u.status(), errs.New(errs.Unsupported, "本地签名包更新只支持原生 macOS ARM64 构建")
}
func (l layout) nativeStagedVersion(*jsonord.Obj) (Version, bool)  { return Version{}, false }
func (u *Updater) validateNativeWorkspace() error                  { return nil }
func (u *Updater) nativeCheckSpace() error                         { return nil }
func (u *Updater) nativeProcessesUsing(bool) ([]processUse, error) { return nil, nil }
func readUpdateStateFile(path string) ([]byte, error)              { return root.ReadFileShared(path) }
func (u *Updater) recoverNativeWorkspace() (string, error)         { return "", nil }
