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

package types

import (
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// Slice represents an array type: []T.
type Slice struct {
	Elem Type
}

func (s Slice) Deps() []string { return s.Elem.Deps() }

func (s Slice) Schema(r Resolver) (*extv1.JSONSchemaProps, error) {
	elemSchema, err := s.Elem.Schema(r)
	if err != nil {
		return nil, err
	}
	return &extv1.JSONSchemaProps{
		Type:  "array",
		Items: &extv1.JSONSchemaPropsOrArray{Schema: elemSchema},
	}, nil
}

// Map represents a map type: map[string]V.
type Map struct {
	Value Type
}

func (m Map) Deps() []string { return m.Value.Deps() }

func (m Map) Schema(r Resolver) (*extv1.JSONSchemaProps, error) {
	valSchema, err := m.Value.Schema(r)
	if err != nil {
		return nil, err
	}
	return &extv1.JSONSchemaProps{
		Type:                 "object",
		AdditionalProperties: &extv1.JSONSchemaPropsOrBool{Schema: valSchema},
	}, nil
}
