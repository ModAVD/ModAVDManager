// Copyright 2026 jinghero. All rights reserved.

package guest

import (
	_ "embed"
	"path/filepath"
)

// RegionalDex is compiled from regional/Regional.java. Regeneration needs JDK and D8; normal Go builds do not.
//
//go:embed regional/classes.dex
var regionalDex []byte

func LocalRegional(dir string) (string, error) {
	return localCopy(filepath.Join(dir, "regional"), "modavd-regional.dex", regionalDex)
}
