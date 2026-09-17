// Copyright 2026 The Kubernetes Authors.
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

package k8s

import (
	"fmt"

	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/generated/openapi"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/discovery/fake"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// RealCustomResourceDefinitionSchema returns the CustomResourceDefinition
// schema in the shape kro's schema resolver produces for it: built from the
// apiextensions-apiserver OpenAPI definitions with $refs populated, so
// spec.versions[].schema.openAPIV3Schema is the full JSONSchemaProps object and
// its recursive self-references are collapsed into {type: object}
// placeholders.
//
// The hand-written CRD schema in NewFakeResolver is deliberately minimal and
// has no spec.versions; tests that exercise CEL expressions inside a CRD's
// versions block should install this schema with FakeResolver.AddSchema so the
// compiler type-checks against the real shape.
func RealCustomResourceDefinitionSchema() (*spec.Schema, error) {
	sch := runtime.NewScheme()
	if err := extv1.AddToScheme(sch); err != nil {
		return nil, fmt.Errorf("register apiextensions/v1 types: %w", err)
	}
	defs := resolver.NewDefinitionsSchemaResolver(openapi.GetOpenAPIDefinitions, sch)
	s, err := defs.ResolveSchema(extv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
	if err != nil {
		return nil, fmt.Errorf("resolve CustomResourceDefinition schema: %w", err)
	}
	return s, nil
}

// NewFakeResolverWithRealCRDSchema is NewFakeResolver with the
// CustomResourceDefinition entry replaced by RealCustomResourceDefinitionSchema.
func NewFakeResolverWithRealCRDSchema() (*FakeResolver, *fake.FakeDiscovery, error) {
	res, disco := NewFakeResolver()
	crd, err := RealCustomResourceDefinitionSchema()
	if err != nil {
		return nil, nil, err
	}
	res.AddSchema(extv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"), crd)
	return res, disco, nil
}
