package provider_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/pulumi/pulumi/pkg/v3/codegen/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zetlen/pulumi-talaria/internal/fake"
	"github.com/zetlen/pulumi-talaria/provider"
)

func buildSchema(t *testing.T, raw []byte) schema.PackageSpec {
	t.Helper()
	doc, err := provider.ParseKinds(raw)
	require.NoError(t, err)
	spec, err := provider.BuildSchema(doc, raw, "1.2.3")
	require.NoError(t, err)
	return spec
}

// kindDoc wraps a single resource kind with the given inputs properties.
func kindDoc(inputProps string, extra string) []byte {
	if inputProps != "" {
		inputProps = "," + inputProps
	}
	return []byte(fmt.Sprintf(`{"protocol":1,"kinds":[{"name":"mod.thing","mode":"resource","keyField":"name",
		"inputs":{"type":"object","required":["name"],"properties":{"name":{"type":"string"}%s}},
		"outputs":{"type":"object","properties":{}}%s}]}`, inputProps, extra))
}

func TestSchemaFromFirstKinds(t *testing.T) {
	spec := buildSchema(t, fake.Kinds)

	assert.Equal(t, "talaria", spec.Name)
	assert.Equal(t, "github://api.github.com/zetlen/pulumi-talaria", spec.PluginDownloadURL)
	assert.Equal(t, fake.Kinds, spec.Parameterization.Parameter)
	assert.Equal(t, "talaria", spec.Parameterization.BaseProvider.Name)

	// provider config
	assert.Equal(t, []string{"url"}, spec.Config.Required)
	assert.Equal(t, []string{"TALARIA_URL"}, spec.Config.Variables["url"].DefaultInfo.Environment)
	assert.True(t, spec.Config.Variables["apiKey"].Secret)
	assert.Equal(t, []string{"TALARIA_API_KEY"}, spec.Config.Variables["apiKey"].DefaultInfo.Environment)

	// tokens
	require.Contains(t, spec.Resources, "talaria:auth:RoleAcl")
	require.Contains(t, spec.Resources, "talaria:api_keys:ApiKey")
	require.Contains(t, spec.Functions, "talaria:directory:getScope")
	assert.Len(t, spec.Resources, 2)
	assert.Len(t, spec.Functions, 1)

	acl := spec.Resources["talaria:auth:RoleAcl"]
	assert.Equal(t, []string{"features", "role"}, acl.RequiredInputs, "nullable and defaulted fields are optional inputs")
	assert.Equal(t, false, acl.InputProperties["isSuperAdmin"].Default)
	assert.Equal(t, "array", acl.InputProperties["organizations"].Type, "nullable array becomes optional array")
	assert.Contains(t, acl.Required, "isSuperAdmin", "defaulted inputs are always present in state")
	assert.NotContains(t, acl.Required, "organizations")

	key := spec.Resources["talaria:api_keys:ApiKey"]
	assert.True(t, key.Properties["secret"].Secret, "secretFields are secret")
	assert.False(t, key.Properties["keyPrefix"].Secret)
	assert.NotContains(t, key.InputProperties, "secret", "outputs are not inputs")
	assert.Contains(t, key.Properties, "name", "properties = inputs ∪ outputs")
	assert.Contains(t, key.Required, "secret")
	assert.Equal(t, []string{"name", "roles"}, key.RequiredInputs)
	// `id` is reserved by Pulumi on resources, so it is exposed as <kind>Id.
	assert.Contains(t, key.Properties, "apiKeyId")
	assert.NotContains(t, key.Properties, "id")

	scope := spec.Functions["talaria:directory:getScope"]
	assert.Nil(t, scope.Inputs, "a data kind without inputs takes no arguments")
	assert.Contains(t, scope.Outputs.Properties, "organizationName")
	assert.Contains(t, scope.Outputs.Properties, "tenantId")

	bindOK(t, spec)
}

func bindOK(t *testing.T, spec schema.PackageSpec) {
	t.Helper()
	b, err := json.Marshal(spec)
	require.NoError(t, err)
	var round schema.PackageSpec
	require.NoError(t, json.Unmarshal(b, &round))
	_, diags, err := schema.BindSpec(round, schema.NewCachedLoader(nil), schema.ValidationOptions{})
	require.NoError(t, err)
	require.False(t, diags.HasErrors(), "schema must bind: %v", diags)
}

