// Copyright 2025 The Kube Resource Orchestrator Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package delta

import (
	"encoding/base64"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ObjectTransformer is an interface for transforming objects before comparison
type ObjectTransformer interface {
	// Transform modifies the object to match server-side representation
	Transform(obj *unstructured.Unstructured) error
	// CanTransform returns true if this transformer can handle the given object
	CanTransform(obj *unstructured.Unstructured) bool
}

// transformerRegistry holds all registered transformers
var transformerRegistry []ObjectTransformer

// RegisterTransformer adds a new transformer to the registry
func RegisterTransformer(t ObjectTransformer) {
	transformerRegistry = append(transformerRegistry, t)
}

// TransformObjectToServerSideRepresentation applies all applicable transformers to the object
func TransformObjectToServerSideRepresentation(obj *unstructured.Unstructured) error {
	for _, transformer := range transformerRegistry {
		if transformer.CanTransform(obj) {
			if err := transformer.Transform(obj); err != nil {
				return fmt.Errorf("failed to transform object: %w", err)
			}
		}
	}
	return nil
}

// SecretTransformer handles transformation of Secret objects
type SecretTransformer struct{}

// CanTransform returns true if the object is a Secret
func (t *SecretTransformer) CanTransform(obj *unstructured.Unstructured) bool {
	return obj.GetAPIVersion() == "v1" && obj.GetKind() == "Secret"
}

// Transform modifies the Secret object to match server-side representation:
// - Moves stringData to data with base64 encoding
// - Merges any existing data with encoded stringData (stringData takes precedence)
func (t *SecretTransformer) Transform(obj *unstructured.Unstructured) error {
	// Get stringData if it exists
	stringData, exists, err := unstructured.NestedMap(obj.Object, "stringData")
	if err != nil {
		return fmt.Errorf("failed to get stringData: %w", err)
	}
	if !exists || len(stringData) == 0 {
		return nil
	}

	// Get existing data or create new map
	data, exists, err := unstructured.NestedMap(obj.Object, "data")
	if err != nil {
		return fmt.Errorf("failed to get data: %w", err)
	}
	if !exists {
		data = make(map[string]interface{})
	}

	// Encode stringData values and add to data
	for k, v := range stringData {
		strVal, ok := v.(string)
		if !ok {
			return fmt.Errorf("stringData value for key %q is not a string", k)
		}
		encoded := base64.StdEncoding.EncodeToString([]byte(strVal))
		data[k] = encoded
	}

	// Update data in the object
	if err := unstructured.SetNestedMap(obj.Object, data, "data"); err != nil {
		return fmt.Errorf("failed to set data: %w", err)
	}

	// Remove stringData
	unstructured.RemoveNestedField(obj.Object, "stringData")
	return nil
}

func init() {
	// Register the Secret transformer
	RegisterTransformer(&SecretTransformer{})
}
