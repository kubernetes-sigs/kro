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

package schema

import (
	"fmt"

	"github.com/google/cel-go/cel"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/utils/ptr"
)

// GenerateSchemaFromCELTypes generates a JSONSchemaProps from a map of CEL types.
// The provider is used to recursively introspect struct types and extract all fields.
func GenerateSchemaFromCELTypes(typeMap map[string]*cel.Type, provider *apiservercel.DeclTypeProvider) (*extv1.JSONSchemaProps, error) {
	fieldDescriptors := make([]fieldDescriptor, 0, len(typeMap))

	for path, celType := range typeMap {
		exprSchema, err := inferSchemaFromCELType(celType, provider)
		if err != nil {
			return nil, fmt.Errorf("failed to infer schema from type at %v: %w", path, err)
		}
		fieldDescriptors = append(fieldDescriptors, fieldDescriptor{
			Path:   path,
			Schema: exprSchema,
		})
	}

	return generateJSONSchemaFromFieldDescriptors(fieldDescriptors)
}

// primitiveTypeToSchema converts a CEL primitive type name to a JSONSchemaProps.
func primitiveTypeToSchema(typeName string) (*extv1.JSONSchemaProps, bool) {
	switch typeName {
	case "bool":
		return &extv1.JSONSchemaProps{Type: "boolean"}, true
	case "int", "uint":
		return &extv1.JSONSchemaProps{Type: "integer"}, true
	case "double":
		return &extv1.JSONSchemaProps{Type: "number"}, true
	case "string":
		return &extv1.JSONSchemaProps{Type: "string"}, true
	case "bytes":
		return &extv1.JSONSchemaProps{Type: "string", Format: "byte"}, true
	case "null_type":
		return &extv1.JSONSchemaProps{
			Description:            "null type - any value allowed",
			XPreserveUnknownFields: ptr.To(true),
		}, true
	default:
		return nil, false
	}
}

// inferSchemaFromCELType converts a CEL type to an OpenAPI schema.
// For struct types, it uses the provider to recursively extract all fields.
func inferSchemaFromCELType(celType *cel.Type, provider *apiservercel.DeclTypeProvider) (*extv1.JSONSchemaProps, error) {
	if celType == nil {
		return nil, fmt.Errorf("type is nil")
	}

	typeName := celType.String()

	// Handle primitive types
	if schema, ok := primitiveTypeToSchema(typeName); ok {
		return schema, nil
	}

	// Handle complex types based on kind
	switch celType.Kind() {
	case cel.ListKind:
		if provider != nil {
			declType, found := provider.FindDeclType(typeName)
			if found {
				visited := make(map[string]bool)
				return extractSchemaFromDeclTypeWithCycleDetection(declType, visited)
			}
		}
		// Fallback: try to infer from CEL type parameters
		if len(celType.Parameters()) > 0 {
			elemType := celType.Parameters()[0]
			elemSchema, err := inferSchemaFromCELType(elemType, provider)
			if err != nil {
				return nil, fmt.Errorf("failed to infer array element type: %w", err)
			}
			return &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: elemSchema,
				},
			}, nil
		}
		return &extv1.JSONSchemaProps{Type: "array"}, nil

	case cel.MapKind:
		if provider != nil {
			declType, found := provider.FindDeclType(typeName)
			if found {
				visited := make(map[string]bool)
				return extractSchemaFromDeclTypeWithCycleDetection(declType, visited)
			}
		}
		// Fallback: try to infer from CEL type parameters
		if len(celType.Parameters()) > 1 {
			valueType := celType.Parameters()[1]
			valueSchema, err := inferSchemaFromCELType(valueType, provider)
			if err != nil {
				return nil, fmt.Errorf("failed to infer map value type: %w", err)
			}
			return &extv1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Schema: valueSchema,
				},
			}, nil
		}
		return &extv1.JSONSchemaProps{
			Type:                 "object",
			AdditionalProperties: &extv1.JSONSchemaPropsOrBool{Allows: true},
		}, nil

	case cel.StructKind:
		if provider != nil {
			declType, found := provider.FindDeclType(typeName)
			if found {
				visited := make(map[string]bool)
				return extractSchemaFromDeclTypeWithCycleDetection(declType, visited)
			}
		}
		// Fallback: permissive object schema
		return &extv1.JSONSchemaProps{
			Type:                 "object",
			AdditionalProperties: &extv1.JSONSchemaPropsOrBool{Allows: true},
		}, nil

	case cel.DynKind:
		return &extv1.JSONSchemaProps{
			XPreserveUnknownFields: ptr.To(true),
		}, nil

	default:
		// Unknown type - be permissive
		return &extv1.JSONSchemaProps{
			Description:            fmt.Sprintf("unknown CEL type: %s", typeName),
			XPreserveUnknownFields: ptr.To(true),
		}, nil
	}
}

