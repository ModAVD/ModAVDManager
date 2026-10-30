// Command winres writes rsrc_windows_amd64.syso for ModAVD.exe: the icon (RT_ICON images + one RT_GROUP_ICON) and
// the application manifest (RT_MANIFEST), as a COFF object with a single .rsrc section. The Go linker merges a
// package's .syso files into the executable, so the icon shows in Explorer and the taskbar with no extra tool.
//
//	cd tools\modavd\cmd\modavd-gui && go run ./winres        (or: go generate ./cmd/modavd-gui)
//
// The output is deterministic (no timestamps), so the same icon gives the same bytes and release builds stay
// reproducible. Run it again only when modavd.ico or the manifest changes, and commit the .syso.
//
// modavd.ico is fan art and is not covered by the code licence.
package main

import (
	"cmp"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// The manifest: common controls v6 (MessageBoxW gets the current look), per-monitor DPI awareness (sharp text on
// scaled displays), asInvoker (no elevation prompt and no installer heuristics), Windows 10/11 as the supported OS.
const manifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <assemblyIdentity type="win32" name="ModAVD.Manager" version="1.0.0.0" processorArchitecture="amd64"/>
  <description>ModAVD</description>
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
  <trustInfo xmlns="urn:schemas-microsoft-com:asm.v3">
    <security>
      <requestedPrivileges>
        <requestedExecutionLevel level="asInvoker" uiAccess="false"/>
      </requestedPrivileges>
    </security>
  </trustInfo>
  <compatibility xmlns="urn:schemas-microsoft-com:compatibility.v1">
    <application>
      <supportedOS Id="{8e0f7a12-bfb3-4fe8-b9a5-48fd50a15a9a}"/>
    </application>
  </compatibility>
  <application xmlns="urn:schemas-microsoft-com:asm.v3">
    <windowsSettings>
      <dpiAware xmlns="http://schemas.microsoft.com/SMI/2005/WindowsSettings">true/pm</dpiAware>
      <dpiAwareness xmlns="http://schemas.microsoft.com/SMI/2016/WindowsSettings">PerMonitorV2, PerMonitor</dpiAwareness>
    </windowsSettings>
  </application>
</assembly>
`

const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtManifest  = 24
	langEnUS    = 0x0409

	machineAMD64     = 0x8664
	fileLineNumsGone = 0x0004
	scnInitData      = 0x00000040
	scnMemRead       = 0x40000000
	relAMD64Addr32NB = 0x0003 // the target's RVA, without the image base
	symClassStatic   = 3

	dirSize, dirEntrySize, dataEntrySize = 16, 8, 16
	fileHeaderSize, sectionHeaderSize    = 20, 40
	relocSize                            = 10
)

type resource struct {
	typ, id uint16
	data    []byte
}

func main() {
	dir := flag.String("dir", ".", "folder with modavd.ico; the .syso is written there")
	flag.Parse()
	ico, err := os.ReadFile(filepath.Join(*dir, "modavd.ico"))
	if err != nil {
		fail(err)
	}
	res, err := iconResources(ico)
	if err != nil {
		fail(err)
	}
	res = append(res, resource{typ: rtManifest, id: 1, data: []byte(manifest)})
	out := filepath.Join(*dir, "rsrc_windows_amd64.syso")
	obj := coff(res)
	if err := os.WriteFile(out, obj, 0o644); err != nil {
		fail(err)
	}
	fmt.Printf("%s: %d bytes, %d icon images, manifest %d bytes\n", out, len(obj), len(res)-2, len(manifest))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "winres:", err)
	os.Exit(1)
}

// iconResources splits an .ico into RT_ICON resources 1..n and the RT_GROUP_ICON 1 that lists them: each group
// entry is the file's directory entry with the 4-byte image offset replaced by the 2-byte resource id.
func iconResources(ico []byte) ([]resource, error) {
	le := binary.LittleEndian
	if len(ico) < 6 || le.Uint16(ico[0:]) != 0 || le.Uint16(ico[2:]) != 1 {
		return nil, errors.New("modavd.ico is not an icon file")
	}
	n := int(le.Uint16(ico[4:]))
	if n == 0 || len(ico) < 6+16*n {
		return nil, errors.New("modavd.ico has no images")
	}
	group := le.AppendUint16(le.AppendUint16(le.AppendUint16(nil, 0), 1), uint16(n))
	var res []resource
	for k := range n {
		e := ico[6+16*k : 6+16*(k+1)]
		size, off := le.Uint32(e[8:]), le.Uint32(e[12:])
		if size == 0 || uint64(off)+uint64(size) > uint64(len(ico)) {
			return nil, fmt.Errorf("modavd.ico image %d lies outside the file", k)
		}
		id := uint16(k + 1)
		res = append(res, resource{typ: rtIcon, id: id, data: ico[off : off+size]})
		group = le.AppendUint16(append(group, e[:12]...), id) // size, colours, planes, bit count, bytes
	}
	return append(res, resource{typ: rtGroupIcon, id: 1, data: group}), nil
}

// coff lays out the resource tree (type -> id -> language -> data entry), then the data, in one .rsrc section and
// wraps it in an AMD64 COFF object. Each data entry's OffsetToData holds the data's offset in the section plus an
// ADDR32NB relocation against the section symbol; the linker turns that into the RVA Windows expects.
func coff(res []resource) []byte {
	// ascending type, then id: the order Windows' lookups rely on
	res = slices.Clone(res)
	slices.SortFunc(res, func(a, b resource) int { return cmp.Or(cmp.Compare(a.typ, b.typ), cmp.Compare(a.id, b.id)) })
	var types []uint16
	for _, r := range res {
		if len(types) == 0 || types[len(types)-1] != r.typ {
			types = append(types, r.typ)
		}
	}
	count := func(t uint16) int {
		n := 0
		for _, r := range res {
			if r.typ == t {
				n++
			}
		}
		return n
	}

	// offsets: root dir, type dirs, one language dir per resource, data entries, data (8-byte aligned)
	off := dirSize + dirEntrySize*len(types)
	typeDir := map[uint16]int{}
	for _, t := range types {
		typeDir[t] = off
		off += dirSize + dirEntrySize*count(t)
	}
	langDir := make([]int, len(res))
	for k := range res {
		langDir[k] = off
		off += dirSize + dirEntrySize
	}
	dataEntry := make([]int, len(res))
	for k := range res {
		dataEntry[k] = off
		off += dataEntrySize
	}
	dataAt := make([]int, len(res))
	for k, r := range res {
		off = align8(off)
		dataAt[k] = off
		off += len(r.data)
	}
	sec := make([]byte, align8(off))

	le := binary.LittleEndian
	putDir := func(at, entries int) { le.PutUint16(sec[at+14:], uint16(entries)) } // everything else stays 0
	putEntry := func(at int, id uint16, target int, subdir bool) {
		le.PutUint32(sec[at:], uint32(id))
		v := uint32(target)
		if subdir {
			v |= 0x80000000
		}
		le.PutUint32(sec[at+4:], v)
	}
	putDir(0, len(types))
	for k, t := range types {
		putEntry(dirSize+dirEntrySize*k, t, typeDir[t], true)
		putDir(typeDir[t], count(t))
	}
	slot := map[uint16]int{}
	for k, r := range res {
		putEntry(typeDir[r.typ]+dirSize+dirEntrySize*slot[r.typ], r.id, langDir[k], true)
		slot[r.typ]++
		putDir(langDir[k], 1)
		putEntry(langDir[k]+dirSize, langEnUS, dataEntry[k], false)
		le.PutUint32(sec[dataEntry[k]:], uint32(dataAt[k])) // + the section's RVA, by the relocation
		le.PutUint32(sec[dataEntry[k]+4:], uint32(len(r.data)))
		copy(sec[dataAt[k]:], r.data)
	}

	rawAt := fileHeaderSize + sectionHeaderSize
	relocAt := rawAt + len(sec)
	symAt := relocAt + relocSize*len(res)
	var b []byte
	b = le.AppendUint16(b, machineAMD64)
	b = le.AppendUint16(b, 1)             // sections
	b = le.AppendUint32(b, 0)             // timestamp: none, for reproducible builds
	b = le.AppendUint32(b, uint32(symAt)) // symbol table
	b = le.AppendUint32(b, 1)             // symbols
	b = le.AppendUint16(b, 0)             // optional header size
	b = le.AppendUint16(b, fileLineNumsGone)

	b = append(b, ".rsrc\x00\x00\x00"...)
	b = le.AppendUint32(b, 0) // virtual size
	b = le.AppendUint32(b, 0) // virtual address
	b = le.AppendUint32(b, uint32(len(sec)))
	b = le.AppendUint32(b, uint32(rawAt))
	b = le.AppendUint32(b, uint32(relocAt))
	b = le.AppendUint32(b, 0) // line numbers
	b = le.AppendUint16(b, uint16(len(res)))
	b = le.AppendUint16(b, 0)
	b = le.AppendUint32(b, scnInitData|scnMemRead)

	b = append(b, sec...)
	for k := range res {
		b = le.AppendUint32(b, uint32(dataEntry[k])) // the OffsetToData field
		b = le.AppendUint32(b, 0)                    // symbol 0: .rsrc
		b = le.AppendUint16(b, relAMD64Addr32NB)
	}

	// the section symbol, then an empty string table
	b = append(b, ".rsrc\x00\x00\x00"...)
	b = le.AppendUint32(b, 0) // value
	b = le.AppendUint16(b, 1) // section number
	b = le.AppendUint16(b, 0) // type
	b = append(b, symClassStatic, 0)
	return le.AppendUint32(b, 4)
}

func align8(n int) int { return (n + 7) &^ 7 }
