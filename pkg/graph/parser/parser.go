// Copyright 2025 The Kube Resource Orchestrator Authors
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

package parser

import (
	"fmt"
	"slices"
	"strings"

	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kro-run/kro/pkg/graph/variable"
)

const (
	xKubernetesPreserveUnknownFields = "x-kubernetes-preserve-unknown-fields"
	xKubernetesIntOrString           = "x-kubernetes-int-or-string"
	schemaTypeAny                    = "any"
)

// ParseResource extracts CEL expressions from a resource based on
// the schema. The resource is expected to be a map[string]interface{}.
//
// Note that this function will also validate the resource against the schema
// and return an error if the resource does not match the schema. When CEL
// expressions are found, they are extracted and returned with the expected
// type of the field (inferred from the schema).
func ParseResource(resource map[string]interface{}, resourceSchema *spec.Schema) ([]variable.FieldDescriptor, error) {
	return parseResource(resource, resourceSchema, "")
}

// parseResource is a helper function that recursively extracts CEL expressions
// from a resource. It uses a depthh first search to traverse the resource and
// extract expressions from string fields
func parseResource(resource interface{}, schema *spec.Schema, path string) ([]variable.FieldDescriptor, error) {
	if err := validateSchema(schema, path); err != nil {
		return nil, err
	}

	expectedTypes, err := getExpectedTypes(schema)
	if err != nil {
		return nil, err
	}

	switch field := resource.(type) {
	case map[string]interface{}:
		return parseObject(field, schema, path, expectedTypes)
	case []interface{}:
		return parseArray(field, schema, path, expectedTypes)
	case string:
		return parseString(field, schema, path, expectedTypes)
	case nil:
		return nil, nil
	default:
		return parseScalarTypes(field, schema, path, expectedTypes)
	}
}

func getExpectedTypes(schema *spec.Schema) ([]string, error) {
	// Handle extensions (like x-kubernetes-int-or-string)
	if types, found := handleSchemaExtensions(schema); found {
		return types, nil
	}

	// Handle composite schemas (like OneOf, AnyOf)
	if types, found := handleCompositeSchemas(schema); found {
		return types, nil
	}

	// Handle direct type definitions
	if len(schema.Type) > 0 && schema.Type[0] != "" {
		return schema.Type, nil
	}

	// Handle additional properties
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Allows {
		// NOTE(a-hilaly): I don't like the type "any", we might want to change this to "object"
		// in the future; just haven't really thought about it yet.
		// Basically "any" means that the field can be of any type, and we have to check
		// the ExpectedSchema field.
		return []string{schemaTypeAny}, nil
	}

	return nil, fmt.Errorf("unknown schema type")
}

// handleSchemaExtensions processes Kubernetes-specific schema extensions
// and returns appropriate types if extensions are present.
func handleSchemaExtensions(schema *spec.Schema) ([]string, bool) {
	// Handle "x-kubernetes-preserve-unknown-fields" extension
	if hasStructuralSchemaMarkerEnabled(schema, xKubernetesPreserveUnknownFields) {
		return []string{schemaTypeAny}, true
	}

	// Handle "x-kubernetes-int-or-string" extension
	if hasStructuralSchemaMarkerEnabled(schema, xKubernetesIntOrString) {
		return []string{"string", "integer"}, true
	}

	return nil, false
}

// handleCompositeSchemas processes OneOf and AnyOf schemas
// and returns collected types if present.
func handleCompositeSchemas(schema *spec.Schema) ([]string, bool) {
	// Handle OneOf schemas
	if len(schema.OneOf) > 0 {
		types := collectTypesFromSubSchemas(schema.OneOf)
		if len(types) > 0 {
			return types, true
		}
	}

	// Handle AnyOf schemas
	if len(schema.AnyOf) > 0 {
		types := collectTypesFromSubSchemas(schema.AnyOf)
		if len(types) > 0 {
			return types, true
		}
	}

	return nil, false
}

// collectTypesFromSubSchemas extracts types from a slice of schemas,
// handling structural constraints like Required and Not.
func collectTypesFromSubSchemas(subSchemas []spec.Schema) []string {
	var types []string

	for _, subSchema := range subSchemas {
		// If there are structural constraints, inject object type
		if len(subSchema.Required) > 0 || subSchema.Not != nil {
			if !slices.Contains(types, "object") {
				types = append(types, "object")
			}
		}
		// Collect types if present
		if len(subSchema.Type) > 0 {
			for _, t := range subSchema.Type {
				if t != "" && !slices.Contains(types, t) {
					types = append(types, t)
				}
			}
		}
	}

	return types
}

