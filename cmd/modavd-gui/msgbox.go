package main

import (
	"modavd/internal/host"
	"modavd/internal/i18n"
)

// fatal shows an error, notice an information such as the usage. A GUI-subsystem process has no console of its own:
// the text goes to the redirected stream (stderr for fatal, stdout for notice) or the parent's console when there is
// one (writeText, ModAVD/ModAVD#258), else into a box. Variables, so that the tests see what would have been shown
// instead of a dialog popping up.
var (
	fatal  = func(msg string) { show(msg, true) }
	notice = func(msg string) { show(msg, false) }
)

// showBox is the fallback of show: an error or information box. A variable for the tests of show.
var showBox = func(msg string, isErr bool) {
	if isErr {
		_ = host.MessageBox(msg)
	} else {
		_ = host.InfoBox(msg)
	}
}

func show(msg string, isErr bool) {
	// the boxes translate the text themselves (internal/win); the stream gets the same translation
	if writeText(i18n.T(msg), isErr) {
		return
	}
	showBox(msg, isErr)
}
