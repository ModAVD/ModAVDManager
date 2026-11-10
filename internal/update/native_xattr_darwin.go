//go:build darwin && cgo

package update

import "modavd/internal/macos"

func nativeXattrs(p string) (map[string]string, error) { return macos.SignatureXattrs(p) }
func validateNativeXattrs(attrs map[string]string) error {
	if err := macos.ValidateSignatureXattrs(attrs); err != nil {
		return packageError("签名扩展属性不安全: %v", err)
	}
	return nil
}
func restoreNativeXattrs(p string, attrs map[string]string) error {
	if err := macos.RestoreSignatureXattrs(p, attrs); err != nil {
		return packageError("不能恢复签名扩展属性: %v", err)
	}
	return nil
}
