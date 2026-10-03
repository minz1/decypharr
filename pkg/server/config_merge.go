package server

import (
	"encoding/json"
	"reflect"
	"strings"
)

// The JSON fields that identify an item in a config list, in order of
// preference: debrids, arrs and folders have a name, usenet providers a host.
const (
	itemNameKey = "name"
	itemHostKey = "host"
)

// prepareSuppliedSlices readies the struct dst for [json.Unmarshal] of body,
// which decodes an array into a slice's existing elements by index. A list
// of keyed items (see itemKey) is reordered so each supplied item lands on
// the current item with the same key, and a new key on a zero item: fields
// the item omits keep that item's values, never the values of whichever
// item used to sit at its index. Any other supplied slice is cleared, so
// the array replaces it.
func prepareSuppliedSlices(dst reflect.Value, body []byte) error {
	if !isJSON(body, '{') {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return err
	}
	for key, raw := range fields {
		field, ok := jsonField(dst, key)
		if !ok {
			continue
		}
		switch {
		case field.Kind() == reflect.Slice:
			if err := prepareSlice(field, raw); err != nil {
				return err
			}
		case field.Kind() == reflect.Struct:
			if err := prepareSuppliedSlices(field, raw); err != nil {
				return err
			}
		case field.Kind() == reflect.Pointer && !field.IsNil() && field.Elem().Kind() == reflect.Struct:
			if err := prepareSuppliedSlices(field.Elem(), raw); err != nil {
				return err
			}
		}
	}
	return nil
}

func prepareSlice(field reflect.Value, raw json.RawMessage) error {
	key, keyed := itemKey(field.Type().Elem())
	if !keyed || !isJSON(raw, '[') {
		field.SetZero()
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	next := reflect.MakeSlice(field.Type(), len(items), len(items))
	used := make([]bool, field.Len())
	for i, item := range items {
		if j := matchByKey(field, key, item, used); j >= 0 {
			next.Index(i).Set(field.Index(j))
			used[j] = true
		}
		if err := prepareSuppliedSlices(next.Index(i), item); err != nil {
			return err
		}
	}
	field.Set(next)
	return nil
}

// itemKey returns the identifying field that list items of type t have.
func itemKey(t reflect.Type) (string, bool) {
	if t.Kind() != reflect.Struct {
		return "", false
	}
	for _, key := range [...]string{itemNameKey, itemHostKey} {
		if field, ok := jsonField(reflect.New(t).Elem(), key); ok && field.Kind() == reflect.String {
			return key, true
		}
	}
	return "", false
}

// matchByKey returns the index of the first unused element of current whose
// key field equals item's, or -1.
func matchByKey(current reflect.Value, key string, item json.RawMessage, used []bool) int {
	if !isJSON(item, '{') {
		return -1
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(item, &fields) != nil {
		return -1
	}
	var want string
	for name, raw := range fields {
		if strings.EqualFold(name, key) && json.Unmarshal(raw, &want) == nil {
			break
		}
	}
	if want == "" {
		return -1
	}
	for j := range current.Len() {
		if field, _ := jsonField(current.Index(j), key); !used[j] && field.String() == want {
			return j
		}
	}
	return -1
}

func isJSON(body []byte, open byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return trimmed != "" && trimmed[0] == open
}

// jsonField finds the settable field of struct v that encoding/json decodes
// key into: an exact name match first, then a case-insensitive one, looking
// through embedded structs.
func jsonField(v reflect.Value, key string) (reflect.Value, bool) {
	var folded reflect.Value
	found := false
	for i := range v.NumField() {
		sf := v.Type().Field(i)
		name, skip := jsonName(sf)
		if skip {
			continue
		}
		if sf.Anonymous && name == "" && v.Field(i).Kind() == reflect.Struct {
			if field, ok := jsonField(v.Field(i), key); ok {
				return field, true
			}
			continue
		}
		if !sf.IsExported() {
			continue
		}
		if name == "" {
			name = sf.Name
		}
		if name == key {
			return v.Field(i), true
		}
		if !found && strings.EqualFold(name, key) {
			folded, found = v.Field(i), true
		}
	}
	return folded, found
}

// jsonName returns the field's JSON tag name, and whether encoding/json
// ignores the field.
func jsonName(sf reflect.StructField) (string, bool) {
	tag := sf.Tag.Get("json")
	if tag == "-" {
		return "", true
	}
	name, _, _ := strings.Cut(tag, ",")
	return name, false
}
