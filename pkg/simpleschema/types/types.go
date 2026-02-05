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

// Resolver resolves custom type names to their schemas.
// It is used by Type.Schema to look up user-defined types during schema generation.
type Resolver interface {
	// Resolve returns the OpenAPI schema for a custom type by name.
	// Returns an error if the type is not found.
	Resolve(name string) (*extv1.JSONSchemaProps, error)
}

// Type represents a parsed SimpleSchema type that can provide dependencies
// and build OpenAPI schemas. Implementations include Atomic (primitives),
// Slice, Map (collections), Object, Struct, and Custom (user-defined types).
type Type interface {
	// Deps returns the names of custom types this type depends on.
	// Used for topological sorting when loading custom types.
	// Returns nil for types with no dependencies (e.g., atomics).
	Deps() []string

	// Schema generates the OpenAPI JSONSchemaProps for this type.
	// The resolver is used to look up custom type schemas.
	Schema(Resolver) (*extv1.JSONSchemaProps, error)
}
