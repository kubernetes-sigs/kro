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

package schema

import (
	"errors"
	"fmt"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kro-run/kro/pkg/graph/fieldpath"
)

// fieldDescriptor represents a field in an OpenAPI schema. Typically this field
// Isn't yet defined in the schema, but we want to add it to the schema.
//
// This is mainly used to generate the proper OpenAPI Schema for the status field
// of a CRD (Created via a ResourceGraphDefinition).
//
// For example, given the following status definition in simpleschema standard:
// status:
//
//	clusterARN: ${cluster.status.ClusterARN}
//	someStruct:
//	  someNestedField: ${cluster.status.someStruct.someNestedField}
//	  someArrayField:
//	  - ${cluster.status.someStruct.someArrayField[0]}
//
// We would generate the following FieldDescriptors:
// - fieldDescriptor{Path: "status.clusterARN", Schema: &extv1.JSONSchemaProps{Type: "string"}}
// - fieldDescriptor{Path: "status.someStruct.someNestedField", Schema: &extv1.JSONSchemaProps{Type: "string"}}
// - fieldDescriptor{Path: "status.someStruct.someArrayField[0]", Schema: &extv1.JSONSchemaProps{Type: "string"}}
//
// These FieldDescriptors can then be used to generate the full OpenAPI schema for
// the status field.
type fieldDescriptor struct {
	// Path is a string representing the location of the field in the schema structure.
	// It uses a dot-separated notation similar to JSONPath.
	//
	// Important: This path may include parent fields for which we don't have explicit
	// schema definitions. The structure of these parent fields (whether they're
	// objects or arrays) is inferred from the path syntax:
	// - Simple dot notation (e.g "parent.child") implies object nesting
	// - Square brackets (e.g "items[0]") implies array structures
	//
	// Examples:
	// - "status" : A top-level field named "status"
	// - "spec.replicas" : A "replicas" field nested under "spec"
	// - "status.conditions[0].type" : A "type" field in the in the items of a "conditions"
	//   array nested under "status".
	//
	// The path is typically found by calling `parser.ParseSchemalessResource` see the
	// `typesystem/parser` package for more information.
	Path string
	// Schema is the schema for the field. This is typically inferred by dry runing
	// the CEL expression that generates the field value, then converting the result
	// into an OpenAPI schema.
	Schema *extv1.JSONSchemaProps
}

var (
	// ErrInvalidEvaluationTypes is returned when the evaluation types are not valid
	// for generating a schema.
	ErrInvalidEvaluationTypes = errors.New("invalid evaluation type")
)

func GenerateSchemaFromEvals(evals map[string][]ref.Val) (*extv1.JSONSchemaProps, error) {
	fieldDescriptors := make([]fieldDescriptor, 0, len(evals))

	for path, evaluationValues := range evals {
		if !areValidExpressionEvals(evaluationValues) {
			return nil, fmt.Errorf("invalid evaluation types at %v: %w", path, ErrInvalidEvaluationTypes)
		}
		exprSchema, err := inferSchemaFromCELValue(evaluationValues[0])
		if err != nil {
			return nil, fmt.Errorf("failed to infer schema type: %w", err)
		}
		fieldDescriptors = append(fieldDescriptors, fieldDescriptor{
			Path:   path,
			Schema: exprSchema,
		})
	}

	return generateJSONSchemaFromFieldDescriptors(fieldDescriptors)
}

// areValidExpressionEvals returns true if all the evaluation types
// are the same, false otherwise.
func areValidExpressionEvals(evaluationValues []ref.Val) bool {
	if len(evaluationValues) == 0 {
		return false // no expressions is problematic
	}
	if len(evaluationValues) == 1 {
		return true // Single value is always valid
	}
	// The only way a multi-value expression is valid is if all the values
	// are of type string. Imagine.. you can't really combine two arrays
	// using two different CEL expression in a meaningful way.
	// e.g: "${a}${b}"" where a and b are arrays.
	firstType := evaluationValues[0].Type()
	if firstType != types.StringType {
		return false
	}
	for _, eval := range evaluationValues[1:] {
		if eval.Type() != firstType {
			return false
		}
	}
	return true
}

// generateJSONSchemaFromFieldDescriptors generates a JSONSchemaProps from a list of StatusStructureParts
func generateJSONSchemaFromFieldDescriptors(fieldDescriptors []fieldDescriptor) (*extv1.JSONSchemaProps, error) {
	rootSchema := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: make(map[string]extv1.JSONSchemaProps),
	}

	for _, part := range fieldDescriptors {
		if err := addFieldToSchema(part, rootSchema); err != nil {
			return nil, err
		}
	}

	return rootSchema, nil
}

func addFieldToSchema(fieldDescriptor fieldDescriptor, schema *extv1.JSONSchemaProps) error {
	segments, err := fieldpath.Parse(fieldDescriptor.Path)
	if err != nil {
		return fmt.Errorf("failed to parse path %s: %w", fieldDescriptor.Path, err)
	}

	currentSchema := schema

	for i, segment := range segments {
		isLast := i == len(segments)-1

		if segment.Index >= 0 {
			// Handle array segment
			if currentSchema.Type != "array" {
				currentSchema.Type = "array"
				currentSchema.Items = &extv1.JSONSchemaPropsOrArray{
					Schema: &extv1.JSONSchemaProps{
						Type:       "object",
						Properties: make(map[string]extv1.JSONSchemaProps),
					},
				}
			}
			currentSchema = currentSchema.Items.Schema
		}

		if isLast {
			// This is the final segment of the path, add the schema here
			if fieldDescriptor.Schema != nil {
				if segment.Index >= 0 {
					*currentSchema = *fieldDescriptor.Schema
				} else {
					currentSchema.Properties[segment.Name] = *fieldDescriptor.Schema
				}
			} else {
				// If no schema is provided, default to a string type
				defaultSchema := extv1.JSONSchemaProps{Type: "string"}
				if segment.Index >= 0 {
					*currentSchema = defaultSchema
				} else {
					currentSchema.Properties[segment.Name] = defaultSchema
				}
			}
		} else {
			// This is an intermediate segment of the path
			if segment.Index < 0 {
				if _, exists := currentSchema.Properties[segment.Name]; !exists {
					currentSchema.Properties[segment.Name] = extv1.JSONSchemaProps{
						Type:       "object",
						Properties: make(map[string]extv1.JSONSchemaProps),
					}
				}
				s := currentSchema.Properties[segment.Name]
				currentSchema = &s
			}
		}
	}

	return nil
}
