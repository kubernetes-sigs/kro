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

package render

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// OfflineGraph represents a simplified graph structure for offline rendering
type OfflineGraph struct {
	Instance  *unstructured.Unstructured
	Resources map[string]*unstructured.Unstructured
	Order     []string
}

// BuildGraphOffline constructs a resource graph from a local RGD and Instance.
// It performs variable substitution (${schema.spec.x}) without checking the API server.
func BuildGraphOffline(rgd *v1alpha1.ResourceGraphDefinition, instance *unstructured.Unstructured) (*OfflineGraph, error) {
	g := &OfflineGraph{
		Instance:  instance,
		Resources: make(map[string]*unstructured.Unstructured),
		Order:     make([]string, 0),
	}

	// Process Resources from RGD
	for _, res := range rgd.Spec.Resources {
		// Skip external refs in offline mode
		if res.ExternalRef != nil {
			continue
		}

		// Parse the template from RawExtension
		var templateObj map[string]interface{}
		if err := json.Unmarshal(res.Template.Raw, &templateObj); err != nil {
			return nil, fmt.Errorf("failed to unmarshal template for resource '%s': %w", res.ID, err)
		}

		// RESOLVE VARIABLES
		// This is the critical offline step. We traverse the template and inject instance values.
		resolvedObject, err := resolveVariablesOffline(templateObj, instance.Object)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve resource '%s': %w", res.ID, err)
		}

		g.Resources[res.ID] = &unstructured.Unstructured{Object: resolvedObject}
		g.Order = append(g.Order, res.ID)
	}

	return g, nil
}

// resolveVariablesOffline recursively walks the template map and replaces ${schema.spec.x}
func resolveVariablesOffline(obj map[string]interface{}, instanceData map[string]interface{}) (map[string]interface{}, error) {
	result := make(map[string]interface{})

	for key, value := range obj {
		// Resolve the Key (keys can theoretically have variables too)
		resolvedKey := substituteString(key, instanceData)

		// Resolve the Value
		var resolvedVal interface{}
		var err error

		switch v := value.(type) {
		case string:
			resolvedVal = substituteString(v, instanceData)
		case map[string]interface{}:
			resolvedVal, err = resolveVariablesOffline(v, instanceData)
			if err != nil {
				return nil, err
			}
		case []interface{}:
			resolvedList := make([]interface{}, len(v))
			for i, item := range v {
				if mapItem, ok := item.(map[string]interface{}); ok {
					resolvedList[i], err = resolveVariablesOffline(mapItem, instanceData)
					if err != nil {
						return nil, err
					}
				} else if strItem, ok := item.(string); ok {
					resolvedList[i] = substituteString(strItem, instanceData)
				} else {
					resolvedList[i] = item
				}
			}
			resolvedVal = resolvedList
		default:
			resolvedVal = v
		}

		result[resolvedKey] = resolvedVal
	}
	return result, nil
}

// substituteString handles the raw string replacement.
// E.g., "${schema.spec.name}-app" -> "myinstance-app"
func substituteString(tmpl string, data map[string]interface{}) string {
	if !strings.Contains(tmpl, "${") {
		return tmpl
	}

	// Handle multiple variables in the same string
	result := tmpl
	for {
		start := strings.Index(result, "${")
		if start == -1 {
			break
		}

		end := strings.Index(result[start:], "}")
		if end == -1 {
			break
		}
		end += start

		variable := result[start+2 : end] // e.g. "schema.spec.name"

		// Extract value from data map
		val, found := getValueFromPath(variable, data)
		if found {
			// Replace this occurrence
			result = result[:start] + fmt.Sprintf("%v", val) + result[end+1:]
		} else {
			// If not found, leave it as is and move past it to avoid infinite loop
			break
		}
	}

	return result
}

// getValueFromPath walks map[string]interface{} using dot notation
// path: schema.spec.foo
func getValueFromPath(path string, data map[string]interface{}) (interface{}, bool) {
	// Strip "schema" prefix if present, as 'data' is the instance object itself
	cleanPath := strings.TrimPrefix(path, "schema.")
	parts := strings.Split(cleanPath, ".")

	var current interface{} = data
	for _, part := range parts {
		m, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		val, exists := m[part]
		if !exists {
			return nil, false
		}
		current = val
	}
	return current, true
}
