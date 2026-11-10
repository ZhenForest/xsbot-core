// Package strictjson preserves duplicate fields until the appropriate message boundary is checked.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

type Kind uint8

const (
	Object Kind = iota
	Array
	String
	Number
	Boolean
	Null
)

type Pair struct {
	Key   string
	Value *Value
}
type Value struct {
	Kind   Kind
	Object []Pair
	Array  []*Value
	Text   string
	Bool   bool
}

func Parse(data []byte) (*Value, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	v, err := parse(d, 0)
	if err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("trailing JSON value")
		}
		return nil, err
	}
	return v, nil
}
func parse(d *json.Decoder, depth int) (*Value, error) {
	if depth > 128 {
		return nil, fmt.Errorf("JSON nesting too deep")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch x := t.(type) {
	case json.Delim:
		v := &Value{}
		switch x {
		case '{':
			v.Kind = Object
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, err
				}
				s, ok := k.(string)
				if !ok {
					return nil, fmt.Errorf("invalid object key")
				}
				child, err := parse(d, depth+1)
				if err != nil {
					return nil, err
				}
				v.Object = append(v.Object, Pair{s, child})
			}
		case '[':
			v.Kind = Array
			for d.More() {
				child, err := parse(d, depth+1)
				if err != nil {
					return nil, err
				}
				v.Array = append(v.Array, child)
			}
		default:
			return nil, fmt.Errorf("unexpected delimiter")
		}
		_, err := d.Token()
		return v, err
	case string:
		return &Value{Kind: String, Text: x}, nil
	case json.Number:
		return &Value{Kind: Number, Text: x.String()}, nil
	case bool:
		return &Value{Kind: Boolean, Bool: x}, nil
	case nil:
		return &Value{Kind: Null}, nil
	}
	return nil, fmt.Errorf("invalid JSON token")
}

// Fields validates only this object. Nested reply objects can be checked independently.
func (v *Value) Fields(allowed ...string) (map[string]*Value, error) {
	if v == nil || v.Kind != Object {
		return nil, fmt.Errorf("expected object")
	}
	known := make(map[string]bool, len(allowed))
	for _, k := range allowed {
		known[k] = true
	}
	result := make(map[string]*Value, len(v.Object))
	for _, p := range v.Object {
		if _, exists := result[p.Key]; exists {
			return nil, fmt.Errorf("duplicate field %q", p.Key)
		}
		if !known[p.Key] {
			return nil, fmt.Errorf("unknown field %q", p.Key)
		}
		result[p.Key] = p.Value
	}
	return result, nil
}
func (v *Value) Get(key string) *Value {
	if v != nil {
		for _, p := range v.Object {
			if p.Key == key {
				return p.Value
			}
		}
	}
	return nil
}
func (v *Value) RequireString() (string, error) {
	if v == nil || v.Kind != String {
		return "", fmt.Errorf("expected string")
	}
	return v.Text, nil
}
