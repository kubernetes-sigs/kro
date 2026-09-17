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

package library

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	k8sjson "k8s.io/apimachinery/pkg/util/json"

	"github.com/kubernetes-sigs/kro/pkg/cel/conversion"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema"
)

const simpleSchemaToOpenAPIFn = "simpleschema.toOpenAPI"

// Keys of the schema block converted by simpleschema.toOpenAPI.
const (
	schemaBlockSpecKey   = "spec"
	schemaBlockTypesKey  = "types"
	schemaBlockStatusKey = "status"
)

// schemaBlockKeys identify a map as an RGD-style schema block: the converted
// keys plus the keys that describe the CRD rather than its schema (the other
// fields of ResourceGraphDefinition.spec.schema, and the names a Kind
// definition held in a def node carries). Descriptor keys are ignored by the
// conversion, but their presence lets a spec-less RGD schema convert to an
// empty root instead of being mistaken for a bare field map.
var schemaBlockKeys = []string{
	schemaBlockSpecKey, schemaBlockTypesKey, schemaBlockStatusKey,
	"apiVersion", "kind", "group", "scope", "plural", "shortNames", "categories", "metadata", "additionalPrinterColumns",
}

// SimpleSchema returns a CEL library that converts kro's SimpleSchema format
// into OpenAPI v3 JSON schema.
//
// This is the same conversion the ResourceGraphDefinition controller applies to
// spec.schema when it synthesizes the instance CRD. Exposing it in CEL lets a
// Graph that holds or reads an RGD-like schema block build a
// CustomResourceDefinition's openAPIV3Schema itself instead of relying on the
// RGD controller.
//
// # ToOpenAPI
//
// Converts a whole RGD-style schema block into the root openAPIV3Schema of a
// CustomResourceDefinition, returned as a CEL map shaped like a JSONSchemaProps
// object:
//
//	simpleschema.toOpenAPI(schema dyn) -> dyn
//
// The block is a map with the optional keys "spec", "types" and "status" — the
// shape of ResourceGraphDefinition.spec.schema, or of a Kind definition held in
// a def node:
//   - spec: field name -> SimpleSchema type expression (e.g. "string | required=true")
//     or a nested map for inline objects.
//   - types: custom type name -> definition (alias string or struct map), shared
//     by spec and status.
//   - status: like spec. A field whose value is a CEL expression — a string
//     whose type position (the text before any "|" marker separator) contains
//     "${", or a list of such strings such as kro's status.conditions block —
//     cannot be typed here, because its type depends on the resources it
//     references, and is emitted as {"x-kubernetes-preserve-unknown-fields": true},
//     which accepts whatever the expression evaluates to.
//
// Other keys (apiVersion, kind, group, scope, additionalPrinterColumns, ...)
// describe the CRD rather than its schema and are ignored, so an RGD's
// spec.schema can be passed as-is. A non-empty block with none of those keys
// and none of the three schema keys is rejected: it is almost certainly a bare
// field map that should be wrapped in {"spec": ...}. The result is
//
//	{"type": "object", "properties": {
//	  "apiVersion": {"type": "string"}, "kind": {"type": "string"}, "metadata": {"type": "object"},
//	  "spec":   <spec converted with types>,
//	  "status": <status converted with types>}}
//
// where an absent spec or status yields {"type": "object"}. Each converted block
// has type "object" and, when fields are present, a "properties" map and a
// sorted "required" list; numeric values (defaults, minimum/maximum, minLength,
// ...) are int or double, never strings. kro's default instance status fields
// (state, conditions) are not injected; declare them in the status block if
// needed.
//
// Example usage:
//
//	// {"type": "object", "properties": {"apiVersion": ..., "kind": ..., "metadata": ...,
//	//   "spec": {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]},
//	//   "status": {"type": "object"}}}
//	simpleschema.toOpenAPI({"spec": {"name": "string | required=true"}})
//
//	// Whole CRD schema from a Kind definition held in a def node, or from an
//	// RGD read via ref:
//	simpleschema.toOpenAPI(kindSpec)
//	simpleschema.toOpenAPI(rgd.spec.schema)
//
//	// Just the spec sub-schema, to compose a root by hand
//	simpleschema.toOpenAPI(kindSpec).properties.spec
//
// In a CustomResourceDefinition template:
//
//	openAPIV3Schema: ${simpleschema.toOpenAPI(kindSpec)}
//
// The parameter is dyn rather than map(string, dyn) on purpose: a schema block
// read from a typed resource (e.g. an RGD's spec.schema, whose spec/types/status
// are x-kubernetes-preserve-unknown-fields objects) type-checks as an object
// type, not a map type, and would be rejected by a map(K, V) signature. See the
// deepMerge overload in maps.go for the same trade-off.
func SimpleSchema() cel.EnvOption {
	return cel.Lib(&simpleSchemaLibrary{})
}

type simpleSchemaLibrary struct{}

func (l *simpleSchemaLibrary) LibraryName() string {
	return "kro.simpleschema"
}

