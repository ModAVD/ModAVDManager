//go:build windows

package win

import (
	"bytes"
	"testing"
)

func TestProtectRoundTrip(t *testing.T) {
	secret := []byte("cdk-0123456789")
	entropy := []byte("ModAVD update CDK")
	enc, err := Protect(secret, entropy)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(enc, secret) {
		t.Fatal("the protected blob holds the secret in clear")
	}
	dec, err := Unprotect(enc, entropy)
	if err != nil || !bytes.Equal(dec, secret) {
		t.Fatalf("Unprotect = %q, %v", dec, err)
	}
	if _, err := Unprotect(enc, []byte("other")); err == nil {
		t.Fatal("other entropy decrypted the blob")
	}
	noEntropy, err := Protect(secret, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dec, err := Unprotect(noEntropy, nil); err != nil || !bytes.Equal(dec, secret) {
		t.Fatalf("without entropy: %q %v", dec, err)
	}
	if _, err := Protect(nil, entropy); err == nil {
		t.Fatal("protected nothing")
	}
}
