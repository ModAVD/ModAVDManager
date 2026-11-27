package emu

import (
	"encoding/binary"
	"testing"
)

func TestParseDiscardMaxBytes(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want int64
		ok   bool
	}{
		{"typical", "2147483136\n", 2147483136, true},
		{"zero means no discard", "0\n", 0, true},
		{"padded", "  65536  ", 65536, true},
		{"empty", "", 0, false},
		{"cat error", "cat: /sys/...: No such file or directory", 0, false},
	}
	for _, c := range cases {
		got, ok := parseDiscardMaxBytes(c.out)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("%s: parseDiscardMaxBytes(%q) = (%d, %v), want (%d, %v)",
				c.name, c.out, got, ok, c.want, c.ok)
		}
	}
}

func TestParseQcow2Header(t *testing.T) {
	hdr := func(magic uint32, bits uint32, size uint64) []byte {
		b := make([]byte, 72)
		binary.BigEndian.PutUint32(b[0:], magic)
		binary.BigEndian.PutUint32(b[4:], 3)
		binary.BigEndian.PutUint32(b[20:], bits)
		binary.BigEndian.PutUint64(b[24:], size)
		return b
	}
	got, err := parseQcow2Header(hdr(0x514649fb, 16, 8<<30))
	if err != nil || got.VirtualSize != 8<<30 || got.ClusterSize != 65536 {
		t.Errorf("good header = %+v, %v", got, err)
	}
	for name, b := range map[string][]byte{
		"short":       make([]byte, 31),
		"not qcow2":   hdr(0x12345678, 16, 8<<30),
		"bits tiny":   hdr(0x514649fb, 3, 8<<30),
		"bits huge":   hdr(0x514649fb, 40, 8<<30),
		"absurd size": hdr(0x514649fb, 16, 1<<63),
	} {
		if _, err := parseQcow2Header(b); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestParseQemuImgCheck(t *testing.T) {
	// real output of qemu-img check --output=json on a clean image
	clean := `{
    "image-end-offset": 2136866816,
    "total-clusters": 131072,
    "check-errors": 0,
    "allocated-clusters": 32586,
    "filename": "C:\\avd\\x.avd\\userdata-qemu.img.qcow2",
    "format": "qcow2",
    "fragmented-clusters": 6736
}
`
	c, err := parseQemuImgCheck([]byte(clean))
	if err != nil || *c.AllocatedClusters != 32586 || *c.ImageEndOffset != 2136866816 || c.CheckErrors != 0 {
		t.Errorf("clean = %+v, %v", c, err)
	}
	// a warning line before the object, and a corrupt image
	c, err = parseQemuImgCheck([]byte("qemu-img: warning: x\n{\"check-errors\": 2, \"corruptions\": 2, \"allocated-clusters\": 5}"))
	if err != nil || *c.AllocatedClusters != 5 || c.CheckErrors != 2 || c.Corruptions != 2 || c.ImageEndOffset != nil {
		t.Errorf("corrupt = %+v, %v", c, err)
	}
	// qemu-img omits allocated-clusters for an empty image: a clean decode without it means 0, not an error (R-3).
	c, err = parseQemuImgCheck([]byte(`{"check-errors": 0, "total-clusters": 131072}`))
	if err != nil || c.AllocatedClusters == nil || *c.AllocatedClusters != 0 {
		t.Errorf("no clusters = %+v, %v", c, err)
	}
	for name, in := range map[string]string{
		"empty":     "",
		"no json":   "qemu-img: Could not open",
		"truncated": `{"allocated-clusters": `,
		"negative":  `{"allocated-clusters": -1}`,
	} {
		if _, err := parseQemuImgCheck([]byte(in)); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestReclaimable(t *testing.T) {
	for _, c := range []struct{ onDisk, alloc, want int64 }{
		{10, 4, 6}, {4, 4, 0}, {3, 4, 0}, {0, 0, 0},
	} {
		if got := reclaimable(c.onDisk, c.alloc); got != c.want {
			t.Errorf("reclaimable(%d, %d) = %d, want %d", c.onDisk, c.alloc, got, c.want)
		}
	}
}
