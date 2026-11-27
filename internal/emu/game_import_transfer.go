// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
package emu

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path"
	"strings"
	"time"

	"modavd/internal/adb"
	"modavd/internal/errs"
)

type importProgress struct {
	pkg         string
	done, total int64
	last        time.Time
	report      func(Progress)
}

func (p *importProgress) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if time.Since(p.last) >= time.Second {
		p.update()
	}
	return len(b), nil
}

func (p *importProgress) update() {
	if p.report != nil {
		p.report(Progress{Step: "copy", Message: fmt.Sprintf("正在导入 %s（%d / %d MiB）", p.pkg, p.done>>20, p.total>>20), Done: int(1000 * p.done / max(1, p.total)), Total: 1000})
	}
	p.last = time.Now()
}

// Use shell v2 without a PTY so binary data and each guest command's exit status arrive separately.
func importStream(ctx context.Context, src, dst importDevice, read, write string, copyData func(io.Writer, io.Reader) error) error {
	from, err := adb.OpenBinaryShell(ctx, src.m.Root.ScriptAdbServerPort(), src.serial, src.script(read))
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := adb.OpenBinaryShell(ctx, dst.m.Root.ScriptAdbServerPort(), dst.serial, dst.script(write))
	if err != nil {
		return err
	}
	defer to.Close()
	// Drain diagnostics while writing. A full stderr pipe must not stall a large failed extraction.
	result := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, to)
		to.Close()
		result <- err
	}()
	err = copyData(to, from)
	if err == nil {
		err = to.CloseInput()
	}
	if err != nil {
		to.Close()
	}
	remoteErr := <-result
	if remoteErr != nil && err == nil {
		return remoteErr
	}
	if err != nil {
		return fmt.Errorf("%w (ModAVD: %v)", err, remoteErr)
	}
	return nil
}

// Forward only ordinary entries below the selected name. Discard ownership, links and extended metadata.
func copyImportTar(w io.Writer, r io.Reader, name string, p *importProgress) error {
	in := tar.NewReader(r)
	out := tar.NewWriter(w)
	seen := false
	for {
		h, err := in.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		rel := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
		if !resourcePath(rel) || (rel != name && !strings.HasPrefix(rel, name+"/")) {
			return fmt.Errorf("tar: %q", h.Name)
		}
		mode := int64(0660)
		switch h.Typeflag {
		case tar.TypeDir:
			mode = 0770
		case tar.TypeReg:
		default:
			return fmt.Errorf("tar: %q (type %d)", rel, h.Typeflag)
		}
		if err := out.WriteHeader(&tar.Header{Name: rel, Size: h.Size, Mode: mode, Typeflag: h.Typeflag}); err != nil {
			return err
		}
		if _, err = io.Copy(io.MultiWriter(out, p), in); err != nil {
			return err
		}
		seen = true
	}
	if !seen {
		return io.ErrUnexpectedEOF
	}
	// Some tar implementations pad the last block. Drain it before waiting for adb.
	if _, err := io.Copy(io.Discard, r); err != nil {
		return err
	}
	return out.Close()
}

// Android and the download-disk watcher can create a new package directory before
// its final ownership and access mode are visible through shared storage. Retry
// only the mkdir/mktemp step, before any file has been transferred.
func (d importDevice) prepareImportDir(ctx context.Context, cmd string) (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := d.shell(ctx, cmd)
		var exit *exec.ExitError
		if err == nil || d.identity == "" || !errors.As(err, &exit) || !strings.Contains(err.Error(), "Permission denied") || time.Now().After(deadline) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return "", ctxErr(ctx)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func (d importDevice) stage(ctx context.Context, parent string) (string, func(), error) {
	out, err := d.prepareImportDir(ctx, "mkdir -p "+importQuote(parent)+" && mktemp -d "+importQuote(parent+"/.modavd-import.XXXXXXXX"))
	dir := strings.TrimSpace(out)
	if err != nil {
		return "", nil, err
	}
	if path.Dir(dir) != parent || !strings.HasPrefix(path.Base(dir), ".modavd-import.") || !resourcePath(path.Base(dir)) {
		return "", nil, fmt.Errorf("mktemp: %q", dir)
	}
	return dir, func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := d.shell(cleanup, "rm -rf "+importQuote(dir)); err != nil {
			d.m.Log.Warnf("清理导入临时目录失败：%v", err)
		}
	}, nil
}

