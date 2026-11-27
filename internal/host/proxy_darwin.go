//go:build darwin && cgo

package host

import (
	"context"
	"modavd/internal/macos"
)

func SystemProxy(ctx context.Context) (map[string]string, []string, error) {
	return macos.SystemProxy(ctx)
}
