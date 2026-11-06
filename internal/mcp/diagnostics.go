// Copyright 2026 Ji Mao Cai
package mcp

import (
	"encoding/json"
	"slices"
)

// Diagnostic state is exposed only by list_instances. Keep the other tools' existing result fields while the
// CLI and HTTP API reuse the richer core types.
func diagnosticToolResult(name string, data json.RawMessage) (json.RawMessage, error) {
	if name == "list_instances" {
		return data, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, err
	}
	delete(object, "diagnostics")
	delete(object, "diagnostics_since")
	delete(object, "diagnostics_cleanup")
	if raw, ok := object["instance"]; ok {
		var instance map[string]json.RawMessage
		if err := json.Unmarshal(raw, &instance); err != nil {
			return nil, err
		}
		delete(instance, "diagnostics")
		delete(instance, "diagnostics_since")
		encoded, err := encodeJSON(instance)
		if err != nil {
			return nil, err
		}
		object["instance"] = encoded
	}
	return encodeJSON(object)
}

func diagnosticToolSchema(name string, schema map[string]any) {
	if name == "list_instances" {
		return
	}
	objects := []map[string]any{schema}
	props, _ := schema["properties"].(map[string]any)
	if instance, ok := props["instance"].(map[string]any); ok {
		objects = append(objects, instance)
	}
	for _, object := range objects {
		properties, _ := object["properties"].(map[string]any)
		delete(properties, "diagnostics")
		delete(properties, "diagnostics_since")
		delete(properties, "diagnostics_cleanup")
		if required, ok := object["required"].([]string); ok {
			object["required"] = slices.DeleteFunc(required, func(key string) bool {
				return key == "diagnostics" || key == "diagnostics_since" || key == "diagnostics_cleanup"
			})
		}
	}
}
