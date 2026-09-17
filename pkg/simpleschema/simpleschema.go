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

package simpleschema

import (
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Option configures ToOpenAPISpec.
type Option func(*transformer)

// AllowExpressionFields emits fields whose value is a CEL expression (a string
// with "${" in its type position, or a list of such strings) as untyped schemas
// with x-kubernetes-preserve-unknown-fields instead of rejecting them as unknown
// types. Meant for RGD-style status blocks; custom types stay strictly parsed.
func AllowExpressionFields() Option {
	return func(t *transformer) {
		t.allowExpressions = true
	}
}

// ToOpenAPISpec converts a SimpleSchema object to an OpenAPI schema.
//
// The first input obj is a map[string]interface{} where the key is the field
// name and the value is the field type.
//
// The second input customTypes is a map[string]interface{} where the key is
// the type name and the value its specification. These custom types will be
// available as predefined types in the transformer.
func ToOpenAPISpec(obj map[string]any, customTypes map[string]any, opts ...Option) (*extv1.JSONSchemaProps, error) {
	t, err := newTransformer(customTypes)
	if err != nil {
		return nil, err
	}
	// Applied after the custom types are built so options only affect obj.
	for _, opt := range opts {
		opt(t)
	}
	return t.buildSchema(obj)
}
