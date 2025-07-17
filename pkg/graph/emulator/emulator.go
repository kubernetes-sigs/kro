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

package emulator

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

var (
	// kubernetesTopLevelFields are top-level fields that are common across all
	// Kubernetes resources. We don't want to generate these fields.
	kubernetesTopLevelFields = []string{"apiVersion", "kind", "metadata"}
)

// Emulator is used to generate dummy CRs based on an OpenAPI schema.
type Emulator struct {
	rand *rand.Rand
}

// NewEmulator creates a new Emulator.
func NewEmulator() *Emulator {
	return &Emulator{
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// GenerateDummyCR generates a dummy CR based on the provided schema.
func (e *Emulator) GenerateDummyCR(gvk schema.GroupVersionKind,
	schema *spec.Schema) (*unstructured.Unstructured, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema is nil for %v", gvk)
	}

	cr := &unstructured.Unstructured{
		Object: make(map[string]interface{}),
	}

	// Only generate fields from the schema
	for propertyName, propertySchema := range schema.Properties {
		// Skip Kubernetes-specific top-level fields
		if slices.Contains(kubernetesTopLevelFields, propertyName) {
			continue
		}
		value, err := e.generateValue(&propertySchema)
		if err != nil {
			return nil, fmt.Errorf("error generating field %s: %w", propertyName, err)
		}
		cr.Object[propertyName] = value
	}

	cr.SetAPIVersion(gvk.GroupVersion().String())
	cr.SetKind(gvk.Kind)
	cr.SetName(fmt.Sprintf("%s-sample", strings.ToLower(gvk.Kind)))
	cr.SetNamespace("default")
	cr.SetResourceVersion(fmt.Sprintf("%d", e.rand.Intn(1000)))
	cr.SetUID("dummy-uid")
	return cr, nil
}

// generateValue generates a value based on the provided schema.
func (e *Emulator) generateValue(schema *spec.Schema) (interface{}, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema is nil")
	}

	if enabled, ok := schema.VendorExtensible.Extensions["x-kubernetes-preserve-unknown-fields"]; ok && enabled.(bool) {
		// Handle x-kubernetes-preserve-unknown-fields
		return e.generateObject(schema)
	}

	if enabled, ok := schema.VendorExtensible.Extensions["x-kubernetes-int-or-string"]; ok && enabled.(bool) {
		// Default to integer for dummy CRs
		return e.generateInteger(schema), nil
	}

	if len(schema.Type) == 0 {
		// If type is not set, check if it's an object
		if len(schema.Properties) > 0 {
			return e.generateObject(schema)
		}
		// Check if it's a oneOf schema
		if len(schema.OneOf) > 0 {
			return e.generateValue(&schema.OneOf[e.rand.Intn(len(schema.OneOf))])
		}

		// Check if its anyOf schema
		if len(schema.AnyOf) > 0 {
			return e.generateValue(&schema.AnyOf[e.rand.Intn(len(schema.AnyOf))])
		}

		// schema is empty and has no properties ==> any value is allowed, even nil, so let's return nil
		// this is the equivalent of emulating the property value to not be present.
		return nil, nil
	}

	if len(schema.Type) != 1 {
		return nil, fmt.Errorf("schema type is not a single type: %v", schema.Type)
	}
	schemaType := schema.Type[0]

	switch schemaType {
	case "string":
		return e.generateString(schema), nil
	case "integer":
		return e.generateInteger(schema), nil
	case "number":
		return e.generateNumber(schema), nil
	case "boolean":
		return e.rand.Intn(2) == 1, nil
	case "array":
		return e.generateArray(schema)
	case "object":
		return e.generateObject(schema)
	case "null":
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported type: %s", schema.Type)
	}
}

// generateObject generates an object based on the provided schema.
func (e *Emulator) generateObject(schema *spec.Schema) (map[string]interface{}, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema is nil")
	}

	result := make(map[string]interface{})
	for propertyName, propertySchema := range schema.Properties {
		value, err := e.generateValue(&propertySchema)
		if err != nil {
			return nil, fmt.Errorf("error generating field %s: %w", propertyName, err)
		}
		result[propertyName] = value
	}

	return result, nil
}

// generateString generates a string based on the provided schema.
func (e *Emulator) generateString(schema *spec.Schema) string {
	if len(schema.Enum) > 0 {
		return schema.Enum[e.rand.Intn(len(schema.Enum))].(string)
	}
	return fmt.Sprintf("dummy-string-%d", e.rand.Intn(1000))
}

func (e *Emulator) generateInteger(schema *spec.Schema) int64 {
	// Default to 0-10000 range
	min := int64(0)
	max := int64(10000)

	if schema.Minimum != nil {
		min = int64(*schema.Minimum)
	}
	if schema.Maximum != nil {
		max = int64(*schema.Maximum)
	}

	if min == max {
		return min
	}

	return min + e.rand.Int63n(max-min)
}

func (e *Emulator) generateNumber(schema *spec.Schema) float64 {
	min := 0.0
	max := 100.0

	if schema.Minimum != nil {
		min = *schema.Minimum
	}
	if schema.Maximum != nil {
		max = *schema.Maximum
	}

	return min + e.rand.Float64()*(max-min)
}

// generateArray generates an array based on the provided schema.
func (e *Emulator) generateArray(schema *spec.Schema) ([]interface{}, error) {
	if schema.Items == nil || schema.Items.Schema == nil {
		return nil, fmt.Errorf("array items schema is nil")
	}

	minItems := 1
	maxItems := 3

	if schema.MinItems != nil {
		minItems = int(*schema.MinItems)
	}
	if schema.MaxItems != nil {
		maxItems = int(*schema.MaxItems)
	}

	numItems := minItems
	if maxItems > minItems {
		numItems += e.rand.Intn(maxItems - minItems)
	}

	result := make([]interface{}, numItems)
	for i := 0; i < numItems; i++ {
		value, err := e.generateValue(schema.Items.Schema)
		if err != nil {
			return nil, err
		}
		result[i] = value
	}

	return result, nil
}
