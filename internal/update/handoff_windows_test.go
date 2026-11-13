//go:build windows

// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later

package update

import (
	"os"
	"testing"
	"time"

	"modavd/internal/root"
)

func TestHandoffLockedResultCanBeRetried(t *testing.T) {
	r := &root.Root{Dir: t.TempDir()}
	if err := SaveHandoff(r, NewHandoffResult(testHandoffID, nil, nil, time.Now())); err != nil {
		t.Fatal(err)
	}
	// os.Open does not share DELETE on Windows. A result that cannot be removed must not be reported yet.
	f, err := os.Open(handoffPath(r, testHandoffID))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if res := TakeHandoff(r, testHandoffID, time.Now()); res != nil {
		t.Fatalf("reported an unremoved result: %+v", res)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if res := TakeHandoff(r, testHandoffID, time.Now()); res == nil {
		t.Fatal("could not retry the result after unlocking it")
	}
}
