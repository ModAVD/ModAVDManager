// Command mkzip packs the standalone download ModAVDManager-<version>-win-x64.zip from build.ps1's output. Neither
// exe imports it. The archive is reproducible: entries sorted by name, stored (no compression, so no zlib version
// can change the bytes), no directory entries, fixed time and mode.
//
//	go run ./internal/tools/mkzip -dir out -version 0.1.0 -out ModAVDManager-0.1.0-win-x64.zip
//
// LICENSE and NOTICE are the committed bytes (git cat-file at -rev), LICENSES/Go-LICENSE.txt is $(go env GOROOT)\LICENSE
// (the Go standard library is BSD-3-Clause, whose text must go with the binaries), and modavd-manager.json marks the
// standalone download for the updater: version and SHA-256 of every other entry.
package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"modavd/internal/buildinfo"
)

type manifest struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mkzip:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "out", "build.ps1 output directory")
	version := flag.String("version", buildinfo.Version, "manager version")
	out := flag.String("out", "", "zip path (default ModAVDManager-<version>-win-x64.zip)")
	rev := flag.String("rev", "HEAD", "commit that LICENSE and NOTICE are taken from")
	flag.Parse()
	if *out == "" {
		*out = fmt.Sprintf("ModAVDManager-%s-win-x64.zip", *version)
	}

	data := map[string][]byte{}
	for name, path := range map[string]string{
		"ModAVD.exe":       filepath.Join(*dir, "ModAVD.exe"),
		"tools/modavd.exe": filepath.Join(*dir, "tools", "modavd.exe"),
	} {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		data[name] = b
	}
	for _, name := range []string{"LICENSE", "NOTICE"} {
		b, err := output("git", "cat-file", "blob", *rev+":"+name)
		if err != nil {
			return fmt.Errorf("%s at %s: %w", name, *rev, err)
		}
		data[name] = b
	}
	goroot, err := output("go", "env", "GOROOT")
	if err != nil {
		return fmt.Errorf("go env GOROOT: %w", err)
	}
	if data["LICENSES/Go-LICENSE.txt"], err = os.ReadFile(filepath.Join(strings.TrimSpace(string(goroot)), "LICENSE")); err != nil {
		return err
	}

	hashes := make(map[string]string, len(data))
	for name, b := range data {
		hashes[name] = digest(b)
	}
	mb, err := json.Marshal(manifest{Version: *version, Files: hashes})
	if err != nil {
		return err
	}
	data["modavd-manager.json"] = append(mb, '\n')

	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	epoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: epoch}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(data[name]); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if d := filepath.Dir(*out); d != "." {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(*out, buf.Bytes(), 0o644)
}

// output runs a command and returns its stdout; stderr goes into the error.
func output(name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}
