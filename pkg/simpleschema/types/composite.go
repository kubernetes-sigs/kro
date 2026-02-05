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
	"k8s.io/utils/ptr"
)

// Object represents a schemaless object with unknown fields preserved.
type Object struct{}

func (Object) Deps() []string { return nil }

func (Object) Schema(_ Resolver) (*extv1.JSONSchemaProps, error) {
	return &extv1.JSONSchemaProps{
		Type:                   "object",
		XPreserveUnknownFields: ptr.To(true),
	}, nil
}

// Struct represents an object with named fields.
type Struct struct {
	Fields map[string]Type
}

func (s Struct) Deps() []string {
	deps := make([]string, 0, len(s.Fields))
	for _, t := range s.Fields {
		deps = append(deps, t.Deps()...)
	}
	return deps
}

func (s Struct) Schema(r Resolver) (*extv1.JSONSchemaProps, error) {
	props := map[string]extv1.JSONSchemaProps{}
	childHasDefault := false
	for name, t := range s.Fields {
		schema, err := t.Schema(r)
		if err != nil {
			return nil, err
		}
		props[name] = *schema
		if schema.Default != nil {
			childHasDefault = true
		}
	}
	result := &extv1.JSONSchemaProps{
		Type:       "object",
		Properties: props,
	}
	// Set parent default only if children have defaults.
	// Note: Struct doesn't track Required (markers are handled separately),
	// so len(Required)==0 check is implicitly satisfied here.
	if childHasDefault {
		result.Default = &extv1.JSON{Raw: []byte("{}")}
	}
	return result, nil
}

// Custom represents a reference to a user-defined type.
// At parse time, this type is unvalidated - the name may not exist in the
// type registry. Resolution happens later when Schema() is called with a
// Resolver that has the custom types loaded. If the type doesn't exist,
// Schema() will return an error from the Resolver.
type Custom string

func (c Custom) Deps() []string { return []string{string(c)} }

func (c Custom) Schema(r Resolver) (*extv1.JSONSchemaProps, error) {
	return r.Resolve(string(c))
}