// extractSchemaFromDeclTypeWithCycleDetection recursively extracts all fields from a DeclType to build an OpenAPI schema.
// It tracks visited types to prevent infinite recursion on cyclic type definitions.
func extractSchemaFromDeclTypeWithCycleDetection(declType *apiservercel.DeclType, visited map[string]bool) (*extv1.JSONSchemaProps, error) {
	if declType == nil {
		return &extv1.JSONSchemaProps{XPreserveUnknownFields: ptr.To(true)}, nil
	}

	// Get type identifier for cycle detection
	celType := declType.CelType()
	if celType != nil {
		typeName := celType.String()

		if visited[typeName] {
			return nil, fmt.Errorf("cyclic type reference detected: %s", typeName)
		}

		visited[typeName] = true
		defer delete(visited, typeName)
	}

	// Handle different DeclType kinds
	if declType.IsList() {
		elemType := declType.ElemType
		if elemType != nil {
			elemSchema, err := extractSchemaFromDeclTypeWithCycleDetection(elemType, visited)
			if err != nil {
				return nil, fmt.Errorf("failed to extract array element schema: %w", err)
			}
			return &extv1.JSONSchemaProps{
				Type: "array",
				Items: &extv1.JSONSchemaPropsOrArray{
					Schema: elemSchema,
				},
			}, nil
		}
		return &extv1.JSONSchemaProps{Type: "array"}, nil
	}

	if declType.IsMap() {
		valueType := declType.ElemType
		if valueType != nil {
			valueSchema, err := extractSchemaFromDeclTypeWithCycleDetection(valueType, visited)
			if err != nil {
				return nil, fmt.Errorf("failed to extract map value schema: %w", err)
			}
			return &extv1.JSONSchemaProps{
				Type: "object",
				AdditionalProperties: &extv1.JSONSchemaPropsOrBool{
					Schema: valueSchema,
				},
			}, nil
		}
		return &extv1.JSONSchemaProps{
			Type:                 "object",
			AdditionalProperties: &extv1.JSONSchemaPropsOrBool{Allows: true},
		}, nil
	}

	if !declType.IsObject() {
		celType := declType.CelType()
		if celType != nil {
			typeName := celType.String()
			if schema, ok := primitiveTypeToSchema(typeName); ok {
				return schema, nil
			}
		}
		return &extv1.JSONSchemaProps{XPreserveUnknownFields: ptr.To(true)}, nil
	}

	schema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: make(map[string]extv1.JSONSchemaProps),
	}

	for _, field := range declType.Fields {
		if field == nil {
			continue
		}

		fieldSchema, err := extractSchemaFromDeclTypeWithCycleDetection(field.Type, visited)
		if err != nil {
			return nil, fmt.Errorf("failed to extract schema for field %s: %w", field.Name, err)
		}

		if fieldSchema != nil {
			schema.Properties[field.Name] = *fieldSchema
		}
	}

	return schema, nil
}
