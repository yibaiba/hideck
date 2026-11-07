package policy

import (
	"reflect"
	"testing"
)

func TestCloneCarrierOverrideCopiesEveryPointer(t *testing.T) {
	var source CarrierOverride
	fillEveryField(reflect.ValueOf(&source).Elem())
	clone := cloneCarrierOverride(source)
	assertPointersCloned(t, "CarrierOverride", reflect.ValueOf(source), reflect.ValueOf(clone))
}

// assertPointersCloned walks nested structs so a pointer at any depth is checked.
func assertPointersCloned(t *testing.T, owner string, source, clone reflect.Value) {
	t.Helper()
	for i := 0; i < source.NumField(); i++ {
		field := source.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		name := owner + "." + field.Name
		switch field.Type.Kind() {
		case reflect.Pointer:
			if source.Field(i).Pointer() == clone.Field(i).Pointer() {
				t.Errorf("%s still shares the source pointer", name)
			}
		case reflect.Struct:
			assertPointersCloned(t, name, source.Field(i), clone.Field(i))
		}
	}
}

// fillEveryField sets each exported field to a non-zero value, so every
// pointer is non-nil and the clone check compares real addresses.
func fillEveryField(value reflect.Value) {
	switch value.Kind() {
	case reflect.String:
		value.SetString("x")
	case reflect.Bool:
		value.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value.SetInt(7)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		value.SetUint(7)
	case reflect.Slice:
		slice := reflect.MakeSlice(value.Type(), 1, 1)
		fillEveryField(slice.Index(0))
		value.Set(slice)
	case reflect.Pointer:
		pointer := reflect.New(value.Type().Elem())
		fillEveryField(pointer.Elem())
		value.Set(pointer)
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				fillEveryField(value.Field(i))
			}
		}
	}
}
