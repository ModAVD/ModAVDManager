package ui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"modavd/internal/components"
	"modavd/internal/emu"
)

type playStoreInfoBackend struct{ backend }

func (playStoreInfoBackend) RootInfo() emu.RootInfo { return emu.RootInfo{Root: "fixture-root"} }

func TestInfoPublishesPlayStoreCapabilitiesBeforeComponentStatus(t *testing.T) {
	s := &server{mgr: playStoreInfoBackend{}}
	w := httptest.NewRecorder()
	s.handleInfo(w, httptest.NewRequest("GET", "/api/info", nil))
	var got struct {
		Supported *bool `json:"play_store_install_supported"`
		Bridge    *bool `json:"play_store_requires_bridge"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Supported == nil || got.Bridge == nil || *got.Supported != components.PlayStoreInstallSupported() ||
		*got.Bridge != components.PlayStoreRequiresBridge() {
		t.Fatalf("missing or incorrect capabilities: %s", w.Body)
	}
}
