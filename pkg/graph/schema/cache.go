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

package schema

import (
	"k8s.io/kube-openapi/pkg/validation/spec"
)

type fieldKey struct {
	parent *spec.Schema
	field  string
}

// Cache returns pointer-stable *spec.Schema values for child field lookups.
// OpenAPI Properties maps store schemas by value, so indexing copies the
// struct and produces a fresh pointer each time. Cache ensures the same
// (parent, field) pair always returns the same *spec.Schema pointer.
//
// Not safe for concurrent use. Intended to be created per-build and
// discarded when the build completes.
type Cache struct {
	fields map[fieldKey]*spec.Schema
	lists  map[*spec.Schema]*spec.Schema
}

// NewCache returns a new schema cache.
func NewCache() *Cache {
	return &Cache{
		fields: make(map[fieldKey]*spec.Schema),
		lists:  make(map[*spec.Schema]*spec.Schema),
	}
}

// LookupField returns a pointer-stable schema for a named property of parent.
// Returns nil if the field doesn't exist in Properties.
func (c *Cache) LookupField(parent *spec.Schema, field string) *spec.Schema {
	if parent == nil || parent.Properties == nil {
		return nil
	}
	prop, ok := parent.Properties[field]
	if !ok {
		return nil
	}
	k := fieldKey{parent: parent, field: field}
	if v, ok := c.fields[k]; ok {
		return v
	}
	p := &prop
	c.fields[k] = p
	return p
}

// LookupAdditionalProperties returns a pointer-stable schema for
// additionalProperties on parent. Returns nil if not present.
func (c *Cache) LookupAdditionalProperties(parent *spec.Schema) *spec.Schema {
	if parent == nil || parent.AdditionalProperties == nil {
		return nil
	}
	if parent.AdditionalProperties.Schema != nil {
		return parent.AdditionalProperties.Schema
	}
	if !parent.AdditionalProperties.Allows {
		return nil
	}
	k := fieldKey{parent: parent, field: "__additional_properties__"}
	if v, ok := c.fields[k]; ok {
		return v
	}
	empty := &spec.Schema{}
	c.fields[k] = empty
	return empty
}

// WrapAsList returns a pointer-stable list schema wrapping itemSchema.
// Same input pointer always returns the same wrapper pointer.
func (c *Cache) WrapAsList(itemSchema *spec.Schema) *spec.Schema {
	if v, ok := c.lists[itemSchema]; ok {
		return v
	}
	wrapped := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"array"},
			Items: &spec.SchemaOrArray{
				Schema: itemSchema,
			},
		},
	}
	c.lists[itemSchema] = wrapped
	return wrapped
}
