//go:build darwin && cgo

package update

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"

	"modavd/internal/jsonord"
	"modavd/internal/root"
)

// Persist an undo record before the metadata write. A crash after that write but
// before the idle journal must not leave the attempted version after rollback.
type nativeMetadataUndo struct {
	ID            string          `json:"id"`
	HadComponents bool            `json:"had_components"`
	HadHistory    bool            `json:"had_history"`
	Components    json.RawMessage `json:"components"`
	History       json.RawMessage `json:"history"`
}

func nativeLastTransaction(o *jsonord.Obj) string {
	h := o.Get("updateHistory").Items()
	if len(h) == 0 || h[len(h)-1].Kind() != jsonord.Object {
		return ""
	}
	id, _ := h[len(h)-1].Obj().Get("transaction").Str()
	return id
}

func (u *Updater) recordNativeMetadata(st *nativeState) error {
	return u.Root.EditMetadata(func(o *jsonord.Obj) (bool, error) {
		if st.Metadata != nil && nativeLastTransaction(o) == st.Metadata.ID {
			return false, nil
		}
		if st.Metadata == nil {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return false, err
			}
			st.Metadata = &nativeMetadataUndo{ID: hex.EncodeToString(id[:]),
				HadComponents: o.Has("components"), HadHistory: o.Has("updateHistory"),
				Components: o.Get("components").MarshalPython(), History: o.Get("updateHistory").MarshalPython()}
			if err := u.writeNativeState(st); err != nil {
				return false, err
			}
		}
		var history []*jsonord.Value
		for _, item := range o.Get("updateHistory").Items() {
			if item.Kind() == jsonord.Object {
				history = append(history, item)
			}
		}
		entry := jsonord.NewObject()
		for key, value := range map[string]string{"action": "update", "at": root.NowISO(), "from": st.From,
			"to": st.Target.Version, "source": st.Target.Source, "asset": st.Target.Asset, "transaction": st.Metadata.ID} {
			entry.Obj().Set(key, jsonord.NewString(value))
		}
		history = append(history, entry)
		if len(history) > 20 {
			history = history[len(history)-20:]
		}
		components := o.Get("components").Clone()
		if components.Kind() != jsonord.Object {
			components = jsonord.NewObject()
		}
		components.Obj().Set("emulator", jsonord.NewString(st.Target.Version))
		o.Set("components", components)
		o.Set("updateHistory", jsonord.NewArray(history...))
		return true, nil
	})
}

func (u *Updater) restoreNativeMetadata(st *nativeState) error {
	if st.Metadata == nil {
		return nil
	}
	undo := st.Metadata
	components, err := jsonord.Parse(undo.Components)
	if err != nil {
		return err
	}
	history, err := jsonord.Parse(undo.History)
	if err != nil {
		return err
	}
	return u.Root.EditMetadata(func(o *jsonord.Obj) (bool, error) {
		// Other updater histories are protected by the root update lock. If an
		// external writer changed the last transaction, preserve its changes.
		if nativeLastTransaction(o) != undo.ID {
			return false, nil
		}
		if undo.HadHistory {
			o.Set("updateHistory", history)
		} else {
			o.Delete("updateHistory")
		}
		current := o.Get("components")
		if current.Kind() == jsonord.Object {
			value, _ := current.Obj().Get("emulator").Str()
			if value == st.Target.Version {
				current = current.Clone()
				if components.Kind() == jsonord.Object && components.Obj().Has("emulator") {
					current.Obj().Set("emulator", components.Obj().Get("emulator"))
				} else {
					current.Obj().Delete("emulator")
				}
				o.Set("components", current)
				if current.Obj().Len() == 0 && components.Kind() != jsonord.Object {
					if undo.HadComponents {
						o.Set("components", components)
					} else {
						o.Delete("components")
					}
				}
			}
		}
		return true, nil
	})
}
