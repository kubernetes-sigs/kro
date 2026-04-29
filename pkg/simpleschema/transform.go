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
	"sort"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kubernetes-sigs/kro/pkg/graph/dag"
	"github.com/kubernetes-sigs/kro/pkg/simpleschema/types"
)

// customType stores the schema and required state for a custom type.
type customType struct {
	Schema         extv1.JSONSchemaProps
	Required       bool
	PrinterColumns []PrinterColumn
}

type transformer struct {
	customTypes map[string]customType
}

// newTransformer creates a new transformer with the given custom types.
func newTransformer(customTypes map[string]interface{}) (*transformer, error) {
	t := &transformer{
		customTypes: make(map[string]customType),
	}
	if err := t.loadCustomTypes(customTypes); err != nil {
		return nil, err
	}
	return t, nil
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
			if _, ok := errors.AsType[*dag.CycleError[string]](err); ok {
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
		schema, required, printerColumns, err := t.buildCustomTypeSchema(name, spec)
		if err != nil {
			return fmt.Errorf("building schema for %s: %w", name, err)
		}
		t.customTypes[name] = customType{
			Schema:         *schema,
			Required:       required,
			PrinterColumns: printerColumns,
		}
	}

	return nil
}

// buildCustomTypeSchema builds a schema for a custom type definition.
// Returns the schema and whether the type has required=true marker.
// Precondition: spec is string or map[string]interface{} (parseSpec validates this).
func (t *transformer) buildCustomTypeSchema(name string, spec interface{}) (*extv1.JSONSchemaProps, bool, []PrinterColumn, error) {
	switch val := spec.(type) {
	case string:
		// Type alias: "MyType": "string | default=foo"
		dummyParent := &extv1.JSONSchemaProps{}
		schema, printerColumns, err := t.buildFieldFromString(name, val, dummyParent, nil)
		if err != nil {
			return nil, false, nil, err
		}
		required := len(dummyParent.Required) > 0
		return schema, required, printerColumns, nil
	default:
		// Struct type: "MyType": { "field": "string" }
		schema, printerColumns, err := t.buildSchema(val.(map[string]interface{}), nil)
		if err != nil {
			return nil, false, nil, err
		}
		return schema, false, printerColumns, nil
	}
}

func (t *transformer) buildSchema(spec map[string]interface{}, path []string) (*extv1.JSONSchemaProps, []PrinterColumn, error) {
	schema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: make(map[string]extv1.JSONSchemaProps),
	}

	childHasDefault := false
	var printerColumns []PrinterColumn

	fieldNames := make([]string, 0, len(spec))
	for fieldName := range spec {
		fieldNames = append(fieldNames, fieldName)
	}
	sort.Strings(fieldNames)

	for _, fieldName := range fieldNames {
		fieldSpec := spec[fieldName]
		fieldPath := extendPath(path, fieldName)
		fieldSchema, fieldPrinterColumns, err := t.buildFieldSchema(fieldName, fieldSpec, schema, fieldPath)
		if err != nil {
			return nil, nil, fmt.Errorf("field %s: %w", fieldName, err)
		}
		schema.Properties[fieldName] = *fieldSchema
		printerColumns = append(printerColumns, fieldPrinterColumns...)

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

	return schema, printerColumns, nil
}

func (t *transformer) buildFieldSchema(name string, spec interface{}, parent *extv1.JSONSchemaProps, path []string) (*extv1.JSONSchemaProps, []PrinterColumn, error) {
	switch val := spec.(type) {
	case string:
		return t.buildFieldFromString(name, val, parent, path)
	case map[string]interface{}:
		return t.buildSchema(val, path)
	default:
		return nil, nil, fmt.Errorf("unexpected type: %T", spec)
	}
}

func (t *transformer) buildFieldFromString(name, fieldValue string, parent *extv1.JSONSchemaProps, path []string) (*extv1.JSONSchemaProps, []PrinterColumn, error) {
	typ, markers, err := ParseField(fieldValue)
	if err != nil {
		return nil, nil, err
	}

	schema, err := typ.Schema(t)
	if err != nil {
		return nil, nil, err
	}

	var printerColumns []PrinterColumn

	// Check if this is a custom type that has required=true
	if custom, ok := typ.(types.Custom); ok {
		customType := t.customTypes[string(custom)]
		if customType.Required {
			parent.Required = append(parent.Required, name)
		}
	}

	printConfig, err := parsePrinterColumnConfig(markers)
	if err != nil {
		return nil, nil, err
	}

	if err := applyMarkers(schema, markers, name, parent); err != nil {
		return nil, nil, err
	}

	customTypesPrinterColumns, err := t.printerColumnsForType(typ)
	if err != nil {
		return nil, nil, err
	}
	if len(customTypesPrinterColumns) > 0 {
		if printConfig.Enabled && len(customTypesPrinterColumns) > 1 {
			return nil, nil, fmt.Errorf("printColumn on field %q references custom type with %d printer columns; title override is only allowed when the custom type produces exactly one column", name, len(customTypesPrinterColumns))
		}
		overrideTitle := ""
		if printConfig.Enabled && len(customTypesPrinterColumns) == 1 {
			overrideTitle = printConfig.Title
		}
		for _, column := range customTypesPrinterColumns {
			expandedColumn := PrinterColumn{
				Path:       extendPath(path, column.Path...),
				TargetType: column.TargetType,
				Title:      column.Title,
			}
			if overrideTitle != "" {
				expandedColumn.Title = overrideTitle
			}
			printerColumns = append(printerColumns, expandedColumn)
		}
		return schema, printerColumns, nil
	}

	if printConfig.Enabled {
		printerColumns = append(printerColumns, PrinterColumn{
			Path:       append([]string(nil), path...),
			TargetType: schema.Type,
			Title:      printConfig.Title,
		})
	}

	return schema, printerColumns, nil
}

func (t *transformer) printerColumnsForType(typ types.Type) ([]PrinterColumn, error) {
	switch val := typ.(type) {
	case types.Custom:
		return t.customTypes[string(val)].PrinterColumns, nil
	case types.Slice:
		printerColumns, err := t.printerColumnsForType(val.Elem)
		if err != nil {
			return nil, err
		}
		if len(printerColumns) > 0 {
			return nil, fmt.Errorf("printColumn markers inside array types are not supported")
		}
		return nil, nil
	case types.Map:
		printerColumns, err := t.printerColumnsForType(val.Value)
		if err != nil {
			return nil, err
		}
		if len(printerColumns) > 0 {
			return nil, fmt.Errorf("printColumn markers inside map types are not supported")
		}
		return nil, nil
	default:
		return nil, nil
	}
}

func extendPath(path []string, segments ...string) []string {
	extended := make([]string, 0, len(path)+len(segments))
	extended = append(extended, path...)
	extended = append(extended, segments...)
	return extended
}
