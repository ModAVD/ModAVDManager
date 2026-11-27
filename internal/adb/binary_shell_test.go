// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package adb

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

func TestBinaryShellRequiresExitStatus(t *testing.T) {
	frame := func(id byte, body []byte) []byte {
		h := make([]byte, 5)
		h[0] = id
		binary.LittleEndian.PutUint32(h[1:], uint32(len(body)))
		return append(h, body...)
	}
	data := []byte{0, 10, 13, 26, 255}
	for _, tc := range []struct {
		name    string
		tail    []byte
		success bool
	}{
		{"success", frame(3, []byte{0}), true}, {"remote failure", frame(3, []byte{1}), false},
		{"disconnected", nil, false}, {"truncated exit", frame(3, []byte{0})[:5], false}, {"truncated stderr", frame(2, []byte{1})[:5], false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			local, remote := net.Pipe()
			defer local.Close()
			go func() { defer remote.Close(); remote.Write(append(frame(1, data), tc.tail...)) }()
			got, err := io.ReadAll(&BinaryShell{conn: local})
			if (err == nil) != tc.success || !bytes.Equal(got, data) {
				t.Fatalf("read %x, err=%v", got, err)
			}
		})
	}
}
