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
type Resolver interface {
	Resolve(name string) (*extv1.JSONSchemaProps, error)
}

// Type represents a parsed type that can provide dependencies and build schemas.
type Type interface {
	Deps() []string
	Schema(Resolver) (*extv1.JSONSchemaProps, error)
}
