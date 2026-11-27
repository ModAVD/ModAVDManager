//go:build !darwin

package emu

import "context"

func (m *Manager) platformCheckItems() []CheckItem { return nil }

func (m *Manager) graphicsItem(ctx context.Context, useCache bool) CheckItem {
	return vulkanItemFromProbe(m.probeVulkan(ctx, useCache))
}
