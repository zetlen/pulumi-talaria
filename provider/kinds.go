// Package provider implements the generic Pulumi provider for Talaria's `iac`
// HTTP protocol (version 1). Its resources and functions are not compiled in:
// they are derived from a kinds document at parameterize time.
package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// ProtocolVersion is the only kinds-document protocol this provider speaks.
const ProtocolVersion = 1

const (
	// PackageName is the name of the parameterized package, and the first token segment.
	PackageName  = "talaria"
	modeResource = "resource"
	modeData     = "data"
)

// KindsDoc is the document served by `GET /api/iac/kinds`.
type KindsDoc struct {
	Protocol int     `json:"protocol"`
	Kinds    []*Kind `json:"kinds"`
}

// Kind is one resource or data kind. Inputs and Outputs are JSON Schema objects.
type Kind struct {
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	Mode             string         `json:"mode"`
	KeyField         string         `json:"keyField"`
	Inputs           map[string]any `json:"inputs"`
	Outputs          map[string]any `json:"outputs"`
	SecretFields     []string       `json:"secretFields"`
	ReplaceOnChanges []string       `json:"replaceOnChanges"`

	module, pascal string
}

var identRe = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Token is the Pulumi token: talaria:<module>:<Pascal> for resources,
// talaria:<module>:get<Pascal> for data kinds.
func (k *Kind) Token() string {
	name := k.pascal
	if k.Mode == modeData {
		name = "get" + name
	}
	return PackageName + ":" + k.module + ":" + name
}

// ParseKinds decodes and validates a kinds document.
func ParseKinds(raw []byte) (*KindsDoc, error) {
	var doc KindsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing kinds document: %w", err)
	}
	if doc.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("unsupported kinds protocol %d (this provider speaks %d)", doc.Protocol, ProtocolVersion)
	}
	seen := map[string]string{}
	for _, k := range doc.Kinds {
		if err := k.validate(); err != nil {
			return nil, err
		}
		if other, dup := seen[k.Token()]; dup {
			return nil, fmt.Errorf("kinds %q and %q both map to token %s", other, k.Name, k.Token())
		}
		seen[k.Token()] = k.Name
	}
	return &doc, nil
}

func (k *Kind) validate() error {
	mod, snake, ok := strings.Cut(k.Name, ".")
	if !ok || !identRe.MatchString(mod) || !identRe.MatchString(snake) {
		return fmt.Errorf("kind %q: name must be <module>.<snake_name>", k.Name)
	}
	k.module, k.pascal = mod, pascal(snake)
	if k.Mode != modeResource && k.Mode != modeData {
		return fmt.Errorf("kind %q: unknown mode %q", k.Name, k.Mode)
	}
	if k.Inputs == nil || k.Outputs == nil {
		return fmt.Errorf("kind %q: inputs and outputs are required", k.Name)
	}
	in, out := properties(k.Inputs), properties(k.Outputs)
	if k.Mode == modeResource {
		for _, r := range []string{"id", "urn"} {
			if in[k.exposed(r)] != nil || out[k.exposed(r)] != nil {
				return fmt.Errorf("kind %q: field %q collides with the exposed name of reserved field %q", k.Name, k.exposed(r), r)
			}
		}
	}
	for _, f := range k.SecretFields {
		if in[f] == nil && out[f] == nil {
			return fmt.Errorf("kind %q: secretFields names unknown field %q", k.Name, f)
		}
	}
	if k.Mode == modeData {
		return nil
	}
	if in[k.KeyField] == nil {
		return fmt.Errorf("kind %q: keyField %q is not an input field", k.Name, k.KeyField)
	}
	if t, _ := in[k.KeyField]["type"].(string); t != "string" {
		return fmt.Errorf("kind %q: keyField %q must be a string field", k.Name, k.KeyField)
	}
	for _, f := range k.ReplaceOnChanges {
		if in[f] == nil {
			return fmt.Errorf("kind %q: replaceOnChanges names unknown input %q", k.Name, f)
		}
	}
	return nil
}

// Pulumi reserves `id` and `urn` on resources, so those protocol fields are exposed to Pulumi
// as <camelName>Id / <camelName>Urn (api_keys.api_key: id -> apiKeyId). Data kinds are unaffected.
func (k *Kind) exposed(name string) string {
	if k.Mode == modeResource && (name == "id" || name == "urn") {
		return strings.ToLower(k.pascal[:1]) + k.pascal[1:] + pascal(name)
	}
	return name
}

// protocolName is the inverse of exposed.
func (k *Kind) protocolName(name string) string {
	for _, r := range []string{"id", "urn"} {
		if name == k.exposed(r) {
			return r
		}
	}
	return name
}

func (k *Kind) isSecret(field string) bool { return slices.Contains(k.SecretFields, field) }

func (k *Kind) isReplace(field string) bool {
	return field == k.KeyField || slices.Contains(k.ReplaceOnChanges, field)
}

// pascal turns snake_case into PascalCase.
func pascal(s string) string {
	var b strings.Builder
	for _, part := range strings.Split(s, "_") {
		if part != "" {
			b.WriteString(strings.ToUpper(part[:1]) + part[1:])
		}
	}
	return b.String()
}

// properties returns the property schemas of a JSON Schema object (nil entries are not produced).
func properties(sch map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	props, _ := sch["properties"].(map[string]any)
	for name, p := range props {
		if m, ok := p.(map[string]any); ok {
			out[name] = m
		}
	}
	return out
}

// sortedKeys returns the keys of m in order, for deterministic output.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// nullable reports whether a property schema is `anyOf: [X, {type: null}]`, and returns X.
func nullable(node map[string]any) (inner map[string]any, ok bool) {
	any2, _ := node["anyOf"].([]any)
	if len(any2) != 2 {
		return nil, false
	}
	for i, alt := range any2 {
		m, _ := alt.(map[string]any)
		if t, _ := m["type"].(string); t == "null" && len(m) == 1 {
			other, _ := any2[1-i].(map[string]any)
			return other, other != nil
		}
	}
	return nil, false
}

// required lists the required fields of a JSON Schema object; nullable fields are optional.
func required(sch map[string]any) []string {
	props := properties(sch)
	var out []string
	list, _ := sch["required"].([]any)
	for _, r := range list {
		name, _ := r.(string)
		if _, isNull := nullable(props[name]); props[name] != nil && !isNull {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
