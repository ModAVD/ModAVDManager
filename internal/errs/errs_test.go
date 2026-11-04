package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestWrapKeepsUnsupported(t *testing.T) {
	u := New(Unsupported, "x")
	if got := Wrap(Internal, u, "wrapped").Code; got != Unsupported {
		t.Fatalf("got %s", got)
	}
	if got := Wrap(Internal, Wrap(Internal, u, "inner"), "outer").Code; got != Unsupported {
		t.Fatalf("nested got %s", got)
	}
	if got := Wrap(Internal, errors.New("plain"), "wrapped").Code; got != Internal {
		t.Fatalf("plain got %s", got)
	}
}

// TestDetails: WithDetail copies (the original keeps its details), DetailsOf finds the details of wrapped errors
// through Wrap and fmt's %w, the outer value winning, and is nil without any.
func TestDetails(t *testing.T) {
	base := New(Timeout, "启动超时").WithAdvice("再试一次")
	withDir := WithDetail(base, BootDiagDir, `D:\r\logs\mas_6-x-bootfail`)
	if base.Details != nil || withDir == base || withDir.Code != Timeout || withDir.Advice != "再试一次" {
		t.Fatalf("WithDetail changed the original or lost fields: %+v / %+v", base, withDir)
	}
	again := WithDetail(withDir, "other", "1")
	if len(withDir.Details) != 1 || len(again.Details) != 2 {
		t.Fatalf("WithDetail shares the map: %v / %v", withDir.Details, again.Details)
	}

	wrapped := Wrap(Timeout, fmt.Errorf("start: %w", withDir), "实例 6 新建后的初始化 20 分钟没做完")
	if got := DetailsOf(wrapped)[BootDiagDir]; got != `D:\r\logs\mas_6-x-bootfail` {
		t.Errorf("through Wrap and %%w: %q", got)
	}
	outer := WithDetail(wrapped, BootDiagDir, "outer")
	if got := DetailsOf(outer)[BootDiagDir]; got != "outer" {
		t.Errorf("outer value: %q", got)
	}
	if DetailsOf(Wrap(Internal, errors.New("plain"), "x")) != nil || DetailsOf(nil) != nil {
		t.Error("details without any")
	}
	if got := WithDetail(errors.New("plain"), "k", "v"); got.Code != Internal || got.Details["k"] != "v" {
		t.Errorf("WithDetail of a plain error: %+v", got)
	}
}
