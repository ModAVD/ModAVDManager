package update

import (
	"os"
	"path/filepath"
	"testing"

	"modavd/internal/testutil"
)

func TestRemoveBestEffortReportsFailedReparseRemoval(t *testing.T) {
	p := filepath.Join(testutil.TempDir(t), "link")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldRemove, oldReparse := removeLinkFunc, isReparsePointFunc
	removeLinkFunc = func(string) bool { return false }
	isReparsePointFunc = func(os.FileInfo) bool { return true }
	t.Cleanup(func() { removeLinkFunc, isReparsePointFunc = oldRemove, oldReparse })
	if got := removeBestEffort(p); got {
		t.Fatal("failed reparse removal reported success")
	}
}
