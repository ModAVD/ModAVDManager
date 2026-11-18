package i18n

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// textKeys hold text for people wherever they appear in a result.
var textKeys = map[string]bool{
	"message": true, "advice": true, "msg": true, "warnings": true, "notes": true, "notices": true, "text": true,
	"summary": true, "problems": true, "instructions": true, "errors": true, "description": true, "help": true,
	"caption": true, "label": true, "hint": true, "privacy_note": true, "not_updatable_reason": true, "error": true,
}

// listTextKeys hold text for people in the elements of textLists arrays (check items, components), where title /
// name / reason are labels rather than the user's own names.
var (
	listTextKeys = map[string]bool{"title": true, "name": true, "reason": true}
	textLists    = map[string]bool{"items": true, "components": true}
)

// machineKeys are never translated, whatever they hold.
var machineKeys = map[string]bool{"details": true, "code": true}

func isTextKey(k string) bool {
	return textKeys[k] || strings.HasSuffix(k, "_error") || strings.HasSuffix(k, "_errors") ||
		strings.HasSuffix(k, "_note") || strings.HasSuffix(k, "_notes")
}

// LocalizeJSON translates the text fields of a JSON value (message, advice, warnings …, see textKeys) into lang. In
// Chinese, or when data is not JSON, it is data itself; otherwise re-encoded (key order may change, numbers kept).
func LocalizeJSON(lang string, data []byte) []byte {
	if lang == Chinese || !hasNonASCII(data) {
		return data
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return data
	}
	v = localizeValue(lang, v, false, false)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return data
	}
	out := buf.Bytes()
	if !bytes.HasSuffix(data, []byte("\n")) {
		out = bytes.TrimSuffix(out, []byte("\n"))
	}
	return out
}

// Localize is a copy of v (a result struct, or a pointer to one) with the text fields LocalizeJSON translates
// translated, for formatting it for people; v itself when lang is Chinese or the copy cannot be made. The copy may
// share slices and pointers with v and change them: only for a result that is printed and then dropped.
func Localize(lang string, v any) any {
	if lang == Chinese || v == nil {
		return v
	}
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	out := LocalizeJSON(lang, data)
	if bytes.Equal(out, data) {
		return v
	}
	t := reflect.TypeOf(v)
	ptr := t.Kind() == reflect.Pointer
	if ptr {
		t = t.Elem()
	}
	cp := reflect.New(t)
	// Fields json leaves out (json:"-", unexported) would be lost: start from the original.
	if ptr {
		if reflect.ValueOf(v).IsNil() {
			return v
		}
		cp.Elem().Set(reflect.ValueOf(v).Elem())
	} else {
		cp.Elem().Set(reflect.ValueOf(v))
	}
	if json.Unmarshal(out, cp.Interface()) != nil {
		return v
	}
	if ptr {
		return cp.Interface()
	}
	return cp.Elem().Interface()
}

func localizeValue(lang string, v any, text, listElem bool) any {
	switch x := v.(type) {
	case string:
		if text {
			return Translate(lang, x)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = localizeValue(lang, e, text, false)
		}
		return x
	case map[string]any:
		for k, e := range x {
			if machineKeys[k] {
				continue
			}
			if arr, ok := e.([]any); ok && textLists[k] {
				for i, el := range arr {
					arr[i] = localizeValue(lang, el, false, true)
				}
				continue
			}
			x[k] = localizeValue(lang, e, isTextKey(k) || listElem && listTextKeys[k], false)
		}
		return x
	}
	return v
}

func hasNonASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 {
			return true
		}
	}
	return false
}
