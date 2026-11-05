package main

import "modavd/internal/win"

// fatal shows an error box, notice an information box (the GUI has no console). Variables, so that the tests see
// what would have been shown instead of a dialog popping up.
var (
	fatal  = func(msg string) { _ = win.MessageBox(msg) }
	notice = func(msg string) { _ = win.InfoBox(msg) }
)
