package carrier

import (
	"reflect"
	"testing"

	"github.com/iniwex5/vowifi-go/internal/vowifi/policy"
)

func TestRegisterTemplateFromInternalCopiesEveryField(t *testing.T) {
	var source policy.IMSRegisterTemplate
	fillEveryField(reflect.ValueOf(&source).Elem())
	if zero := zeroFields(reflect.ValueOf(RegisterTemplateFromInternal(source))); len(zero) > 0 {
		t.Fatalf("RegisterTemplateFromInternal drops %v", zero)
	}
}

func TestTemplateOverrideToInternalCopiesEveryField(t *testing.T) {
	var source IMSRegisterTemplate
	fillEveryField(reflect.ValueOf(&source).Elem())
	if zero := zeroFields(reflect.ValueOf(templateOverrideToInternal(source))); len(zero) > 0 {
		t.Fatalf("templateOverrideToInternal drops %v", zero)
	}
}

// fillEveryField sets each exported field to a non-zero value, so a
// conversion that drops a field shows up as a zero field on the other side.
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

func zeroFields(value reflect.Value) []string {
	var zero []string
	for i := 0; i < value.NumField(); i++ {
		if value.Type().Field(i).IsExported() && value.Field(i).IsZero() {
			zero = append(zero, value.Type().Field(i).Name)
		}
	}
	return zero
}
