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

package simpleschema

import (
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// ToOpenAPISpec converts a SimpleSchema object to an OpenAPI schema.
//
// The first input obj is a map[string]interface{} where the key is the field
// name and the value is the field type.
//
// The second input customTypes is a map[string]interface{} where the key is
// the type name and the value its specification. These custom types will be
// available as predefined types in the transformer.
func ToOpenAPISpec(obj map[string]interface{}, customTypes map[string]interface{}) (*extv1.JSONSchemaProps, error) {
	tf := newTransformer()
	if err := tf.loadPreDefinedTypes(customTypes); err != nil {
		return nil, err
	}
	return tf.buildOpenAPISchema(obj)
}

// FromOpenAPISpec converts an OpenAPI schema to a SimpleSchema object.
func FromOpenAPISpec(schema *extv1.JSONSchemaProps) (map[string]interface{}, error) {
	return nil, fmt.Errorf("not implemented")
}
