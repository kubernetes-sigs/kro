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
	"fmt"

	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// ObjectMeta holds the k8s ObjectMeta schema, populated once at startup.
var ObjectMetaSchema spec.Schema

func init() {
	// Populate ObjectMeta schema once at startup to avoid repeated query operations.
	var err error
	ObjectMetaSchema, err = getObjectMetaSchema()
	if err != nil {
		// This should never happen as getObjectMetaSchemaUncached only fails if
		// Kubernetes OpenAPI definitions are missing, which would be a
		// critical build/dependency issue.
		panic(fmt.Sprintf("failed to initialize ObjectMeta schema: %v", err))
	}
}

// getObjectMetaSchema extracts the ObjectMeta schema from Kubernetes OpenAPI definitions.
// This returns the fully resolved ObjectMeta schema including all nested types like
// OwnerReference, ManagedFieldsEntry, Time, etc.
func getObjectMetaSchema() (spec.Schema, error) {
	// get OpenAPI definitions from apiserver package
	definitions := openapi.GetOpenAPIDefinitions(spec.MustCreateRef)
	populatedSchema, err := resolver.PopulateRefs(func(ref string) (*spec.Schema, bool) {
		def, ok := definitions[ref]
		if !ok {
			return nil, false
		}
		s := def.Schema
		return &s, true
	}, "k8s.io/apimachinery/pkg/apis/meta/v1.ObjectMeta")
	if err != nil {
		return spec.Schema{}, fmt.Errorf("failed to populate refs for ObjectMeta: %w", err)
	}
	return *populatedSchema, nil
}
