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
	"strconv"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// ParseSchemalessResource extracts CEL expressions without a schema, this is useful
// when the schema is not available. e.g RGI statuses
func ParseSchemalessResource(resource map[string]any) ([]variable.FieldDescriptor, []string, error) {
	return parseSchemalessResource(resource, "")
}

// parseSchemalessResource is a helper function that recursively
// extracts expressions from a resource. It uses a depth first search to traverse
// the resource and extract expressions from string fields
func parseSchemalessResource(resource any, path string) ([]variable.FieldDescriptor, []string, error) {
	var expressionsFields []variable.FieldDescriptor
	var allPlainFieldPaths []string
	switch field := resource.(type) {
	case map[string]any:
		for field, value := range field {
			fieldPath := joinPathAndFieldName(path, field)
			fieldExpressions, plainFieldPaths, err := parseSchemalessResource(value, fieldPath)
			if err != nil {
				return nil, nil, err
			}
			expressionsFields = append(expressionsFields, fieldExpressions...)
			allPlainFieldPaths = append(allPlainFieldPaths, plainFieldPaths...)
		}
	case []any:
		for i, item := range field {
			itemPath := path + "[" + strconv.Itoa(i) + "]"
			itemExpressions, plainFieldPaths, err := parseSchemalessResource(item, itemPath)
			if err != nil {
				return nil, nil, err
			}
			expressionsFields = append(expressionsFields, itemExpressions...)
			allPlainFieldPaths = append(allPlainFieldPaths, plainFieldPaths...)
		}
	case string:
		matches, err := extractExpressions(field)
		if err != nil {
			return nil, nil, err
		}
		if len(matches) == 1 && matches[0].start == 0 && matches[0].end == len(field) {
			expressionsFields = append(expressionsFields, variable.FieldDescriptor{
				Expression: &krocel.Expression{Original: matches[0].expr},
				Path:       path,
			})
		} else if len(matches) > 0 {
			celExpr := buildStringTemplate(field, matches)
			expressionsFields = append(expressionsFields, variable.FieldDescriptor{
				Expression: &krocel.Expression{Original: celExpr, OriginalTemplate: field},
				Path:       path,
			})
		} else {
			allPlainFieldPaths = append(allPlainFieldPaths, path)
		}

	default:
		allPlainFieldPaths = append(allPlainFieldPaths, path)
	}
	return expressionsFields, allPlainFieldPaths, nil
}
