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
	"maps"

	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// ObjectMetaSchema holds the k8s ObjectMeta schema, populated once at startup.
var ObjectMetaSchema spec.Schema

// NamespacelessObjectMetaSchema is ObjectMeta without metadata.namespace.
// Cluster-scoped instance CRDs use this when building the typed CEL schema for
// the "schema" variable, so expressions cannot type-check against a field that
// does not exist at runtime.
var NamespacelessObjectMetaSchema spec.Schema

// LabelSelectorSchema holds the k8s metav1.LabelSelector schema, populated once
// at startup. External collection references (externalRef.metadata.selector) are
// validated against this schema so that structural type enforcement comes from
// the canonical Kubernetes definition rather than hand-rolled checks.
var LabelSelectorSchema spec.Schema

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
	NamespacelessObjectMetaSchema = buildNamespacelessObjectMetaSchema(ObjectMetaSchema)

	LabelSelectorSchema, err = getModelSchema(metav1.LabelSelector{}.OpenAPIModelName())
	if err != nil {
		// This should never happen unless the Kubernetes OpenAPI definitions are
		// missing, which would be a critical build/dependency issue.
		panic(fmt.Sprintf("failed to initialize LabelSelector schema: %v", err))
	}
}

// getObjectMetaSchema extracts the ObjectMeta schema from Kubernetes OpenAPI definitions.
// This returns the fully resolved ObjectMeta schema including all nested types like
// OwnerReference, ManagedFieldsEntry, Time, etc.
func getObjectMetaSchema() (spec.Schema, error) {
	return getModelSchema(metav1.ObjectMeta{}.OpenAPIModelName())
}

// getModelSchema extracts and fully resolves the schema for the given OpenAPI
// model name (e.g. "io.k8s.apimachinery.pkg.apis.meta.v1.LabelSelector") from
// the Kubernetes OpenAPI definitions bundled with the apiserver package. All
// nested $ref references are populated so the returned schema is self-contained.
func getModelSchema(modelName string) (spec.Schema, error) {
	// get OpenAPI definitions from apiserver package
	definitions := openapi.GetOpenAPIDefinitions(spec.MustCreateRef)
	populatedSchema, err := resolver.PopulateRefs(func(ref string) (*spec.Schema, bool) {
		def, ok := definitions[ref]
		if !ok {
			return nil, false
		}
		return new(def.Schema), true
	}, modelName)
	if err != nil {
		return spec.Schema{}, fmt.Errorf("failed to populate refs for %s: %w", modelName, err)
	}
	return *populatedSchema, nil
}

func buildNamespacelessObjectMetaSchema(metaSchema spec.Schema) spec.Schema {
	cloned := metaSchema
	if metaSchema.Properties != nil {
		cloned.Properties = make(map[string]spec.Schema, len(metaSchema.Properties))
		maps.Copy(cloned.Properties, metaSchema.Properties)
		delete(cloned.Properties, "namespace")
	}
	if metaSchema.Required != nil {
		cloned.Required = make([]string, 0, len(metaSchema.Required))
		for _, field := range metaSchema.Required {
			if field != "namespace" {
				cloned.Required = append(cloned.Required, field)
			}
		}
	}
	return cloned
}
