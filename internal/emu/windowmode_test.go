package emu

import (
	"errors"
	"strings"
	"testing"

	"modavd/internal/errs"
)

// TestWindowedFrom: the -headless build of qemu, or -no-window on its command line, is headless; anything else runs
// with the window; an unreadable command line (of a qemu that is not the -headless build) cannot be told.
func TestWindowedFrom(t *testing.T) {
	headlessArgs := []string{`D:\r\sdk\emulator\qemu\windows-x86_64\qemu-system-x86_64-headless.exe`, "-avd", "mas_1", "-no-window"}
	windowArgs := []string{`D:\r\sdk\emulator\qemu\windows-x86_64\qemu-system-x86_64.exe`, "-avd", "mas_1", "-no-snapshot"}
	for _, c := range []struct {
		name            string
		cmdline         []string
		err             error
		windowed, known bool
	}{
		{"qemu-system-x86_64-headless.exe", headlessArgs, nil, false, true},
		// the -headless build alone tells it, whatever the command line says or whether it can be read
		{"qemu-system-x86_64-headless.exe", windowArgs, nil, false, true},
		{"qemu-system-x86_64-headless.exe", nil, errors.New("access denied"), false, true},
		{"QEMU-SYSTEM-X86_64-HEADLESS.EXE", nil, errors.New("access denied"), false, true},
		{"qemu-system-x86_64.exe", windowArgs, nil, true, true},
		{"qemu-system-x86_64", append(windowArgs[:3:3], "-no-window"), nil, false, true}, // Linux: one build
		{"qemu-system-aarch64", windowArgs, nil, true, true},
		{"qemu-system-x86_64.exe", nil, errors.New("access denied"), false, false},
	} {
		w, k := windowedFrom(c.name, c.cmdline, c.err)
		if w != c.windowed || k != c.known {
			t.Errorf("%s %v %v: windowed %v known %v, want %v %v", c.name, c.cmdline, c.err, w, k, c.windowed, c.known)
		}
	}
}

// TestWindowConflict: a start that names the other way than the instance runs is busy and says to shut it down first;
// one that names none, or the same, adopts it.
func TestWindowConflict(t *testing.T) {
	for _, c := range []struct {
		opt      StartOptions
		windowed bool
		want     string // "" = adopted
	}{
		{StartOptions{}, true, ""},
		{StartOptions{}, false, ""},
		{StartOptions{Window: true}, true, ""},
		{StartOptions{Headless: true}, false, ""},
		{StartOptions{Window: true}, false, "正以无头方式运行，运行中不能换成带窗口"},
		{StartOptions{Headless: true}, true, "正带窗口运行，运行中不能换成无头"},
	} {
		err := windowConflict(3, c.opt, c.windowed)
		if c.want == "" {
			if err != nil {
				t.Errorf("%+v on windowed=%v: %v", c.opt, c.windowed, err)
			}
			continue
		}
		e := errs.As(err)
		if errs.DetailsOf(err)[errs.Reason] != "window_mismatch" {
			t.Errorf("details: %v", errs.DetailsOf(err))
		}
		if e == nil || e.Code != errs.Busy || errs.ExitCode(err) != 5 || !strings.Contains(e.Message, "实例 3 "+c.want) ||
			!strings.Contains(e.Advice, "先关机再换：modavd stop 3") {
			t.Errorf("%+v on windowed=%v: %v", c.opt, c.windowed, err)
		}
	}
}
