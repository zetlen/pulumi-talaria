package provider

import (
	"fmt"
	"slices"

	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
)

const (
	// BaseName is the name of the un-parameterized provider (and of its binary suffix).
	BaseName          = "talaria"
	pluginDownloadURL = "github://api.github.com/zetlen/pulumi-talaria"
)

// unsupported JSON Schema keywords that change a value's shape in ways we cannot express.
var unsupported = []string{"$ref", "oneOf", "allOf", "not", "if", "const", "prefixItems", "patternProperties", "$defs", "definitions", "contains"}

// BuildSchema builds the Pulumi package schema for a kinds document. doc may be nil for the
// bare base provider. raw is the kinds document, embedded as the parameterization value.
func BuildSchema(doc *KindsDoc, raw []byte, version string) (schema.PackageSpec, error) {
	config := map[string]schema.PropertySpec{
		"url": {
			TypeSpec:    schema.TypeSpec{Type: "string"},
			Description: "Base URL of the Talaria instance, without /api/iac.",
			DefaultInfo: &schema.DefaultSpec{Environment: []string{"TALARIA_URL"}},
		},
		"apiKey": {
			TypeSpec:    schema.TypeSpec{Type: "string"},
			Description: "API key sent as `Authorization: ApiKey <key>`.",
			Secret:      true,
			DefaultInfo: &schema.DefaultSpec{Environment: []string{"TALARIA_API_KEY"}},
		},
	}
	spec := schema.PackageSpec{
		Name:              BaseName,
		Version:           version,
		Description:       "Manage a Talaria instance's configuration through its iac module.",
		Repository:        "https://github.com/zetlen/pulumi-talaria",
		PluginDownloadURL: pluginDownloadURL,
		Config:            schema.ConfigSpec{Variables: config, Required: []string{"url"}},
		Provider: &schema.ResourceSpec{
			ObjectTypeSpec:  schema.ObjectTypeSpec{Type: "object", Properties: config},
			InputProperties: config,
			RequiredInputs:  []string{"url"},
		},
	}
	if doc == nil {
		return spec, nil
	}
	spec.Name = PackageName
	spec.Parameterization = &schema.ParameterizationSpec{
		BaseProvider: schema.BaseProviderSpec{Name: BaseName, Version: version},
		Parameter:    raw,
	}
	spec.Types = map[string]schema.ComplexTypeSpec{}
	spec.Resources = map[string]schema.ResourceSpec{}
	spec.Functions = map[string]schema.FunctionSpec{}
	for _, k := range doc.Kinds {
		b := &builder{kind: k.Name, types: spec.Types}
		if err := b.add(k, &spec); err != nil {
			return spec, err
		}
	}
	return spec, nil
}

type builder struct {
	kind  string
	types map[string]schema.ComplexTypeSpec
}

func (b *builder) errorf(path, format string, args ...any) error {
	return fmt.Errorf("kind %q, field %q: %s", b.kind, path, fmt.Sprintf(format, args...))
}

func (b *builder) add(k *Kind, spec *schema.PackageSpec) error {
	tok := k.Token()
	in, err := b.objectAt(k, k.Inputs, tok+"Args", "")
	if err != nil {
		return err
	}
	out, err := b.objectAt(k, k.Outputs, tok, "")
	if err != nil {
		return err
	}
	if k.Mode == modeData {
		fn := schema.FunctionSpec{
			Description: k.Description,
			Outputs:     &schema.ObjectTypeSpec{Type: "object", Properties: out.props, Required: out.required},
		}
		if len(in.props) > 0 {
			fn.Inputs = &schema.ObjectTypeSpec{Type: "object", Properties: in.props, Required: in.required}
		}
		spec.Functions[tok] = fn
		return nil
	}
	props := map[string]schema.PropertySpec{}
	for n, p := range out.props {
		props[n] = p
	}
	for n, p := range in.props {
		props[n] = p
	}
	// State is inputs ∪ outputs; anything required or defaulted on the way in is always present.
	req := append(append(slices.Clone(in.required), out.required...), in.defaulted...)
	slices.Sort(req)
	spec.Resources[tok] = schema.ResourceSpec{
		ObjectTypeSpec:  schema.ObjectTypeSpec{Description: k.Description, Type: "object", Properties: props, Required: slices.Compact(req)},
		InputProperties: in.props,
		RequiredInputs:  in.required,
	}
	return nil
}

type objectProps struct {
	props     map[string]schema.PropertySpec
	required  []string
	defaulted []string
}