func TestSchemaTypeMapping(t *testing.T) {
	spec := buildSchema(t, kindDoc(`
		"n":{"type":"number"},
		"i":{"type":"integer","default":3},
		"b":{"type":"boolean"},
		"e":{"type":"string","enum":["a","b"],"default":"a"},
		"tags":{"type":"array","items":{"type":"string"}},
		"labels":{"type":"object","propertyNames":{"type":"string"},"additionalProperties":{"type":"string"}},
		"maybe":{"anyOf":[{"type":"integer"},{"type":"null"}]},
		"cfg":{"type":"object","required":["host"],"properties":{"host":{"type":"string"},"port":{"type":"integer"},
			"tls":{"type":"object","properties":{"ca":{"type":"string"}}}}},
		"rules":{"type":"array","items":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}`, ""))

	in := spec.Resources["talaria:mod:Thing"].InputProperties
	assert.Equal(t, "number", in["n"].Type)
	assert.Equal(t, "integer", in["i"].Type)
	assert.EqualValues(t, 3, in["i"].Default)
	assert.Equal(t, "boolean", in["b"].Type)
	assert.Equal(t, "string", in["e"].Type, "enums are plain strings")
	assert.Equal(t, "a", in["e"].Default)
	assert.Equal(t, "array", in["tags"].Type)
	assert.Equal(t, "string", in["tags"].Items.Type)
	assert.Equal(t, "object", in["labels"].Type)
	assert.Equal(t, "string", in["labels"].AdditionalProperties.Type)
	assert.Equal(t, "integer", in["maybe"].Type, "nullable unwraps")
	assert.NotContains(t, spec.Resources["talaria:mod:Thing"].RequiredInputs, "maybe")

	assert.Equal(t, "#/types/talaria:mod:ThingArgsCfg", in["cfg"].Ref)
	cfg := spec.Types["talaria:mod:ThingArgsCfg"]
	assert.Equal(t, []string{"host"}, cfg.Required)
	assert.Equal(t, "#/types/talaria:mod:ThingArgsCfgTls", cfg.Properties["tls"].Ref)
	assert.Contains(t, spec.Types, "talaria:mod:ThingArgsCfgTls")
	assert.Equal(t, "#/types/talaria:mod:ThingArgsRulesItem", in["rules"].Items.Ref)
	assert.Contains(t, spec.Types, "talaria:mod:ThingArgsRulesItem")

	bindOK(t, spec)
}

func TestSchemaRejectsUnsupported(t *testing.T) {
	cases := map[string]string{
		"oneOf":              `"x":{"oneOf":[{"type":"string"},{"type":"number"}]}`,
		"$ref":               `"x":{"$ref":"#/$defs/a"}`,
		"const":              `"x":{"type":"string","const":"a"}`,
		"anyOf is only":      `"x":{"anyOf":[{"type":"string"},{"type":"number"}]}`,
		"unsupported type":   `"x":{"type":"null"}`,
		"non-string":         `"x":{"type":["string","number"]}`,
		"missing":            `"x":{}`,
		"enum is only":       `"x":{"type":"integer","enum":[1,2]}`,
		"without an \"items": `"x":{"type":"array"}`,
		"needs":              `"x":{"type":"object"}`,
		"x.y":                `"x":{"type":"object","properties":{"y":{"allOf":[]}}}`,
	}
	for want, props := range cases {
		t.Run(want, func(t *testing.T) {
			raw := kindDoc(props, "")
			doc, err := provider.ParseKinds(raw)
			require.NoError(t, err)
			_, err = provider.BuildSchema(doc, raw, "1.0.0")
			require.Error(t, err)
			assert.Contains(t, err.Error(), `"mod.thing"`, "names the kind")
			assert.Contains(t, err.Error(), `"x`, "names the field")
			assert.Contains(t, strings.ToLower(err.Error()), strings.ToLower(want))
		})
	}
}

func TestParseKindsValidation(t *testing.T) {
	_, err := provider.ParseKinds([]byte(`{"protocol":2,"kinds":[]}`))
	assert.ErrorContains(t, err, "unsupported kinds protocol 2")

	for want, tail := range map[string]string{
		"secretFields names unknown field": `,"secretFields":["ghost"]`,
		"replaceOnChanges names unknown":   `,"replaceOnChanges":["ghost"]`,
	} {
		_, err := provider.ParseKinds(kindDoc(``, tail))
		assert.ErrorContains(t, err, want)
	}

	// `id` is exposed as thingId, which must not collide with a real field.
	_, err = provider.ParseKinds(kindDoc(`"id":{"type":"string"},"thingId":{"type":"string"}`, ""))
	assert.ErrorContains(t, err, "collides")
}
