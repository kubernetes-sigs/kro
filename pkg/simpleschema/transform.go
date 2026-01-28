// Copyright 2025 The Kubernetes Authors.
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

package simpleschema

import (
	"errors"
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema/types"
)

// customType stores the schema and required state for a custom type.
type customType struct {
	Schema   extv1.JSONSchemaProps
	Required bool
}

type transformer struct {
	customTypes map[string]customType
}

// Resolve implements types.Resolver.
func (t *transformer) Resolve(name string) (*extv1.JSONSchemaProps, error) {
	ct, ok := t.customTypes[name]
	if !ok {
		return nil, fmt.Errorf("unknown type: %s", name)
	}
	return ct.Schema.DeepCopy(), nil
}

// IsRequired returns whether a custom type has required=true marker.
// Precondition: name must exist in customTypes (caller ensures this via Schema resolution).
func (t *transformer) IsRequired(name string) bool {
	return t.customTypes[name].Required
}

func (t *transformer) loadCustomTypes(customTypes map[string]interface{}) error {
	if len(customTypes) == 0 {
		return nil
	}

	// Parse types to extract dependencies for DAG ordering
	parsed := make(map[string]types.Type)
	for name, spec := range customTypes {
		typ, err := parseSpec(spec)
		if err != nil {
			return fmt.Errorf("parsing type %s: %w", name, err)
		}
		parsed[name] = typ
	}

	// Build DAG for dependency ordering
	graph := dag.NewDirectedAcyclicGraph[string]()
	for name := range parsed {
		// AddVertex cannot fail here - map keys are unique
		_ = graph.AddVertex(name, 0)
	}
	for name, typ := range parsed {
		if err := graph.AddDependencies(name, typ.Deps()); err != nil {
			var cycleErr *dag.CycleError[string]
			if errors.As(err, &cycleErr) {
				return fmt.Errorf("cyclic dependency in type %s: %w", name, err)
			}
			return err
		}
	}

	// Build schemas in topological order
	// TopologicalSort cannot fail here - cycles are caught by AddDependencies above
	order, _ := graph.TopologicalSort()

	for _, name := range order {
		spec := customTypes[name]
		schema, required, err := t.buildCustomTypeSchema(name, spec)
		if err != nil {
			return fmt.Errorf("building schema for %s: %w", name, err)
		}
		t.customTypes[name] = customType{Schema: *schema, Required: required}
	}

	return nil
}

// buildCustomTypeSchema builds a schema for a custom type definition.
// Returns the schema and whether the type has required=true marker.
// Precondition: spec is string or map[string]interface{} (parseSpec validates this).
func (t *transformer) buildCustomTypeSchema(name string, spec interface{}) (*extv1.JSONSchemaProps, bool, error) {
	switch val := spec.(type) {
	case string:
		// Type alias: "MyType": "string | default=foo"
		dummyParent := &extv1.JSONSchemaProps{}
		schema, err := t.buildFieldFromString(name, val, dummyParent)
		if err != nil {
			return nil, false, err
		}
		required := len(dummyParent.Required) > 0
		return schema, required, nil
	default:
		// Struct type: "MyType": { "field": "string" }
		schema, err := t.buildSchema(val.(map[string]interface{}))
		if err != nil {
			return nil, false, err
		}
		return schema, false, nil
	}
}

func (t *transformer) buildSchema(spec map[string]interface{}) (*extv1.JSONSchemaProps, error) {
	schema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: make(map[string]extv1.JSONSchemaProps),
	}

	childHasDefault := false

	for fieldName, fieldSpec := range spec {
		fieldSchema, err := t.buildFieldSchema(fieldName, fieldSpec, schema)
		if err != nil {
			return nil, fmt.Errorf("field %s: %w", fieldName, err)
		}
		schema.Properties[fieldName] = *fieldSchema

		if fieldSchema.Default != nil {
			childHasDefault = true
		}
	}

	// Only set default if no required fields and a child has defaults.
	// Setting default:{} on an object with required fields would violate
	// the schema since {} doesn't include the required fields.
	if len(schema.Required) == 0 && childHasDefault && schema.Default == nil {
		schema.Default = &extv1.JSON{Raw: []byte("{}")}
	}

	return schema, nil
}

func (t *transformer) buildFieldSchema(name string, spec interface{}, parent *extv1.JSONSchemaProps) (*extv1.JSONSchemaProps, error) {
	switch val := spec.(type) {
	case string:
		return t.buildFieldFromString(name, val, parent)
	case map[string]interface{}:
		return t.buildSchema(val)
	default:
		return nil, fmt.Errorf("unexpected type: %T", spec)
	}
}

func (t *transformer) buildFieldFromString(name, fieldValue string, parent *extv1.JSONSchemaProps) (*extv1.JSONSchemaProps, error) {
	typ, markers, err := ParseField(fieldValue)
	if err != nil {
		return nil, err
	}

	schema, err := typ.Schema(t)
	if err != nil {
		return nil, err
	}

	// Check if this is a custom type that has required=true
	if custom, ok := typ.(types.Custom); ok {
		if t.IsRequired(string(custom)) {
			parent.Required = append(parent.Required, name)
		}
	}

	if err := applyMarkers(schema, markers, name, parent); err != nil {
		return nil, err
	}

	return schema, nil
}