// objectAt converts the properties of a JSON Schema object; nested objects become named types
// under tokenBase. path is empty at the top level.
func (b *builder) objectAt(k *Kind, sch map[string]any, tokenBase, path string) (objectProps, error) {
	res := objectProps{props: map[string]schema.PropertySpec{}, required: required(sch)}
	if path == "" {
		for i, r := range res.required {
			res.required[i] = k.exposed(r)
		}
	}
	props := properties(sch)
	for _, name := range sortedKeys(props) {
		p := name
		if path != "" {
			p = path + "." + name
		}
		spec, hasDefault, err := b.property(k, props[name], tokenBase+pascal(name), p)
		if err != nil {
			return res, err
		}
		spec.Secret = path == "" && k.isSecret(name)
		if path == "" {
			name = k.exposed(name)
		}
		res.props[name] = spec
		if hasDefault {
			res.defaulted = append(res.defaulted, name)
		}
	}
	return res, nil
}

// property converts one property schema. Defaults are only carried into the Pulumi schema for
// primitives (the only types the schema binder accepts a default for); Check still applies the
// rest from the kinds document.
func (b *builder) property(k *Kind, node map[string]any, tokenBase, path string) (schema.PropertySpec, bool, error) {
	ts, err := b.typeSpec(k, node, tokenBase, path)
	if err != nil {
		return schema.PropertySpec{}, false, err
	}
	desc, _ := node["description"].(string)
	spec := schema.PropertySpec{TypeSpec: ts, Description: desc}
	def := node["default"]
	hasDefault := def != nil
	if hasDefault && slices.Contains([]string{"string", "number", "integer", "boolean"}, ts.Type) {
		spec.Default = def
	}
	return spec, hasDefault, nil
}

// isAnySchema reports whether a JSON Schema node imposes no constraints beyond an
// optional description, i.e. it should accept arbitrary JSON. This matches zod's
// z.any() output ({}) and the inner alternative of nullable any schemas.
func isAnySchema(node map[string]any) bool {
	for k := range node {
		if k != "description" {
			return false
		}
	}
	return true
}

func (b *builder) typeSpec(k *Kind, node map[string]any, tokenBase, path string) (schema.TypeSpec, error) {
	for _, kw := range unsupported {
		if _, ok := node[kw]; ok {
			return schema.TypeSpec{}, b.errorf(path, "unsupported JSON Schema keyword %q", kw)
		}
	}
	if _, ok := node["anyOf"]; ok {
		inner, ok := nullable(node)
		if ok {
			return b.typeSpec(k, inner, tokenBase, path)
		}
		// Non-nullable anyOf (unions) cannot be expressed precisely; fall back to untyped JSON.
		return schema.TypeSpec{Ref: "pulumi.json#/Any"}, nil
	}
	// A bare/empty schema (or one carrying only a description) accepts any JSON value.
	if isAnySchema(node) {
		return schema.TypeSpec{Ref: "pulumi.json#/Any"}, nil
	}
	t, isString := node["type"].(string)
	if !isString {
		return schema.TypeSpec{}, b.errorf(path, "missing or non-string \"type\"")
	}
	if _, ok := node["enum"]; ok && t != "string" {
		return schema.TypeSpec{}, b.errorf(path, "enum is only supported on string types, got %q", t)
	}
	switch t {
	case "string", "number", "integer", "boolean":
		return schema.TypeSpec{Type: t}, nil
	case "array":
		items, _ := node["items"].(map[string]any)
		if items == nil {
			return schema.TypeSpec{}, b.errorf(path, "array without an \"items\" schema")
		}
		el, err := b.typeSpec(k, items, tokenBase+"Item", path+"[]")
		if err != nil {
			return schema.TypeSpec{}, err
		}
		return schema.TypeSpec{Type: "array", Items: &el}, nil
	case "object":
		props := properties(node)
		ap, hasAP := node["additionalProperties"].(map[string]any)
		switch {
		case len(props) > 0 && hasAP:
			// Pulumi object types cannot combine named properties with a catch-all map; fall back.
			return schema.TypeSpec{Ref: "pulumi.json#/Any"}, nil
		case len(props) > 0:
			obj, err := b.objectAt(k, node, tokenBase, path)
			if err != nil {
				return schema.TypeSpec{}, err
			}
			desc, _ := node["description"].(string)
			b.types[tokenBase] = schema.ComplexTypeSpec{ObjectTypeSpec: schema.ObjectTypeSpec{
				Description: desc, Type: "object", Properties: obj.props, Required: obj.required,
			}}
			return schema.TypeSpec{Ref: "#/types/" + tokenBase}, nil
		case hasAP:
			el, err := b.typeSpec(k, ap, tokenBase+"Value", path+"{}")
			if err != nil {
				return schema.TypeSpec{}, err
			}
			return schema.TypeSpec{Type: "object", AdditionalProperties: &el}, nil
		}
		return schema.TypeSpec{}, b.errorf(path, "object needs \"properties\" or an additionalProperties schema")
	}
	return schema.TypeSpec{}, b.errorf(path, "unsupported type %q", t)
}
