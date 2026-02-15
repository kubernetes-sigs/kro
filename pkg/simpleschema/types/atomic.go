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

// Atomic represents primitive types: string, integer, boolean, float.
type Atomic string

const (
	String  Atomic = "string"
	Integer Atomic = "integer"
	Boolean Atomic = "boolean"
	Float   Atomic = "float"
)

func (a Atomic) Deps() []string { return nil }

func (a Atomic) Schema(_ Resolver) (*extv1.JSONSchemaProps, error) {
	return &extv1.JSONSchemaProps{Type: string(a)}, nil
}

// IsAtomic returns true if s is an atomic type name.
func IsAtomic(s string) bool {
	switch s {
	case string(String), string(Integer), string(Boolean), string(Float):
		return true
	}
	return false
}
