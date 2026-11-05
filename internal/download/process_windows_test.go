package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"modavd/internal/testutil"
	"modavd/internal/win"
)

func TestDownloadProcessChild(t *testing.T) {
	if os.Getenv("MODAVD_DOWNLOAD_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	d := Client{HTTP: NewHTTPClient(0)}
	size, _ := strconv.ParseInt(os.Getenv("MODAVD_DOWNLOAD_SIZE"), 10, 64)
	if err := d.File(context.Background(), FileOptions{URL: os.Getenv("MODAVD_DOWNLOAD_URL"), Path: os.Getenv("MODAVD_DOWNLOAD_PATH"), Size: size}); err != nil {
		t.Fatal(err)
	}
}

func TestFileResumesAfterProcessKilled(t *testing.T) {
	payload := append(bytes.Repeat([]byte("abcdefgh"), 8192), bytes.Repeat([]byte("zyxwvuts"), 8192)...)
	half := len(payload) / 2
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") == "" {
			w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
			w.Write(payload[:half])
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", half) {
			t.Errorf("resume header %q", r.Header.Get("Range"))
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", half, len(payload)-1, len(payload)))
		w.WriteHeader(206)
		w.Write(payload[half:])
	}))
	defer srv.Close()
	path := filepath.Join(testutil.TempDir(t), "resume.part")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, self, "-test.run=^TestDownloadProcessChild$")
		cmd.Env = append(os.Environ(), "MODAVD_DOWNLOAD_CHILD=1", "MODAVD_DOWNLOAD_URL="+srv.URL, "MODAVD_DOWNLOAD_PATH="+path, "MODAVD_DOWNLOAD_SIZE="+fmt.Sprint(len(payload)))
		win.HideConsoleCmd(cmd)
		return cmd
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := child(ctx)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, err := os.Stat(path); err == nil && st.Size() == int64(half) {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			cmd.Wait()
			t.Fatal("child did not persist the partial file")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("child exited normally instead of being killed")
	}
	if out, err := child(ctx).CombinedOutput(); err != nil {
		t.Fatalf("resume: %v %s", err, strings.TrimSpace(string(out)))
	}
	got, err := os.ReadFile(path)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(payload) {
		t.Fatalf("resumed file differs: %v (%d bytes)", err, len(got))
	}
}
