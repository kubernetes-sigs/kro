// Copyright 2026 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package compiler

import (
	"strings"

	"k8s.io/kube-openapi/pkg/validation/spec"
)

// inferDefSchema walks the rendered def payload (post-parse, so CEL
// fragments are still string-shaped at their site) and produces an
// OpenAPI schema describing the def's runtime shape. This lets the
// typed CEL environment narrow def-sourced expressions so a typo like
// `${naming.colr}` (vs `color`) fails at compile time instead of at
// apply time.
//
// Rules:
//   - bool literal             → {type: boolean}
//   - integer literal          → {type: integer}
//   - float literal            → {type: number}
//   - string literal           → {type: string}
//   - string containing `${`   → empty schema (dyn — runtime decides)
//   - map                      → {type: object, properties: { recurse }}
//   - array                    → {type: array, items: { infer from first }}
//   - empty array              → {type: array} (items unspecified)
//   - nil / other              → empty schema (dyn)
//
// Empty schemas survive the round-trip through SchemaDeclTypeWithMetadata
// as DynType, which is what we want for fields whose runtime type we
// can't know statically.
func inferDefSchema(payload map[string]any) *spec.Schema {
	return inferSchemaFromValue(payload)
}

func inferSchemaFromValue(v any) *spec.Schema {
	switch val := v.(type) {
	case bool:
		return scalar("boolean")
	case int, int32, int64, uint, uint32, uint64:
		return scalar("integer")
	case float32, float64:
		return scalar("number")
	case string:
		// A def field whose literal value is a CEL fragment can resolve
		// to any type at runtime — leave it as dyn so downstream
		// expressions aren't held to the literal-string type.
		if strings.Contains(val, "${") {
			return dynSchema()
		}
		return scalar("string")
	case map[string]any:
		props := make(map[string]spec.Schema, len(val))
		for k, child := range val {
			props[k] = *inferSchemaFromValue(child)
		}
		return &spec.Schema{SchemaProps: spec.SchemaProps{
			Type:       []string{"object"},
			Properties: props,
		}}
	case []any:
		// Items must be set or SchemaDeclTypeWithMetadata drops the field.
		// Empty arrays infer to list<dyn>; non-empty arrays take their
		// first element's schema as the items schema.
		var items *spec.Schema
		if len(val) == 0 {
			items = dynSchema()
		} else {
			items = inferSchemaFromValue(val[0])
		}
		return &spec.Schema{SchemaProps: spec.SchemaProps{
			Type:  []string{"array"},
			Items: &spec.SchemaOrArray{Schema: items},
		}}
	case nil:
		return dynSchema()
	default:
		// Unknown Go type (rare via yaml.UnmarshalStrict + JSON path).
		// Fall back to dyn rather than rejecting outright.
		return dynSchema()
	}
}

func scalar(t string) *spec.Schema {
	return &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{t}}}
}

// dynSchema returns a schema that the CEL DeclType converter maps to
// DynType. SchemaDeclTypeWithMetadata returns nil for a fully empty
// schema (which would silently drop the field from its parent's
// properties), so we set x-kubernetes-int-or-string to take its
// IsXIntOrString fast path which returns DynType. The actual value can
// be any type at runtime — int-or-string is just the marker.
func dynSchema() *spec.Schema {
	s := &spec.Schema{}
	s.AddExtension("x-kubernetes-int-or-string", true)
	return s
}