func validateSchema(schema *spec.Schema, path string) error {
	if schema == nil {
		return fmt.Errorf("schema is nil for path %s", path)
	}

	if hasStructuralSchemaMarkerEnabled(schema, xKubernetesPreserveUnknownFields) {
		return nil
	}

	// Ensure the schema has at least one valid construct
	if len(schema.Type) == 0 && len(schema.OneOf) == 0 && len(schema.AnyOf) == 0 && schema.AdditionalProperties == nil {
		return fmt.Errorf("schema at path %s has no valid type, OneOf, AnyOf, or AdditionalProperties", path)
	}
	return nil
}

func parseObject(field map[string]interface{}, schema *spec.Schema, path string, expectedTypes []string) ([]variable.FieldDescriptor, error) {
	// Look for vendor schema extensions first
	if len(schema.VendorExtensible.Extensions) > 0 {
		// If the schema has the x-kubernetes-preserve-unknown-fields extension, we need to parse
		// this field using the schemaless parser. This allows us to extract CEL expressions from
		// fields that don't have a strict schema definition, while still preserving any unknown
		// fields. This is particularly important for handling custom resources and fields that
		// may contain arbitrary nested structures with potential CEL expressions.
		if hasStructuralSchemaMarkerEnabled(schema, xKubernetesPreserveUnknownFields) {
			expressions, err := parseSchemalessResource(field, path)
			if err != nil {
				return nil, err
			}
			return expressions, nil
		}
	}

	if !slices.Contains(expectedTypes, "object") && (schema.AdditionalProperties == nil || !schema.AdditionalProperties.Allows) {
		return nil, fmt.Errorf("expected object type or AdditionalProperties allowed for path %s, got %v", path, field)
	}

	var expressionsFields []variable.FieldDescriptor
	for fieldName, value := range field {
		fieldSchema, err := getFieldSchema(schema, fieldName)
		if err != nil {
			return nil, fmt.Errorf("error getting field schema for path %s: %v", path+"."+fieldName, err)
		}
		fieldPath := joinPathAndFieldName(path, fieldName)
		fieldExpressions, err := parseResource(value, fieldSchema, fieldPath)
		if err != nil {
			return nil, err
		}
		expressionsFields = append(expressionsFields, fieldExpressions...)
	}
	return expressionsFields, nil
}

func parseArray(field []interface{}, schema *spec.Schema, path string, expectedTypes []string) ([]variable.FieldDescriptor, error) {
	// Look for vendor schema extensions first
	if len(schema.VendorExtensible.Extensions) > 0 {
		// If the schema has the x-kubernetes-preserve-unknown-fields extension, we need to parse
		// this field using the schemaless parser. This allows us to extract CEL expressions from
		// fields that don't have a strict schema definition, while still preserving any unknown
		// fields. This is particularly important for handling custom resources and fields that
		// may contain arbitrary nested structures with potential CEL expressions.
		if hasStructuralSchemaMarkerEnabled(schema, xKubernetesPreserveUnknownFields) {
			expressions, err := parseSchemalessResource(field, path)
			if err != nil {
				return nil, err
			}
			return expressions, nil
		}
	}

	if !slices.Contains(expectedTypes, "array") {
		return nil, fmt.Errorf("expected array type for path %s, got %v", path, field)
	}

	itemSchema, err := getArrayItemSchema(schema, path)
	if err != nil {
		return nil, err
	}

	var expressionsFields []variable.FieldDescriptor
	for i, item := range field {
		itemPath := fmt.Sprintf("%s[%d]", path, i)
		itemExpressions, err := parseResource(item, itemSchema, itemPath)
		if err != nil {
			return nil, err
		}
		expressionsFields = append(expressionsFields, itemExpressions...)
	}
	return expressionsFields, nil
}

