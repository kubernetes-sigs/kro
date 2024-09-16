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

package emulator

import (
	"fmt"
	"math/rand"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// TODO(a-hilaly): generate fields based on the schema constraints(min, max, pattern, etc...)

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
func (e *Emulator) GenerateDummyCR(gvk schema.GroupVersionKind, schema *spec.Schema) (*unstructured.Unstructured, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema is nil for %v", gvk)
	}

	cr := &unstructured.Unstructured{
		Object: make(map[string]interface{}),
	}

	// Generate the entire object based on the schema
	object, err := e.generateObject(schema)
	if err != nil {
		return nil, fmt.Errorf("error generating CR: %w", err)
	}

	// Merge the generated object with the existing CR object
	for k, v := range object {
		cr.Object[k] = v
	}

	// Set the GVK after generating the object...
	cr.SetAPIVersion(gvk.GroupVersion().String())
	cr.SetKind(gvk.Kind)
	cr.SetName(fmt.Sprintf("%s-sample", strings.ToLower(gvk.Kind)))
	cr.SetNamespace("default")

	return cr, nil
}

// generateObject generates an object (Struct) based on the provided schema.
func (e *Emulator) generateObject(schema *spec.Schema) (map[string]interface{}, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema is nil")
	}

	result := make(map[string]interface{})
	for propertyName, propertySchema := range schema.Properties {
		// Skip metadata as it's already set
		if propertyName == "metadata" {
			continue
		}
		value, err := e.generateValue(&propertySchema)
		if err != nil {
			return nil, fmt.Errorf("error generating value for %s: %w", propertyName, err)
		}
		result[propertyName] = value
	}

	return result, nil
}

// generateValue generates a value based on the provided schema.
func (e *Emulator) generateValue(schema *spec.Schema) (interface{}, error) {
	if schema == nil {
		return nil, fmt.Errorf("schema is nil")
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

		return nil, fmt.Errorf("schema type is empty and has no properties")
	}

	// Handle 0 or more than type
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
	default:
		return nil, fmt.Errorf("unsupported type: %s", schema.Type)
	}
}

// generateString generates a string based on the provided schema.
func (e *Emulator) generateString(schema *spec.Schema) string {
	if len(schema.Enum) > 0 {
		return schema.Enum[e.rand.Intn(len(schema.Enum))].(string)
	}
	return fmt.Sprintf("dummy-string-%d", e.rand.Intn(1000))
}

func (e *Emulator) generateInteger(_ *spec.Schema) int64 {
	return e.rand.Int63n(100)
}

func (e *Emulator) generateNumber(_ *spec.Schema) float64 {
	return e.rand.Float64() * 100
}

// generateArray generates an array based on the provided schema.
// TODO(a-hilaly): respect the minItems and maxItems constraints.
func (e *Emulator) generateArray(schema *spec.Schema) ([]interface{}, error) {
	if schema.Items == nil || schema.Items.Schema == nil {
		return nil, fmt.Errorf("array items schema is nil")
	}

	numItems := 1 + e.rand.Intn(3) // Generate 1 to 3 items
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
