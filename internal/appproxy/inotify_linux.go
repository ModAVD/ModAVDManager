//go:build linux

package appproxy

import (
	"bytes"
	"encoding/binary"
	"path/filepath"
	"syscall"
)

// watchPackages signals ch (without blocking) whenever packages.list may have changed, so that an app installed
// while listed gets its rules before it can be opened.
func watchPackages(ch chan<- struct{}) error {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return err
	}
	dir, name := filepath.Split(PackagesList)
	if _, err := syscall.InotifyAddWatch(fd, dir, syscall.IN_MOVED_TO|syscall.IN_CLOSE_WRITE|syscall.IN_CREATE); err != nil {
		syscall.Close(fd)
		return err
	}
	go func() {
		buf := make([]byte, 16<<10)
		for {
			n, err := syscall.Read(fd, buf)
			if err == syscall.EINTR {
				continue
			}
			if err != nil || n <= 0 {
				return
			}
			// struct inotify_event: wd, mask, cookie, len (each 4 bytes), then len bytes of NUL-padded name
			for off := 0; off+16 <= n; {
				l := int(binary.LittleEndian.Uint32(buf[off+12:]))
				if off+16+l > n {
					break
				}
				if string(bytes.TrimRight(buf[off+16:off+16+l], "\x00")) == name {
					select {
					case ch <- struct{}{}:
					default:
					}
				}
				off += 16 + l
			}
		}
	}()
	return nil
}