func (l *simpleSchemaLibrary) CompileOptions() []cel.EnvOption {
	return []cel.EnvOption{
		// simpleschema.toOpenAPI(schema dyn) -> dyn
		cel.Function(simpleSchemaToOpenAPIFn,
			cel.Overload(simpleSchemaToOpenAPIFn+"_dyn",
				[]*cel.Type{cel.DynType},
				cel.DynType,
				cel.UnaryBinding(toOpenAPI),
			),
		),
	}
}

func (l *simpleSchemaLibrary) ProgramOptions() []cel.ProgramOption {
	return nil
}

// toOpenAPI implements simpleschema.toOpenAPI: it reads the spec,
// types and status sub-blocks of the schema block and composes the root
// openAPIV3Schema of a CRD from them.
func toOpenAPI(block ref.Val) ref.Val {
	if types.IsUnknownOrError(block) {
		return block
	}
	root, err := openAPISchemaFromBlock(block)
	if err != nil {
		return types.NewErr("%s: %s", simpleSchemaToOpenAPIFn, err.Error())
	}
	return types.DefaultTypeAdapter.NativeToValue(root)
}

func openAPISchemaFromBlock(block ref.Val) (map[string]any, error) {
	native, err := conversion.GoNativeType(block)
	if err != nil {
		return nil, fmt.Errorf("schema argument must be a map with string keys: %w", err)
	}
	blockMap, ok := native.(map[string]any)
	if !ok {
		// CEL null is rejected rather than treated as an empty block so that a
		// missing schema surfaces as a clear error instead of an empty CRD schema.
		return nil, fmt.Errorf("schema argument must be a map with string keys, got %s", block.Type().TypeName())
	}
	if isBareFieldMap(blockMap) {
		return nil, fmt.Errorf("schema block has none of the keys %q, %q or %q; wrap SimpleSchema fields in {%q: {...}}",
			schemaBlockSpecKey, schemaBlockTypesKey, schemaBlockStatusKey, schemaBlockSpecKey)
	}

	typesMap, err := schemaBlockSubMap(blockMap, schemaBlockTypesKey)
	if err != nil {
		return nil, err
	}
	// Every conversion below loads the custom types; check them once here so a
	// bad type is reported under "types" rather than under whichever block is
	// converted first.
	if _, err := simpleschema.ToOpenAPISpec(nil, typesMap); err != nil {
		return nil, fmt.Errorf("%s: %w", schemaBlockTypesKey, err)
	}
	specSchema, err := convertSchemaBlock(blockMap, schemaBlockSpecKey, typesMap)
	if err != nil {
		return nil, err
	}
	statusSchema, err := convertSchemaBlock(blockMap, schemaBlockStatusKey, typesMap, simpleschema.AllowExpressionFields())
	if err != nil {
		return nil, err
	}

	// Same root shape the RGD controller synthesizes for instance CRDs (see
	// pkg/graph/crd newCRDSchema), minus the default state/conditions status
	// fields, which are controller policy rather than part of the schema.
	root := &extv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]extv1.JSONSchemaProps{
			"apiVersion": {Type: "string"},
			"kind":       {Type: "string"},
			"metadata":   {Type: "object"},
			"spec":       *specSchema,
			"status":     *statusSchema,
		},
	}

	// Round-trip through JSON so the CEL value has exactly the shape the API
	// server expects in a CRD (honours the custom marshalers of JSON,
	// JSONSchemaPropsOrBool and JSONSchemaPropsOrArray, and drops zero-valued
	// fields via omitempty). The apimachinery decoder keeps integral numbers as
	// int64 rather than float64, so a marker such as default=3 is a CEL int, as
	// it is when kro reads the same CRD back from the cluster.
	raw, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("failed to encode schema: %w", err)
	}
	result := map[string]any{}
	if err := k8sjson.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("failed to decode schema: %w", err)
	}
	return result, nil
}

// isBareFieldMap reports whether a non-empty block has no recognised key. Such
// a map is almost certainly a spec field map passed without its {"spec": ...}
// wrapper, which would otherwise silently convert to an empty spec.
func isBareFieldMap(block map[string]any) bool {
	if len(block) == 0 {
		return false
	}
	for key := range block {
		if slices.Contains(schemaBlockKeys, key) {
			return false
		}
	}
	return true
}

// convertSchemaBlock converts the SimpleSchema field map at block[key] (an
// absent key is an empty map) against the shared custom types, attributing any
// error to the key so the author knows which block to fix.
func convertSchemaBlock(block map[string]any, key string, customTypes map[string]any, opts ...simpleschema.Option) (*extv1.JSONSchemaProps, error) {
	fields, err := schemaBlockSubMap(block, key)
	if err != nil {
		return nil, err
	}
	schema, err := simpleschema.ToOpenAPISpec(fields, customTypes, opts...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return schema, nil
}

// schemaBlockSubMap returns block[key] as a string-keyed map. An absent or null
// key yields an empty map; any other non-map value is an error naming the key.
func schemaBlockSubMap(block map[string]any, key string) (map[string]any, error) {
	raw, ok := block[key]
	if !ok || raw == nil {
		return map[string]any{}, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("schema block key %q must be a map with string keys, got %s",
			key, types.DefaultTypeAdapter.NativeToValue(raw).Type().TypeName())
	}
	return m, nil
}
