package main

import (
	"strings"
	"testing"
)

// showBoxes replaces the two message boxes for one test and returns what each would have shown.
func showBoxes(t *testing.T) (errors, notices *[]string) {
	t.Helper()
	var e, n []string
	oldFatal, oldNotice := fatal, notice
	fatal = func(msg string) { e = append(e, msg) }
	notice = func(msg string) { n = append(n, msg) }
	t.Cleanup(func() { fatal, notice = oldFatal, oldNotice })
	return &e, &n
}

// ModAVD/ModAVD#115: asking for the usage is not an error.
func TestHelpShowsUsageInInformationBox(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"-help"}, {"--port", "0", "--help"}, {"--no-window", "-h", "--bogus"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			errBoxes, notices := showBoxes(t)
			if code := run(args); code != 0 {
				t.Fatalf("exit code %d, want 0", code)
			}
			if len(*errBoxes) != 0 || len(*notices) != 1 {
				t.Fatalf("error boxes %q, information boxes %q; want one information box", *errBoxes, *notices)
			}
			text := (*notices)[0]
			for _, want := range []string{usage, "--root 根目录：", "--port 端口：", "--no-window：", "--dev："} {
				if !strings.Contains(text, want) {
					t.Errorf("usage box lacks %q:\n%s", want, text)
				}
			}
			for _, bad := range []string{"参数不对", "flag:", "help requested"} {
				if strings.Contains(text, bad) {
					t.Errorf("usage box says %q:\n%s", bad, text)
				}
			}
		})
	}
}

// Everything else that is wrong on the command line stays an error box with exit code 2.
func TestBadArgumentsStillErrors(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--bogus"}, "命令行参数不对：flag provided but not defined: -bogus"},
		{[]string{"--port"}, "命令行参数不对：flag needs an argument: -port"},
		{[]string{"help"}, "多余的参数：help"},
		{[]string{"--port", "70000"}, "端口 70000 不对"},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			errBoxes, notices := showBoxes(t)
			if code := run(c.args); code != 2 {
				t.Fatalf("exit code %d, want 2", code)
			}
			if len(*notices) != 0 || len(*errBoxes) != 1 || !strings.Contains((*errBoxes)[0], c.want) {
				t.Fatalf("error boxes %q, information boxes %q; want one error box with %q", *errBoxes, *notices, c.want)
			}
		})
	}
}
