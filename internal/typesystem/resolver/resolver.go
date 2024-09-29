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

package resolver

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/aws-controllers-k8s/symphony/internal/typesystem/parser"
)

// ResolutionResult represents the result of resolving a single expression.
type ResolutionResult struct {
	Path     string
	Resolved bool
	Original string
	Replaced interface{}
	Error    error
}

// ResolutionSummary provides a summary of the resolution process.
type ResolutionSummary struct {
	TotalExpressions    int
	ResolvedExpressions int
	Results             []ResolutionResult
	Errors              []error
}

// Resolver handles the resolution of CEL expressions in Kubernetes resources.
type Resolver struct {
	// The orginal resource to be resolved. In symphony, this will typically
	// be a Kubernetes resource with some fields containing CEL expressions.
	resource map[string]interface{}
	// The data to be used for resolving the expressions. Other systems are
	// responsible for providing this only with available data aka CEL Expressions
	// we've been able to resolve.
	data map[string]interface{}
}

// NewResolver creates a new Resolver instance.
func NewResolver(resource map[string]interface{}, data map[string]interface{}) *Resolver {
	return &Resolver{
		resource: resource,
		data:     data,
	}
}

// Resolve processes all the given ExpressionFields and resolves their CEL expressions.
// It returns a ResolutionSummary containing information about the resolution process.
func (r *Resolver) Resolve(expressions []parser.ExpressionField) ResolutionSummary {
	summary := ResolutionSummary{
		TotalExpressions: len(expressions),
		Results:          make([]ResolutionResult, 0, len(expressions)),
	}

	for _, field := range expressions {
		result := r.resolveField(field)
		summary.Results = append(summary.Results, result)
		if result.Resolved {
			summary.ResolvedExpressions++
		}
		if result.Error != nil {
			summary.Errors = append(summary.Errors, result.Error)
		}
	}

	return summary
}

// resolveField handles the resolution of a single ExpressionField (one field) in
// the resource. It returns a ResolutionResult containing information about the
// resolution process
func (r *Resolver) resolveField(field parser.ExpressionField) ResolutionResult {
	result := ResolutionResult{
		Path:     field.Path,
		Original: fmt.Sprintf("%v", field.Expressions),
	}

	value, err := r.getValueFromPath(field.Path)
	if err != nil {
		// Not sure if these kind of errors should be fatal, these paths are produced
		// by the parser, so they should be valid. Maybe we should log them instead....
		result.Error = fmt.Errorf("error getting value: %v", err)
		return result
	}

	if field.OneShotCEL {
		resolvedValue, ok := r.data[strings.Trim(field.Expressions[0], "${}")]
		if !ok {
			result.Error = fmt.Errorf("no data provided for expression: %s", field.Expressions[0])
			return result
		}
		err = r.setValueAtPath(field.Path, resolvedValue)
		if err != nil {
			result.Error = fmt.Errorf("error setting value: %v", err)
			return result
		}
		result.Resolved = true
		result.Replaced = resolvedValue
	} else {
		strValue, ok := value.(string)
		if !ok {
			result.Error = fmt.Errorf("expected string value for path %s", field.Path)
			return result
		}

		replaced := strValue
		for _, expr := range field.Expressions {
			key := strings.Trim(expr, "${}")
			replacement, ok := r.data[key]
			if !ok {
				result.Error = fmt.Errorf("no data provided for expression: %s", expr)
				return result
			}
			replaced = strings.Replace(replaced, "${"+expr+"}", fmt.Sprintf("%v", replacement), -1)
		}

		err = r.setValueAtPath(field.Path, replaced)
		if err != nil {
			result.Error = fmt.Errorf("error setting value: %v", err)
			return result
		}
		result.Resolved = true
		result.Replaced = replaced
	}

	return result
}

// getValueFromPath retrieves a value from the resource using a dot separated path.
// NOTE(a-hilaly): this is very similar to the `setValueAtPath` function maybe
// we can refactor something here.
// getValueFromPath retrieves a value from the resource using a dot-separated path.
func (r *Resolver) getValueFromPath(path string) (interface{}, error) {
	path = strings.TrimPrefix(path, ".") // Remove leading dot if present
	parts := strings.Split(path, ".")
	current := interface{}(r.resource)

	for _, part := range parts {
		if strings.Contains(part, "[") && strings.Contains(part, "]") {
			// Handle array access
			arrayPart := strings.Split(part, "[")
			key := arrayPart[0]
			indexStr := strings.Trim(arrayPart[1], "]")
			index, err := strconv.Atoi(indexStr)
			if err != nil {
				return nil, fmt.Errorf("invalid array index: %s", indexStr)
			}

			// Check if current is a map
			currentMap, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("expected map for array access at path: %s", path)
			}

			array, ok := currentMap[key].([]interface{})
			if !ok {
				return nil, fmt.Errorf("path is not an array: %s", key)
			}

			if index < 0 || index >= len(array) {
				return nil, fmt.Errorf("array index out of bounds: %d", index)
			}

			current = array[index]
		} else {
			// Handle object access
			currentMap, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("expected map at path: %s", path)
			}

			value, ok := currentMap[part]
			if !ok {
				return nil, fmt.Errorf("key not found: %s in path: %s", part, path)
			}
			current = value
		}
	}

	return current, nil
}