func parseString(field string, schema *spec.Schema, path string, expectedTypes []string) ([]variable.FieldDescriptor, error) {
	ok, err := isStandaloneExpression(field)
	if err != nil {
		return nil, err
	}

	if ok {
		return []variable.FieldDescriptor{{
			Expressions:          []string{strings.Trim(field, "${}")},
			ExpectedTypes:        expectedTypes,
			ExpectedSchema:       schema,
			Path:                 path,
			StandaloneExpression: true,
		}}, nil
	}

	if !slices.Contains(expectedTypes, "string") && !slices.Contains(expectedTypes, schemaTypeAny) {
		return nil, fmt.Errorf("expected string type or AdditionalProperties for path %s, got %v", path, field)
	}

	expressions, err := extractExpressions(field)
	if err != nil {
		return nil, err
	}
	if len(expressions) > 0 {
		return []variable.FieldDescriptor{{
			Expressions:   expressions,
			ExpectedTypes: expectedTypes,
			Path:          path,
		}}, nil
	}
	return nil, nil
}

func parseScalarTypes(field interface{}, _ *spec.Schema, path string, expectedTypes []string) ([]variable.FieldDescriptor, error) {
	// perform type checks for scalar types
	switch {
	case slices.Contains(expectedTypes, "any"):
		// Skip checks for expected == provided type
	case slices.Contains(expectedTypes, "number"):
		if !isNumber(field) {
			return nil, fmt.Errorf("expected number type for path %s, got %T", path, field)
		}
	case slices.Contains(expectedTypes, "int"), slices.Contains(expectedTypes, "integer"):
		if !isInteger(field) {
			return nil, fmt.Errorf("expected integer type for path %s, got %T", path, field)
		}
	case slices.Contains(expectedTypes, "boolean"), slices.Contains(expectedTypes, "bool"):
		if _, ok := field.(bool); !ok {
			return nil, fmt.Errorf("expected boolean type for path %s, got %T", path, field)
		}
	default:
		return nil, fmt.Errorf("unexpected type for path %s: %T, expectedTypes: [%s]", path, field, strings.Join(expectedTypes, ","))
	}
	return nil, nil
}

func getFieldSchema(schema *spec.Schema, field string) (*spec.Schema, error) {
	if schema.Properties != nil {
		if fieldSchema, ok := schema.Properties[field]; ok {
			return &fieldSchema, nil
		}
	}

	if schema.AdditionalProperties != nil {
		if schema.AdditionalProperties.Schema != nil {
			return schema.AdditionalProperties.Schema, nil
		} else if schema.AdditionalProperties.Allows {
			return &spec.Schema{}, nil
		}
	}

	return nil, fmt.Errorf("schema not found for field %s", field)
}

func getArrayItemSchema(schema *spec.Schema, path string) (*spec.Schema, error) {
	if schema.Items != nil && schema.Items.Schema != nil {
		return schema.Items.Schema, nil
	}
	if schema.Items != nil && schema.Items.Schema != nil && len(schema.Items.Schema.Properties) > 0 {
		return &spec.Schema{
			SchemaProps: spec.SchemaProps{
				Type:       []string{"object"},
				Properties: schema.Properties,
			},
		}, nil
	}
	return nil, fmt.Errorf("invalid array schema for path %s: neither Items.Schema nor Properties are defined", path)
}

func isNumber(v interface{}) bool {
	return isInteger(v) || isFloat(v)
}

func isFloat(v interface{}) bool {
	switch v.(type) {
	case float32, float64:
		return true
	default:
		return false
	}
}

func isInteger(v interface{}) bool {
	switch v.(type) {
	case int, int8, int32, int64:
		return true
	default:
		return false
	}
}

// joinPathAndField appends a field name to a path. If the fieldName contains
// a dot or is empty, the path will be appended using ["fieldName"] instead of
// .fieldName to avoid ambiguity and simplify parsing back the path.
func joinPathAndFieldName(path, fieldName string) string {
	if fieldName == "" || strings.Contains(fieldName, ".") {
		return fmt.Sprintf("%s[%q]", path, fieldName)
	}
	if path == "" {
		return fieldName
	}
	return fmt.Sprintf("%s.%s", path, fieldName)
}

// hasStructuralSchemaMarkerEnabled checks if a schema has a specific marker enabled.
func hasStructuralSchemaMarkerEnabled(schema *spec.Schema, marker string) bool {
	if ext, ok := schema.VendorExtensible.Extensions[marker]; ok {
		if enabled, ok := ext.(bool); ok && enabled {
			return true
		}
	}
	return false
}
