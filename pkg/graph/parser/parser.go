// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//	http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

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
	// Handle "x-kubernetes-int-or-string" extension
	if ext, ok := schema.VendorExtensible.Extensions[xKubernetesIntOrString]; ok {
		enabled, ok := ext.(bool)
		if !ok {
			return nil, fmt.Errorf("xKubernetesIntOrString extension is not a boolean")
		}
		if enabled {
			return []string{"string", "integer"}, nil
		}
	}

	// Handle OneOf schemas
	if len(schema.OneOf) > 0 {
		var types []string
		for _, subType := range schema.OneOf {
			types = append(types, subType.Type...)
		}
		return types, nil
	}

	// Handle AnyOf schemas
	if len(schema.AnyOf) > 0 {
		var types []string
		for _, subType := range schema.AnyOf {
			types = append(types, subType.Type...)
		}
		return types, nil
	}

	if schema.Type[0] != "" {
		return schema.Type, nil
	}
	if schema.AdditionalProperties != nil && schema.AdditionalProperties.Allows {
		// NOTE(a-hilaly): I don't like the type "any", we might want to change this to "object"
		// in the future; just haven't really thought about it yet.
		// Basically "any" means that the field can be of any type, and we have to check
		// the ExpectedSchema field.
		return []string{schemaTypeAny}, nil
	}
	return nil, fmt.Errorf("unknown schema type")
}

func sliceInclude(expectedTypes []string, expectedType string) bool {
	return slices.Contains(expectedTypes, expectedType)
}

func validateSchema(schema *spec.Schema, path string) error {
	if schema == nil {
		return fmt.Errorf("schema is nil for path %s", path)
	}
	// Ensure the schema has at least one valid construct
	if len(schema.Type) == 0 && len(schema.OneOf) == 0 && len(schema.AnyOf) == 0 && schema.AdditionalProperties == nil {
		return fmt.Errorf("schema at path %s has no valid type, OneOf, AnyOf, or AdditionalProperties", path)
	}
	return nil
}

func parseObject(field map[string]interface{}, schema *spec.Schema, path string, expectedTypes []string) ([]variable.FieldDescriptor, error) {
	if !sliceInclude(expectedTypes, "object") && (schema.AdditionalProperties == nil || !schema.AdditionalProperties.Allows) {
		return nil, fmt.Errorf("expected object type or AdditionalProperties allowed for path %s, got %v", path, field)
	}

	// Look for vendor schema extensions first
	if len(schema.VendorExtensible.Extensions) > 0 {
		// If the schema has the x-kubernetes-preserve-unknown-fields extension, we need to parse
		// this field using the schemaless parser. This allows us to extract CEL expressions from
		// fields that don't have a strict schema definition, while still preserving any unknown
		// fields. This is particularly important for handling custom resources and fields that
		// may contain arbitrary nested structures with potential CEL expressions.
		if enabled, ok := schema.VendorExtensible.Extensions[xKubernetesPreserveUnknownFields]; ok && enabled.(bool) {
			expressions, err := parseSchemalessResource(field, path)
			if err != nil {
				return nil, err
			}
			return expressions, nil
		}
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
	if !sliceInclude(expectedTypes, "array") {
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

	if !sliceInclude(expectedTypes, "string") && !sliceInclude(expectedTypes, schemaTypeAny) {
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
	case sliceInclude(expectedTypes, "number"):
		if _, ok := field.(float64); !ok {
			return nil, fmt.Errorf("expected number type for path %s, got %T", path, field)
		}
	case sliceInclude(expectedTypes, "int"), sliceInclude(expectedTypes, "integer"):
		if !isInteger(field) {
			return nil, fmt.Errorf("expected integer type for path %s, got %T", path, field)
		}
	case sliceInclude(expectedTypes, "boolean"), sliceInclude(expectedTypes, "bool"):
		if _, ok := field.(bool); !ok {
			return nil, fmt.Errorf("expected boolean type for path %s, got %T", path, field)
		}
	default:
		return nil, fmt.Errorf("unexpected type for path %s: %T", path, field)
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

func isInteger(v interface{}) bool {
	switch v.(type) {
	case int, int64, int32:
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