// An existing directory may already hold settings or completed items from an
// earlier attempt. Add missing items atomically and compare collisions byte for byte.
const publishResources = `publish() {
 [ ! -L "$2" ] || return 1
 if [ ! -e "$2" ]; then
   # A game download folder may be a separate mount. mv would fall back to a
   # non-atomic copy directly into the final name; stage inside that mount instead.
   srcdev=$(stat -c %d "$1") && dstdev=$(stat -c %d "${2%/*}") || return 1
   if [ "$srcdev" != "$dstdev" ]; then
     need=$(du -sk "$1") && avail=$(df -Pk "${2%/*}") || return 1
     avail=$(echo "$avail" | tail -1 | awk '{print $4}')
     [ "${need%%[[:space:]]*}" -le "$avail" ] || { echo "$2" >&2; return 74; }
     (tmp=$(mktemp -d "${2%/*}/.modavd-import.XXXXXXXX") || exit 1
      trap 'rm -rf "$tmp"' EXIT
      cp -R "$1" "$tmp/item" && publish "$tmp/item" "$2")
     return $?
   fi
   mv -nT "$1" "$2" || return 1
   [ -e "$1" ] || return 0
 fi
 if [ -d "$1" ] && [ -d "$2" ]; then
   for child in "$1"/* "$1"/.[!.]* "$1"/..?*; do
     [ -e "$child" ] || continue
     publish "$child" "$2/${child##*/}" || return $?
   done
 elif [ -f "$1" ] && [ -f "$2" ] && cmp -s "$1" "$2"; then
   return 0
 else
   echo "$2" >&2
   return 73
 fi
}`

// importStreamFunc is replaced in tests.
var importStreamFunc = importStream

// copyResource transfers one selected resource into a staging directory next to its destination and publishes it.
// want is the size counted before the transfer: an item whose transferred bytes differ is not published.
func copyResource(ctx context.Context, src, dst importDevice, full string, want int64, p *importProgress) error {
	parent, name := path.Split(full)
	parent = strings.TrimSuffix(parent, "/")
	// pkgRoot is exactly /sdcard/Android/{data,obb}/<validated package>.
	parts := strings.SplitN(strings.TrimPrefix(full, "/sdcard/Android/"), "/", 3)
	pkgRoot := "/sdcard/Android/" + parts[0] + "/" + parts[1]
	// The installer may not have created the package directory. Give mkdir the
	// shared-storage group for that parent only. Extraction runs as the app UID.
	mkdir := dst
	if parts[0] == "obb" {
		mkdir.identity += ",1079"
	} else {
		mkdir.identity += ",1078"
	}
	base := importQuote(pkgRoot)
	if _, err := mkdir.prepareImportDir(ctx, "[ ! -L "+base+" ] && mkdir -p "+base); err != nil {
		return err
	}
	// Refuse links in an existing target's ancestors before creating the next level.
	for prefix := parent; prefix != pkgRoot; prefix = path.Dir(prefix) {
		if _, err := dst.shell(ctx, "[ ! -L "+importQuote(prefix)+" ]"); err != nil {
			return err
		}
	}
	dir, cleanup, err := dst.stage(ctx, parent)
	if err != nil {
		return err
	}
	defer cleanup()
	read := importPublicCD(parent) + " && tar -cf - " + importQuote("./"+name)
	write := "tar -xomf - -C " + importQuote(dir)
	before := p.done
	if err := importStreamFunc(ctx, src, dst, read, write, func(w io.Writer, r io.Reader) error { return copyImportTar(w, r, name, p) }); err != nil {
		return err
	}
	// A source that changed while it was copied must not reach the final directory: a retry would find it there
	// with other content and stop at already_exists. The deferred cleanup removes only this item's staging directory.
	if got := p.done - before; got != want {
		return errs.New(errs.Internal, "源文件大小在导入期间发生变化，请退出游戏后重试")
	}
	// Stage and destination share a filesystem, so each newly published file arrives whole.
	staged, final := importQuote(path.Join(dir, name)), importQuote(full)
	_, err = dst.shell(ctx, publishResources+"\npublish "+staged+" "+final)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 73:
			return errs.Newf(errs.AlreadyExists, "目标已有不同内容，请另选实例或资源：%v", err)
		case 74:
			return errs.Newf(errs.PrecheckFailed, "目标资源目录空间不足：%v", err)
		}
	}
	return err
}
