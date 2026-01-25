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
	"errors"
	"fmt"

	"github.com/google/cel-go/common/types"
	"github.com/google/cel-go/common/types/ref"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
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
	// Schema is the schema for the field. This is typically inferred by dry running
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

func addFieldToSchema(fd fieldDescriptor, schema *extv1.JSONSchemaProps) error {
	segments, err := fieldpath.Parse(fd.Path)
	if err != nil {
		return fmt.Errorf("failed to parse path %s: %w", fd.Path, err)
	}

	leaf := fd.Schema
	if leaf == nil {
		leaf = &extv1.JSONSchemaProps{Type: "string"}
	}
	addFieldToSchemaSegments(schema, segments, leaf)
	return nil
}

// ensureObjectSchema initializes a schema as an object type with properties.
func ensureObjectSchema(s *extv1.JSONSchemaProps) {
	if s.Type == "" {
		s.Type = "object"
	}
	if s.Properties == nil {
		s.Properties = make(map[string]extv1.JSONSchemaProps)
	}
}

// addFieldToSchemaSegments walks the parsed path segments, ensures the schema
// tree has the required object/array nodes, and writes the leaf schema.
func addFieldToSchemaSegments(
	current *extv1.JSONSchemaProps,
	segments []fieldpath.Segment,
	leaf *extv1.JSONSchemaProps,
) {
	if len(segments) == 0 {
		return
	}

	seg, rest := segments[0], segments[1:]

	// Array index segment
	if seg.Index >= 0 {
		current.Type = "array"
		if current.Items == nil || current.Items.Schema == nil {
			current.Items = &extv1.JSONSchemaPropsOrArray{Schema: &extv1.JSONSchemaProps{}}
		}
		if len(rest) == 0 {
			*current.Items.Schema = *leaf
			return
		}
		ensureObjectSchema(current.Items.Schema)
		addFieldToSchemaSegments(current.Items.Schema, rest, leaf)
		return
	}

	// Named field segment
	ensureObjectSchema(current)
	if len(rest) == 0 {
		current.Properties[seg.Name] = *leaf
		return
	}

	child := current.Properties[seg.Name]
	ensureObjectSchema(&child)
	addFieldToSchemaSegments(&child, rest, leaf)
	current.Properties[seg.Name] = child
}