// setValueAtPath sets a value in the resource using a dot-separated path.
func (r *Resolver) setValueAtPath(path string, value interface{}) error {
	path = strings.TrimPrefix(path, ".")
	parts := strings.Split(path, ".")
	current := r.resource

	for i, part := range parts {
		if strings.HasSuffix(part, "]") {
			// Handle array access
			openBracket := strings.LastIndex(part, "[")
			if openBracket == -1 {
				return fmt.Errorf("invalid array syntax in path: %s", path)
			}

			key := part[:openBracket]
			indexStr := part[openBracket+1 : len(part)-1]
			index, err := strconv.Atoi(indexStr)
			if err != nil {
				return fmt.Errorf("invalid array index in path: %s", path)
			}

			arr, ok := current[key].([]interface{})
			if !ok {
				if i == len(parts)-1 {
					// If this is the last part and its not an array, we need to create it
					// not sure if this is a valid case, but in theory we're supposed to.
					arr = make([]interface{}, index+1)
					current[key] = arr
				} else {
					return fmt.Errorf("expected array at key %s in path: %s", key, path)
				}
			}

			if index >= len(arr) {
				if i == len(parts)-1 {
					// another edge case that sounds weird, but we need to handle it.
					// the problem here is that we're trying to set a value in an array
					// that has a gap in the indexes, so we need to fill the gap with nils
					newArr := make([]interface{}, index+1)
					copy(newArr, arr)
					arr = newArr
					current[key] = arr
				} else {
					return fmt.Errorf("array index out of bounds in path: %s", path)
				}
			}

			if i == len(parts)-1 {
				// set the value
				arr[index] = value
				return nil
			}

			// move to the next level
			nextMap, ok := arr[index].(map[string]interface{})
			if !ok {
				nextMap = make(map[string]interface{})
				arr[index] = nextMap
			}
			current = nextMap
		} else {
			if i == len(parts)-1 {
				// This is the last part, set the value
				current[part] = value
				return nil
			}

			// Handle object access
			next, ok := current[part].(map[string]interface{})
			if !ok {
				next = make(map[string]interface{})
				current[part] = next
			}
			current = next
		}
	}

	return nil
}

func (r *Resolver) BlindSetValueAtPath(path string, value interface{}) error {
	return r.setExpressionToPath(path, value)
}

// setExpressionToPath sets a value in the resource using a dot-separated path.
// It constructs the path data structure if it doesn't exist.
func (r *Resolver) setExpressionToPath(path string, value interface{}) error {
	path = strings.TrimPrefix(path, ".")
	parts := strings.Split(path, ".")
	current := r.resource

	for i, part := range parts {
		if strings.HasSuffix(part, "]") {
			// Handle array access
			openBracket := strings.LastIndex(part, "[")
			if openBracket == -1 {
				return fmt.Errorf("invalid array syntax in path: %s", path)
			}

			key := part[:openBracket]
			indexStr := part[openBracket+1 : len(part)-1]
			index, err := strconv.Atoi(indexStr)
			if err != nil {
				return fmt.Errorf("invalid array index in path: %s", path)
			}

			arr, ok := current[key].([]interface{})
			if !ok {
				// If this is the last part and its not an array, we need to create it
				// not sure if this is a valid case, but in theory we're supposed to.
				arr = make([]interface{}, index+1)
				current[key] = arr
			}

			if index >= len(arr) {
				if i == len(parts)-1 {
					// another edge case that sounds weird, but we need to handle it.
					// the problem here is that we're trying to set a value in an array
					// that has a gap in the indexes, so we need to fill the gap with nils
					newArr := make([]interface{}, index+1)
					copy(newArr, arr)
					arr = newArr
					current[key] = arr
				} else {
					return fmt.Errorf("array index out of bounds in path: %s", path)
				}
			}

			if i == len(parts)-1 {
				// set the value
				arr[index] = value
				return nil
			}

			// move to the next level
			nextMap, ok := arr[index].(map[string]interface{})
			if !ok {
				nextMap = make(map[string]interface{})
				arr[index] = nextMap
			}
			current = nextMap
		} else {
			if i == len(parts)-1 {
				// This is the last part, set the value
				current[part] = value
				return nil
			}

			// Handle object access
			next, ok := current[part].(map[string]interface{})
			if !ok {
				next = make(map[string]interface{})
				current[part] = next
			}
			current = next
		}
	}
	return nil
}
