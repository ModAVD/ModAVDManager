//go:build linux

package appproxy

import (
	"syscall"
	"testing"
	"unsafe"
)

func TestOrigDst(t *testing.T) {
	sa := []byte{2, 0, 0x1f, 0x90, 93, 184, 216, 34, 0, 0, 0, 0, 0, 0, 0, 0} // AF_INET, port 8080
	b := make([]byte, syscall.CmsgSpace(len(sa)))
	h := (*syscall.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level, h.Type = syscall.IPPROTO_IP, ipRecvOrigDstAddr
	h.SetLen(syscall.CmsgLen(len(sa)))
	copy(b[syscall.CmsgLen(0):], sa)
	if got, ok := origDst(b); !ok || got.String() != "93.184.216.34:8080" {
		t.Errorf("%v %v", got, ok)
	}
	h.Type = 7 // another control message
	if _, ok := origDst(b); ok {
		t.Error("wrong type taken")
	}
	if _, ok := origDst(nil); ok {
		t.Error("nothing taken")
	}
}
