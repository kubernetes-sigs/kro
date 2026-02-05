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

package resolver

import (
	"fmt"
	"strings"

	"github.com/kubernetes-sigs/kro/pkg/graph/fieldpath"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

// ResolutionResult represents the result of resolving a single expression.
type ResolutionResult struct {
	Path     string
	Resolved bool
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
	// The original resource to be resolved. In kro, this will typically
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
func (r *Resolver) Resolve(expressions []variable.FieldDescriptor) ResolutionSummary {
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

// UpsertValueAtPath sets a value in the resource using the fieldpath parser.
func (r *Resolver) UpsertValueAtPath(path string, value interface{}) error {
	return r.setValueAtPath(path, value)
}

// resolveField handles the resolution of a single ExpressionField (one field) in
// the resource. It returns a ResolutionResult containing information about the
// resolution process
func (r *Resolver) resolveField(field variable.FieldDescriptor) ResolutionResult {
	result := ResolutionResult{
		Path: field.Path,
	}

	value, err := r.getValueFromPath(field.Path)
	if err != nil {
		// Callers are responsible for providing valid paths.
		result.Error = fmt.Errorf("error getting value: %v", err)
		return result
	}

	if field.StandaloneExpression {
		expr := field.Expressions[0].Original
		resolvedValue, ok := r.data[expr]
		if !ok {
			result.Error = fmt.Errorf("no data provided for expression: %s", expr)
			return result
		}
		// setValueAtPath cannot fail here: if getValueFromPath succeeded,
		// the path is valid and traversable.
		_ = r.setValueAtPath(field.Path, resolvedValue)
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
			replacement, ok := r.data[expr.Original]
			if !ok {
				result.Error = fmt.Errorf("no data provided for expression: %s", expr.Original)
				return result
			}
			replaced = strings.ReplaceAll(replaced, "${"+expr.Original+"}", fmt.Sprintf("%v", replacement))
		}

		// setValueAtPath cannot fail here: if getValueFromPath succeeded,
		// the path is valid and traversable.
		_ = r.setValueAtPath(field.Path, replaced)
		result.Resolved = true
		result.Replaced = replaced
	}

	return result
}

// getValueFromPath retrieves a value from the resource using a dot-separated path.
func (r *Resolver) getValueFromPath(path string) (interface{}, error) {
	segments, err := fieldpath.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("invalid path '%s': %v", path, err)
	}

	current := interface{}(r.resource)

	for _, segment := range segments {
		if segment.Index >= 0 {
			// Handle array access
			array, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("expected array at path segment: %v", segment)
			}

			if segment.Index >= len(array) {
				return nil, fmt.Errorf("array index out of bounds: %d", segment.Index)
			}

			current = array[segment.Index]
		} else {
			// Handle object access
			currentMap, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("expected map at path segment: %v", segment)
			}

			value, ok := currentMap[segment.Name]
			if !ok {
				return nil, fmt.Errorf("key not found: %s", segment.Name)
			}
			current = value
		}
	}

	return current, nil
}

// setValueAtPath sets a value in the resource using a dot-separated path.
func (r *Resolver) setValueAtPath(path string, value interface{}) error {
	segments, err := fieldpath.Parse(path)
	if err != nil {
		return fmt.Errorf("invalid path '%s': %v", path, err)
	}

	// We need to keep track of the parent and current object to be able to
	// create new maps and arrays (pointers) as needed. This is crucial for
	// maintaining the proper chain of references.
	var parent interface{} = r.resource
	var current interface{} = r.resource
	var parentKey string
	var parentIndex int

	for i, segment := range segments {
		if segment.Index >= 0 {
			newCurrent, err := handleArraySegment(current, parent, segment, parentKey, parentIndex)
			if err != nil {
				return err
			}
			current = newCurrent

			if i == len(segments)-1 {
				array := current.([]interface{})
				if value == nil {
					// Nil array elements are removed rather than set to null. SSA's structured-merge-diff
					// library sets omitted fields to null, but most CRDs don't define nullable:true.
					// Removing the element avoids SSA conflicts and aligns with K8s API conventions.
					// See: kubernetes-sigs/structured-merge-diff#171, kubernetes/kubernetes#103011
					array = append(array[:segment.Index], array[segment.Index+1:]...)
					updateParent(parent, parentKey, parentIndex, array)
				} else {
					array[segment.Index] = value
				}
				return nil
			}
			parent = current
			parentIndex = segment.Index

			current = getOrCreateNext(current.([]interface{}), segment.Index, segments[i+1].Index >= 0)
		} else {
			currentMap, ok := current.(map[string]interface{})
			if !ok {
				return fmt.Errorf("expected map at path segment: %v", segment)
			}

			if i == len(segments)-1 {
				// Nil values trigger field deletion instead of setting null. This works around an
				// SSA limitation where structured-merge-diff sets omitted fields to null during Apply.
				// Most CRD providers (ACK, ASO, KCC) don't mark optional fields as nullable:true,
				// causing SSA conflicts when null is applied. Deleting the field releases ownership
				// and follows K8s API conventions (field omission > null). kro will not re-manage
				// this field until the expression evaluates to non-nil.
				// See: kubernetes-sigs/structured-merge-diff#171, kubernetes/kubernetes#103011
				if value == nil {
					delete(currentMap, segment.Name)
				} else {
					currentMap[segment.Name] = value
				}
				return nil
			}

			parent = currentMap
			parentKey = segment.Name
			if currentMap[segment.Name] == nil {
				if segments[i+1].Index >= 0 {
					currentMap[segment.Name] = make([]interface{}, 0)
				} else {
					currentMap[segment.Name] = make(map[string]interface{})
				}
			}
			current = currentMap[segment.Name]
		}
	}
	return nil
}

// handleArraySegment manages array access including creation and resizing.
// current is never nil here - callers ensure it via getOrCreateNext or map init.
func handleArraySegment(
	current, parent interface{},
	segment fieldpath.Segment,
	parentKey string,
	parentIndex int,
) (interface{}, error) {
	array, ok := current.([]interface{})
	if !ok {
		return nil, fmt.Errorf("expected array at segment %v, got %T", segment, current)
	}

	if segment.Index >= len(array) {
		newArray := make([]interface{}, segment.Index+1)
		copy(newArray, array)
		updateParent(parent, parentKey, parentIndex, newArray)
		return newArray, nil
	}

	return array, nil
}

// getOrCreateNext ensures the next element in the path exists.
// It initializes a new array or map based on whether the next
// segment is array access.
func getOrCreateNext(array []interface{}, index int, nextIsArray bool) interface{} {
	if array[index] == nil {
		if nextIsArray {
			array[index] = make([]interface{}, 0)
		} else {
			array[index] = make(map[string]interface{})
		}
	}
	return array[index]
}

// updateParent updates the parent's reference to point to a new value.
// This is crucial when we create new arrays or maps to ensure the entire
// object structure remains properly connected.
func updateParent(parent interface{}, key string, index int, value interface{}) {
	switch p := parent.(type) {
	case map[string]interface{}:
		p[key] = value
	case []interface{}:
		p[index] = value
	}
}
