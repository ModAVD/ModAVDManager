//go:build windows

package main

import (
	"strings"
	"testing"
)

func TestRouteFatal(t *testing.T) {
	tests := []struct {
		name          string
		fileType      uint32
		parentConsole bool
		want          fatalRoute
	}{
		{"redirected file", fileTypeDisk, false, routeRedirect},
		{"parent console", fileTypeChar, true, routeConsole},
		{"desktop launch", 0, false, routeDialog},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := routeFatal(tc.fileType, tc.parentConsole); got != tc.want {
				t.Fatalf("routeFatal(%d, %v) = %v, want %v", tc.fileType, tc.parentConsole, got, tc.want)
			}
		})
	}
}

func TestParseArgsExitCodesAndDestinations(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantCode  int
		wantError bool
	}{
		{"help", []string{"--help"}, 0, false},
		{"bad flag", []string{"--bogus"}, 2, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotMessage string
			var gotError bool
			_, code, ok := parseArgs(tc.args, func(message string, stderr bool) {
				gotMessage = message
				gotError = stderr
			})
			if ok || code != tc.wantCode {
				t.Fatalf("parseArgs() = ok %v, code %d, want ok false, code %d", ok, code, tc.wantCode)
			}
			if gotError != tc.wantError {
				t.Fatalf("stderr = %v, want %v", gotError, tc.wantError)
			}
			if gotMessage == "" || (!tc.wantError && !strings.Contains(gotMessage, "ModAVD.exe")) {
				t.Fatalf("message %q does not contain the expected usage text", gotMessage)
			}
		})
	}
}
