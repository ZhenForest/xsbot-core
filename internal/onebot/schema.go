package onebot

import (
	"embed"
	"encoding/json"
	"fmt"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
	"github.com/ZhenForest/xsbot-core/sdk"
)

//go:embed query_schema.json query_observed.json
var querySchemaFiles embed.FS

type nodeSchema struct {
	Type         string                `json:"type"`
	Fields       map[string]nodeSchema `json:"fields"`
	Element      *nodeSchema           `json:"element"`
	Alternatives []nodeSchema          `json:"alternatives"`
}

var querySchemas = func() map[string]nodeSchema {
	b, e := querySchemaFiles.ReadFile("query_schema.json")
	if e != nil {
		panic(e)
	}
	var schemas map[string]nodeSchema
	if e = json.Unmarshal(b, &schemas); e != nil {
		panic(e)
	}
	b, e = querySchemaFiles.ReadFile("query_observed.json")
	if e != nil {
		panic(e)
	}
	var observed map[string]map[string]nodeSchema
	if e = json.Unmarshal(b, &observed); e != nil {
		panic(e)
	}
	for name, fields := range observed {
		schema, ok := schemas[name]
		if !ok {
			panic("unknown observed schema")
		}
		for key, node := range fields {
			schema.Fields[key] = node
		}
	}
	return schemas
}()

func decodeSchema(value *strictjson.Value, schema nodeSchema) (sdk.Scalar, error) {
	out := sdk.Scalar{State: sdk.Present}
	switch schema.Type {
	case "one-of":
		for _, candidate := range schema.Alternatives {
			if result, e := decodeSchema(value, candidate); e == nil {
				return result, nil
			}
		}
		return out, fmt.Errorf("unrecognized field shape")
	case "empty-array":
		if value.Kind != strictjson.Array || len(value.Array) != 0 {
			return out, fmt.Errorf("only observed empty array recognized")
		}
		out.List = []sdk.Scalar{}
	case "string":
		s, e := value.RequireString()
		if e != nil {
			return out, e
		}
		out.String = &s
	case "integer":
		n, e := integer(value, false)
		if e != nil {
			return out, e
		}
		out.Integer = &n
	case "boolean":
		if value.Kind != strictjson.Boolean {
			return out, fmt.Errorf("expected boolean")
		}
		b := value.Bool
		out.Boolean = &b
	case "object":
		keys := make([]string, 0, len(schema.Fields))
		for k := range schema.Fields {
			keys = append(keys, k)
		}
		fields, e := value.Fields(keys...)
		if e != nil {
			return out, e
		}
		out.Object = map[string]sdk.Scalar{}
		for k, v := range fields {
			child, e := decodeSchema(v, schema.Fields[k])
			if e != nil {
				return out, fmt.Errorf("%s: %w", k, e)
			}
			out.Object[k] = child
		}
	case "array":
		if value.Kind != strictjson.Array {
			return out, fmt.Errorf("expected array")
		}
		out.List = make([]sdk.Scalar, 0, len(value.Array))
		for _, v := range value.Array {
			child, e := decodeSchema(v, *schema.Element)
			if e != nil {
				return out, e
			}
			out.List = append(out.List, child)
		}
	case "opaque-unavailable":
		if value.Kind != strictjson.Null {
			return out, fmt.Errorf("unrecognized non-null upstream field")
		}
		out.State = sdk.Null
	default:
		return out, fmt.Errorf("invalid internal schema")
	}
	return out, nil
}
