// Command mkzip creates the deterministic standalone manager archive.
package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
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
	dir := flag.String("dir", "out", "build output directory")
	version := flag.String("version", "0.1.0", "manager version")
	out := flag.String("out", "", "zip path")
	flag.Parse()
	if *out == "" {
		*out = fmt.Sprintf("ModAVDManager-%s-win-x64.zip", *version)
	}
	goRoot := runtime.GOROOT()
	files := map[string]string{
		"ModAVD.exe":              filepath.Join(*dir, "ModAVD.exe"),
		"tools/modavd.exe":        filepath.Join(*dir, "tools", "modavd.exe"),
		"LICENSE":                 "LICENSE",
		"NOTICE":                  "NOTICE",
		"LICENSES/Go-LICENSE.txt": filepath.Join(goRoot, "LICENSE"),
	}
	data := make(map[string][]byte, len(files)+1)
	hashes := make(map[string]string, len(files))
	for name, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			panic(err)
		}
		data[name] = b
		hashes[name] = digest(b)
	}
	manifestBytes, err := json.Marshal(manifest{Version: *version, Files: hashes})
	if err != nil {
		panic(err)
	}
	data["modavd-manager.json"] = append(manifestBytes, '\n')

	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(*out)), 0o755); err != nil && filepath.Dir(filepath.Clean(*out)) != "." {
		panic(err)
	}
	f, err := os.Create(*out)
	if err != nil {
		panic(err)
	}
	zw := zip.NewWriter(f)
	epoch := time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: epoch}
		h.SetMode(0o644)
		w, err := zw.CreateHeader(h)
		if err != nil {
			panic(err)
		}
		if _, err := io.Copy(w, &byteReader{data: data[name]}); err != nil {
			panic(err)
		}
	}
	if err := zw.Close(); err != nil {
		panic(err)
	}
	if err := f.Close(); err != nil {
		panic(err)
	}
}

type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos == len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
