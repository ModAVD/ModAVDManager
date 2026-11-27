//go:build !windows

package report

import "testing"

// setHidden: only Windows has the attribute; the scenarios that need it run on Windows only.
func setHidden(*testing.T, string) {}

// shortName: only Windows has 8.3 names; the scenarios that need them are skipped elsewhere.
func shortName(*testing.T, string) string { return "" }
