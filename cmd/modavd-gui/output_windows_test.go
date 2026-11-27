package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"modavd/internal/update"
)

func TestRouteText(t *testing.T) {
	for _, c := range []struct {
		name          string
		fileType      uint32
		isConsole     bool
		parentConsole bool
		want          textRoute
	}{
		{"file", syscall.FILE_TYPE_DISK, false, true, routeStream},
		{"pipe", syscall.FILE_TYPE_PIPE, false, false, routeStream},
		{"NUL", syscall.FILE_TYPE_CHAR, false, true, routeStream},
		{"console handle", syscall.FILE_TYPE_CHAR, true, true, routeConsole},
		{"terminal, not redirected", 0, false, true, routeConsole},
		{"double click", 0, false, false, routeBox},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := routeText(c.fileType, c.isConsole, c.parentConsole); got != c.want {
				t.Fatalf("routeText(%d, %v, %v) = %v, want %v", c.fileType, c.isConsole, c.parentConsole, got, c.want)
			}
		})
	}
}

// redirect points stdout and stderr at files for one test and fails it if a box would have been shown.
func redirect(t *testing.T) (stdout, stderr func() string) {
	t.Helper()
	dir := t.TempDir()
	open := func(name string) *os.File {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	out, errf := open("stdout.txt"), open("stderr.txt")
	oldOut, oldErr, oldBox := os.Stdout, os.Stderr, showBox
	os.Stdout, os.Stderr = out, errf
	showBox = func(msg string, isErr bool) { t.Errorf("box shown (isErr %v): %q", isErr, msg) }
	t.Cleanup(func() { os.Stdout, os.Stderr, showBox = oldOut, oldErr, oldBox })
	read := func(f *os.File) func() string {
		return func() string {
			b, err := os.ReadFile(f.Name())
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	return read(out), read(errf)
}

// With stdout / stderr redirected, the manager prints the usage and the command line errors there and shows no box
// (ModAVD/ModAVD#258); the exit codes stay 0 and 2.
func TestRunWritesToRedirectedStreams(t *testing.T) {
	for _, c := range []struct {
		args     []string
		code     int
		toStderr bool
		want     string
	}{
		{[]string{"--help"}, 0, false, "--no-window："},
		{[]string{"--bogus"}, 2, true, "flag provided but not defined: -bogus"},
		{[]string{"--port", "70000"}, 2, true, "端口 70000 不对"},
		{[]string{update.HandoffArg, "bad"}, 2, true, "更新交接 ID 必须是 32 位小写十六进制字符"},
		{[]string{"--chrome-window"}, 2, true, "flag provided but not defined: -chrome-window"},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			stdout, stderr := redirect(t)
			if code := run(c.args); code != c.code {
				t.Fatalf("exit code %d, want %d", code, c.code)
			}
			got, other := stdout(), stderr()
			if c.toStderr {
				got, other = other, got
			}
			if !strings.Contains(got, c.want) || other != "" {
				t.Fatalf("written %q, other stream %q; want %q in the %s stream only", got, other, c.want,
					map[bool]string{true: "error", false: "output"}[c.toStderr])
			}
			if !strings.HasSuffix(got, "\r\n") {
				t.Errorf("%q does not end with a line break", got)
			}
		})
	}
}
