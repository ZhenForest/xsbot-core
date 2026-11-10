package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

// Configurable is optional. Methods may run concurrently with Deliver and workers.
// Values is a JSON object encoded as a string; secret fields MUST be blank on reads.
// Save must check Revision, validate, persist, then publish a complete snapshot.
type Configurable interface {
	Configuration(context.Context) (Configuration, error)
	SaveConfiguration(context.Context, string, string) error
}

type Configuration struct {
	Revision string                `json:"revision"`
	Values   string                `json:"values"`
	Fields   []ConfigurationField  `json:"fields"`
	Actions  []ConfigurationAction `json:"actions"`
}

type ConfigurationField struct {
	Key    string               `json:"key"`
	Label  string               `json:"label"`
	Kind   string               `json:"kind"` // text, password, textarea, number, checkbox, list
	Hint   string               `json:"hint"`
	Fields []ConfigurationField `json:"fields"` // list item fields
}

type ConfigurationAction struct {
	ID      string               `json:"id"`
	Label   string               `json:"label"`
	Confirm string               `json:"confirm"`
	Fields  []ConfigurationField `json:"fields"`
}

// ConfigurationActions is also optional. Only authenticated management requests
// invoke it; it is never reachable from a chat command.
type ConfigurationActions interface {
	RunConfigurationAction(context.Context, string, string, string) (string, error)
}

// DecodeConfiguration rejects unknown fields, duplicate object keys, invalid
// JSON and trailing values. Size limits belong to the caller.
func DecodeConfiguration(data []byte, out any) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("configuration is not UTF-8")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	var scan func() error
	scan = func() error {
		depth++
		defer func() { depth-- }()
		if depth > 64 {
			return fmt.Errorf("configuration JSON is too deeply nested")
		}
		t, err := d.Token()
		if err != nil {
			return err
		}
		if t == nil {
			return fmt.Errorf("null configuration value")
		}
		if x, ok := t.(json.Delim); ok {
			switch x {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					key := k.(string)
					if seen[key] {
						return fmt.Errorf("duplicate configuration key")
					}
					seen[key] = true
					if e = scan(); e != nil {
						return e
					}
				}
			case '[':
				for d.More() {
					if e := scan(); e != nil {
						return e
					}
				}
			default:
				return fmt.Errorf("invalid JSON delimiter")
			}
			_, err = d.Token()
		}
		return err
	}
	if err := scan(); err != nil {
		return fmt.Errorf("invalid configuration JSON")
	}
	if _, err := d.Token(); err != io.EOF {
		return fmt.Errorf("trailing configuration JSON")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return fmt.Errorf("configuration fields or types are invalid")
	}
	return nil
}
