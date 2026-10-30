//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modavd/internal/logx"
	"modavd/internal/root"
)

func TestSingleInstanceState(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	dir := t.TempDir()
	r, err := root.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	upper, err := root.New(strings.ToUpper(dir))
	if err != nil {
		t.Fatal(err)
	}
	if instanceKey(r) != instanceKey(upper) || !strings.HasPrefix(instanceKey(r), "ui-") || len(instanceKey(r)) != 19 {
		t.Fatalf("keys %q / %q", instanceKey(r), instanceKey(upper))
	}

	first, err := lockInstance(r)
	if err != nil || first == nil {
		t.Fatalf("first lock: %v %v", first, err)
	}
	// the lock belongs to the handle, so a second one in this process is refused like another process would be
	if second, err := lockInstance(upper); err != nil || second != nil {
		t.Fatalf("second lock on the same root: %v %v", second, err)
	}
	if _, err := readState(r); err == nil {
		t.Fatal("state readable before it was written")
	}
	if err := first.writeState("http://127.0.0.1:51234/", "tok"); err != nil {
		t.Fatal(err)
	}
	st, err := readState(upper)
	if err != nil || st.Port != 51234 || st.Token != "tok" || st.URL != "http://127.0.0.1:51234/" || st.PID != os.Getpid() {
		t.Fatalf("state %+v, %v", st, err)
	}
	first.release()
	first.release()
	if _, err := os.Stat(statePath(r)); !os.IsNotExist(err) {
		t.Fatalf("state file left after release: %v", err)
	}
	again, err := lockInstance(r)
	if err != nil || again == nil {
		t.Fatalf("lock after release: %v %v", again, err)
	}
	again.release()
}

func TestLogRotation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "logs")
	l := openLogFile(dir)
	defer l.Close()
	log := logx.New(l.sink(logx.Info))
	log.Debugf("not written")
	big := strings.Repeat("x", 1<<20)
	for k := 0; k < 6; k++ {
		log.Infof("%d %s", k, big)
	}
	log.Warnf("第二行\n第三行")
	cur, err := os.ReadFile(filepath.Join(dir, logFileName))
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(filepath.Join(dir, logFileName+".1"))
	if err != nil || old.Size() < logMaxBytes {
		t.Fatalf("no full .1 backup: %v", err)
	}
	if len(cur) >= logMaxBytes || !strings.Contains(string(cur), "[警告] 第二行\n    第三行\n") ||
		strings.Contains(string(cur), "not written") {
		t.Fatalf("current log (%d bytes) ends %q", len(cur), cur[max(0, len(cur)-60):])
	}
}
