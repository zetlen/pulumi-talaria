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

// applyDefaults fills in schema defaults for absent properties, descending into nested objects.
func applyDefaults(sch map[string]any, m property.Map) property.Map {
	for name, node := range properties(sch) {
		v, present := m.GetOk(name)
		if !present {
			if d := node["default"]; d != nil {
				m = m.Set(name, fromJSON(d))
			}
			continue
		}
		if inner, ok := nullable(node); ok {
			node = inner
		}
		if v.IsMap() {
			m = m.Set(name, property.WithGoValue(v, applyDefaults(node, v.AsMap())))
		}
	}
	return m
}

// renameKeys returns m with every top-level key passed through f.
func renameKeys(m property.Map, f func(string) string) property.Map {
	out := make(map[string]property.Value, m.Len())
	for k, v := range m.All {
		out[f(k)] = v
	}
	return property.NewMap(out)
}
