//go:build darwin && cgo

package macos

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation -framework Foundation -framework LocalAuthentication
#include <stdlib.h>
#include <stdint.h>
int32_t modavd_wrapping_key(const char *service, const char *account, unsigned char *key, int create);
*/
import "C"

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"unsafe"
)

// Each purpose gets an independent random AES key in this user's Keychain.
// Only the encrypted, versioned envelope is written to the settings file.
const wrappingKeyService = "org.modavd.manager.settings.v1"

var wrappingKeyMu sync.Mutex

// The last byte is the envelope version. Unknown versions are rejected before
// accessing Keychain; raw Windows DPAPI blobs are never mistaken for this form.
var protectedHeader = []byte{'M', 'o', 'd', 'A', 'V', 'D', 'K', 1}

func wrappingKey(entropy []byte, create bool) ([]byte, error) {
	wrappingKeyMu.Lock()
	defer wrappingKeyMu.Unlock()
	digest := sha256.Sum256(entropy)
	service := C.CString(wrappingKeyService)
	defer C.free(unsafe.Pointer(service))
	account := C.CString(hex.EncodeToString(digest[:]))
	defer C.free(unsafe.Pointer(account))
	key := make([]byte, 32)
	flag := C.int(0)
	if create {
		flag = 1
	}
	status := int32(C.modavd_wrapping_key(service, account, (*C.uchar)(unsafe.Pointer(&key[0])), flag))
	if status != 0 {
		clear(key)
		return nil, keychainError(status)
	}
	return key, nil
}

func keychainError(status int32) error {
	var reason string
	switch status {
	case -25300:
		reason = "没有对应的包装密钥（设置可能来自其他用户或设备）"
	case -25308, -25293:
		reason = "钥匙串已锁定或未授权；请先解锁并授权当前应用"
	case -25291:
		reason = "钥匙串当前不可用"
	case -26275:
		reason = "包装密钥格式损坏"
	default:
		reason = "无法读取或保存包装密钥"
	}
	return fmt.Errorf("macOS Keychain：%s（OSStatus %d）", reason, status)
}

func protectedCipher(entropy []byte, create bool) (cipher.AEAD, error) {
	key, err := wrappingKey(entropy, create)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func protectedAAD(entropy []byte) []byte {
	aad := make([]byte, 0, len(protectedHeader)+len(entropy))
	aad = append(aad, protectedHeader...)
	return append(aad, entropy...)
}

// Protect encrypts data with AES-256-GCM using a purpose-specific random key
// kept in the current user's Keychain. A locked or unauthorized Keychain fails
// without opening an authentication dialog or falling back to a plaintext key.
func Protect(data, entropy []byte) ([]byte, error) {
	gcm, err := protectedCipher(entropy, true)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("生成设置加密随机数：%w", err)
	}
	envelope := make([]byte, len(protectedHeader), len(protectedHeader)+len(nonce)+len(data)+gcm.Overhead())
	copy(envelope, protectedHeader)
	envelope = append(envelope, nonce...)
	return gcm.Seal(envelope, nonce, data, protectedAAD(entropy)), nil
}

// Unprotect authenticates the version, purpose and ciphertext before returning
// any plaintext. It never creates a key when the original key is missing.
func Unprotect(data, entropy []byte) ([]byte, error) {
	// Standard GCM uses a 12-byte nonce and 16-byte tag, including empty data.
	if len(data) < len(protectedHeader)+12+16 || !bytes.Equal(data[:len(protectedHeader)], protectedHeader) {
		return nil, errors.New("macOS 加密设置的格式或版本无效")
	}
	gcm, err := protectedCipher(entropy, false)
	if err != nil {
		return nil, err
	}
	body := data[len(protectedHeader):]
	plain, err := gcm.Open(nil, body[:gcm.NonceSize()], body[gcm.NonceSize():], protectedAAD(entropy))
	if err != nil {
		return nil, errors.New("macOS 加密设置校验失败（内容被修改或用途不匹配）")
	}
	return plain, nil
}
