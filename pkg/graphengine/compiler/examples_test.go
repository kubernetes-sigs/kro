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

package compiler

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/yaml"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

type exampleTestResolver struct {
	base *testk8s.FakeResolver
}

func permissiveResourceSchema() *spec.Schema {
	return &spec.Schema{
		VendorExtensible: spec.VendorExtensible{
			Extensions: spec.Extensions{
				"x-kubernetes-preserve-unknown-fields": true,
			},
		},
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata": {
					VendorExtensible: spec.VendorExtensible{
						Extensions: spec.Extensions{
							"x-kubernetes-preserve-unknown-fields": true,
						},
					},
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"name":              {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							"namespace":         {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							"labels":            {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
							"annotations":       {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
							"creationTimestamp": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
							"generation":        {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
							"uid":               {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					},
				},
				"spec": {
					VendorExtensible: spec.VendorExtensible{
						Extensions: spec.Extensions{
							"x-kubernetes-preserve-unknown-fields": true,
						},
					},
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
					},
				},
				"status": {
					VendorExtensible: spec.VendorExtensible{
						Extensions: spec.Extensions{
							"x-kubernetes-preserve-unknown-fields": true,
						},
					},
					SchemaProps: spec.SchemaProps{
						Type: []string{"object"},
						Properties: map[string]spec.Schema{
							"conditions": {
								SchemaProps: spec.SchemaProps{
									Type: []string{"array"},
									Items: &spec.SchemaOrArray{
										Schema: &spec.Schema{
											SchemaProps: spec.SchemaProps{
												Type: []string{"object"},
												Properties: map[string]spec.Schema{
													"type":               {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
													"status":             {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
													"lastTransitionTime": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
													"reason":             {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
													"message":            {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (r *exampleTestResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	if gvk.Group == "apiextensions.k8s.io" && gvk.Kind == "CustomResourceDefinition" {
		crdSch := permissiveResourceSchema()
		return crdSch, nil
	}
	if r.base != nil {
		if s, err := r.base.ResolveSchema(gvk); err == nil && s != nil {
			return s, nil
		}
	}
	return permissiveResourceSchema(), nil
}

type exampleRESTMapper struct {
	meta.RESTMapper
}

func (m *exampleRESTMapper) RESTMapping(gk schema.GroupKind, versions ...string) (*meta.RESTMapping, error) {
	v := "v1"
	if len(versions) > 0 && versions[0] != "" {
		v = versions[0]
	}
	resource := strings.ToLower(gk.Kind) + "s"
	scope := meta.RESTScopeNamespace
	if gk.Kind == "ClusterRole" || gk.Kind == "ClusterRoleBinding" || gk.Kind == "CustomResourceDefinition" || gk.Kind == "Namespace" {
		scope = meta.RESTScopeRoot
	}
	return &meta.RESTMapping{
		Resource:         schema.GroupVersionResource{Group: gk.Group, Version: v, Resource: resource},
		GroupVersionKind: schema.GroupVersionKind{Group: gk.Group, Version: v, Kind: gk.Kind},
		Scope:            scope,
	}, nil
}

func newExampleTestCompiler(t *testing.T) *Compiler {
	t.Helper()
	fakeRes, _ := testk8s.NewFakeResolver()
	res := &exampleTestResolver{base: fakeRes}
	rm := &exampleRESTMapper{}
	return NewCompilerWithDependencies(res, rm)
}

// TestExamplesGraphCompile validates that every shipped examples/graph/*.yaml file
// can be parsed, unmarshaled into a Graph object, and compiled successfully.
func TestExamplesGraphCompile(t *testing.T) {
	files, err := filepath.Glob("../../../examples/graph/*.yaml")
	require.NoError(t, err)
	require.NotEmpty(t, files)

	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			data, err := os.ReadFile(file)
			require.NoError(t, err)

			decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
			var foundGraph bool
			for {
				var raw map[string]interface{}
				err := decoder.Decode(&raw)
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				if len(raw) == 0 {
					continue
				}
				kind, _ := raw["kind"].(string)
				if kind != "Graph" {
					continue
				}

				docBytes, err := yaml.Marshal(raw)
				require.NoError(t, err)

				var g expv1alpha1.Graph
				require.NoError(t, yaml.UnmarshalStrict(docBytes, &g))

				c := newExampleTestCompiler(t)
				prog, err := c.Compile(&g)
				if err != nil {
					t.Logf("compile error for %s: %v", file, err)
				}
				require.NoError(t, err, "failed to compile %s", file)
				require.NotNil(t, prog)
				foundGraph = true
			}
			require.True(t, foundGraph, "expected at least one Graph document in %s", file)
		})
	}
}
