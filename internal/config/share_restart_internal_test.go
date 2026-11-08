package config

import (
	"reflect"
	"testing"
)

// TestShareSettingsRequireRestart pins the SM06-6 ruling: the NFS and SMB
// servers and their cache are built once per generation, so changing any
// setting of theirs needs a restart. A field made hot in clearHotFields fails
// here with its path.
func TestShareSettingsRequireRestart(t *testing.T) {
	t.Parallel()
	for _, section := range []string{"NFS", "SMB", "ShareCache"} {
		sectionType, ok := reflect.TypeFor[Config]().FieldByName(section)
		if !ok {
			t.Fatalf("Config has no %s field", section)
		}
		for _, path := range leafPaths(sectionType.Type, []int{}) {
			index := append([]int{sectionType.Index[0]}, path.index...)
			name := section + "." + path.name
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var before, after Config
				field := reflect.ValueOf(&after).Elem().FieldByIndex(index)
				if !field.CanSet() {
					t.Fatalf("%s is not settable", name)
				}
				setNonZero(t, name, field)
				if reflect.DeepEqual(before, after) {
					t.Fatalf("%s: setting it changed nothing", name)
				}
				if !before.RequiresRestart(&after) {
					t.Errorf("changing %s does not require a restart", name)
				}
			})
		}
	}
}

type leafPath struct {
	name  string
	index []int
}

// leafPaths lists every leaf field of the struct type t: nested structs are
// walked, everything else (scalars, slices, maps, pointers) is a leaf.
func leafPaths(t reflect.Type, prefix []int) []leafPath {
	var out []leafPath
	for i := range t.NumField() {
		f := t.Field(i)
		index := append(append([]int{}, prefix...), i)
		if f.Type.Kind() == reflect.Struct {
			for _, inner := range leafPaths(f.Type, index) {
				out = append(out, leafPath{name: f.Name + "." + inner.name, index: inner.index})
			}
			continue
		}
		out = append(out, leafPath{name: f.Name, index: index})
	}
	return out
}

// setNonZero sets v to a non-zero value of its kind.
func setNonZero(t *testing.T, name string, v reflect.Value) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		v.SetString("x")
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Slice:
		s := reflect.MakeSlice(v.Type(), 1, 1)
		setNonZero(t, name, s.Index(0))
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(v.Type())
		key, val := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
		setNonZero(t, name, key)
		setNonZero(t, name, val)
		m.SetMapIndex(key, val)
		v.Set(m)
	case reflect.Pointer:
		p := reflect.New(v.Type().Elem())
		setNonZero(t, name, p.Elem())
		v.Set(p)
	case reflect.Struct:
		for _, field := range v.Fields() {
			setNonZero(t, name, field)
		}
	case reflect.Invalid, reflect.Uintptr, reflect.Complex64, reflect.Complex128, reflect.Array,
		reflect.Chan, reflect.Func, reflect.Interface, reflect.UnsafePointer:
		t.Fatalf("%s: kind %s is not handled by the test", name, v.Kind())
	}
}
