package provider

import (
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

// toJSON converts a property value to its JSON form; secrets are unwrapped, and unknown
// (computed) or non-JSON values are rejected.
func toJSON(v property.Value) (any, error) {
	switch {
	case v.IsNull():
		return nil, nil
	case v.IsComputed():
		return nil, fmt.Errorf("value is not known yet")
	case v.IsBool():
		return v.AsBool(), nil
	case v.IsNumber():
		return v.AsNumber(), nil
	case v.IsString():
		return v.AsString(), nil
	case v.IsArray():
		out := make([]any, 0, v.AsArray().Len())
		for _, e := range v.AsArray().All {
			j, err := toJSON(e)
			if err != nil {
				return nil, err
			}
			out = append(out, j)
		}
		return out, nil
	case v.IsMap():
		out := map[string]any{}
		for k, e := range v.AsMap().All {
			j, err := toJSON(e)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = j
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported value type (asset, archive or resource reference)")
}

// fromJSON converts a value decoded by encoding/json to a property value.
func fromJSON(x any) property.Value {
	switch x := x.(type) {
	case bool:
		return property.New(x)
	case float64:
		return property.New(x)
	case string:
		return property.New(x)
	case []any:
		arr := make([]property.Value, len(x))
		for i, e := range x {
			arr[i] = fromJSON(e)
		}
		return property.New(arr)
	case map[string]any:
		m := make(map[string]property.Value, len(x))
		for k, e := range x {
			m[k] = fromJSON(e)
		}
		return property.New(m)
	}
	return property.Value{}
}

// mapToJSON converts the entries of m named by keep to a JSON object.
func mapToJSON(m property.Map, keep map[string]map[string]any) (map[string]any, error) {
	out := map[string]any{}
	for k, v := range m.All {
		if keep[k] == nil {
			continue
		}
		j, err := toJSON(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		out[k] = j
	}
	return out, nil
}

// applyDefaults fills in schema defaults for absent properties, descending into nested objects
// and the items of arrays. The server applies every default of a nested object too (zod does), so
// leaving one out here would make refresh report it as drift of a field the program never set.
func applyDefaults(sch map[string]any, m property.Map) property.Map {
	for name, node := range properties(sch) {
		v, present := m.GetOk(name)
		if !present {
			if d := node["default"]; d != nil {
				m = m.Set(name, fromJSON(d))
			}
			continue
		}
		m = m.Set(name, defaultsIn(node, v))
	}
	return m
}

// defaultsIn applies the defaults inside one present value: an object's properties, an array's items.
func defaultsIn(node map[string]any, v property.Value) property.Value {
	if inner, ok := nullable(node); ok {
		node = inner
	}
	switch {
	case v.IsMap():
		return property.WithGoValue(v, applyDefaults(node, v.AsMap()))
	case v.IsArray():
		items, _ := node["items"].(map[string]any)
		if items == nil {
			return v
		}
		out := make([]property.Value, 0, v.AsArray().Len())
		for _, e := range v.AsArray().All {
			out = append(out, defaultsIn(items, e))
		}
		return property.WithGoValue(v, property.NewArray(out))
	}
	return v
}

// renameKeys returns m with every top-level key passed through f.
func renameKeys(m property.Map, f func(string) string) property.Map {
	out := make(map[string]property.Value, m.Len())
	for k, v := range m.All {
		out[f(k)] = v
	}
	return property.NewMap(out)
}
