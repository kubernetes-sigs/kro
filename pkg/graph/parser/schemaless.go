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

package parser

import (
	"fmt"
	"strings"

	"github.com/google/cel-go/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// ParseSchemalessResource extracts CEL expressions without a schema, this is useful
// when the schema is not available. e.g RGI statuses
func ParseSchemalessResource(resource map[string]interface{}) ([]variable.FieldDescriptor, error) {
	return parseSchemalessResource(resource, "")
}

// parseSchemalessResource is a helper function that recursively
// extracts expressions from a resource. It uses a depth first search to traverse
// the resource and extract expressions from string fields
func parseSchemalessResource(resource interface{}, path string) ([]variable.FieldDescriptor, error) {
	var expressionsFields []variable.FieldDescriptor
	switch field := resource.(type) {
	case map[string]interface{}:
		for field, value := range field {
			fieldPath := joinPathAndFieldName(path, field)
			fieldExpressions, err := parseSchemalessResource(value, fieldPath)
			if err != nil {
				return nil, err
			}
			expressionsFields = append(expressionsFields, fieldExpressions...)
		}
	case []interface{}:
		for i, item := range field {
			itemPath := fmt.Sprintf("%s[%d]", path, i)
			itemExpressions, err := parseSchemalessResource(item, itemPath)
			if err != nil {
				return nil, err
			}
			expressionsFields = append(expressionsFields, itemExpressions...)
		}
	case string:
		ok, err := isStandaloneExpression(field)
		if err != nil {
			return nil, err
		}
		if ok {
			expr := strings.TrimPrefix(field, "${")
			expr = strings.TrimSuffix(expr, "}")
			expressionsFields = append(expressionsFields, variable.FieldDescriptor{
				Expressions:          []string{expr},
				ExpectedType:         cel.DynType, // No schema, so we use dynamic type
				Path:                 path,
				StandaloneExpression: true,
			})
		} else {
			expressions, err := extractExpressions(field)
			if err != nil {
				return nil, err
			}
			if len(expressions) > 0 {
				// String template in schemaless parsing - always produces string
				expressionsFields = append(expressionsFields, variable.FieldDescriptor{
					Expressions:          expressions,
					ExpectedType:         cel.StringType,
					Path:                 path,
					StandaloneExpression: false,
				})
			}
		}

	default:
		// Ignore other types
	}
	return expressionsFields, nil
}
