package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
)

// WebValue renders every int64 as a decimal string before browsers can lose precision.
func WebValue(value any) any { return webValue(reflect.ValueOf(value)) }
func webValue(v reflect.Value) any {
	if !v.IsValid() {
		return nil
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil
		}
		return webValue(v.Elem())
	}
	switch v.Kind() {
	case reflect.Int64:
		return strconv.FormatInt(v.Int(), 10)
	case reflect.Struct:
		out := map[string]any{}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			out[name] = webValue(v.Field(i))
		}
		return out
	case reflect.Map:
		out := map[string]any{}
		iter := v.MapRange()
		for iter.Next() {
			out[fmt.Sprint(iter.Key().Interface())] = webValue(iter.Value())
		}
		return out
	case reflect.Slice, reflect.Array:
		out := []any{}
		for i := 0; i < v.Len(); i++ {
			out = append(out, webValue(v.Index(i)))
		}
		return out
	default:
		return v.Interface()
	}
}
func DecodeWebJSON(data []byte, out any) error {
	tree, e := strictjson.Parse(data)
	if e != nil {
		return e
	}
	value, e := normalizeJSON(tree, reflect.TypeOf(out).Elem())
	if e != nil {
		return e
	}
	normalized, e := json.Marshal(value)
	if e != nil {
		return e
	}
	return json.Unmarshal(normalized, out)
}
func normalizeJSON(v *strictjson.Value, t reflect.Type) (any, error) {
	switch t.Kind() {
	case reflect.Struct:
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			key := strings.Split(f.Tag.Get("json"), ",")[0]
			if key == "-" {
				continue
			}
			if key == "" {
				key = f.Name
			}
			fields[key] = f.Type
		}
		keys := []string{}
		for k := range fields {
			keys = append(keys, k)
		}
		f, e := v.Fields(keys...)
		if e != nil {
			return nil, e
		}
		out := map[string]any{}
		for k, child := range f {
			value, e := normalizeJSON(child, fields[k])
			if e != nil {
				return nil, fmt.Errorf("%s: %w", k, e)
			}
			out[k] = value
		}
		return out, nil
	case reflect.Slice:
		if v.Kind != strictjson.Array {
			return nil, fmt.Errorf("expected array")
		}
		out := []any{}
		for _, child := range v.Array {
			x, e := normalizeJSON(child, t.Elem())
			if e != nil {
				return nil, e
			}
			out = append(out, x)
		}
		return out, nil
	case reflect.Map:
		if v.Kind != strictjson.Object {
			return nil, fmt.Errorf("expected object")
		}
		keys := []string{}
		for _, p := range v.Object {
			keys = append(keys, p.Key)
		}
		f, e := v.Fields(keys...)
		if e != nil {
			return nil, e
		}
		out := map[string]any{}
		for k, child := range f {
			if t.Key().Kind() == reflect.Int64 {
				if _, e := strconv.ParseInt(k, 10, 64); e != nil {
					return nil, e
				}
			}
			x, e := normalizeJSON(child, t.Elem())
			if e != nil {
				return nil, e
			}
			out[k] = x
		}
		return out, nil
	case reflect.Int, reflect.Int64:
		if v.Kind != strictjson.Number && !(t.Kind() == reflect.Int64 && v.Kind == strictjson.String) {
			return nil, fmt.Errorf("expected exact integer")
		}
		if _, e := strconv.ParseInt(v.Text, 10, t.Bits()); e != nil {
			return nil, e
		}
		return json.Number(v.Text), nil
	case reflect.String:
		if v.Kind != strictjson.String {
			return nil, fmt.Errorf("expected string")
		}
		return v.Text, nil
	case reflect.Bool:
		if v.Kind != strictjson.Boolean {
			return nil, fmt.Errorf("expected boolean")
		}
		return v.Bool, nil
	}
	return nil, fmt.Errorf("unsupported web schema")
}
