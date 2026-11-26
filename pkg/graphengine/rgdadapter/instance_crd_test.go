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

package rgdadapter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// The resolver serves an instance schema without status.out2; the revision's
// CRD has it.
func TestWithInstanceCRD_TypesStatusPatchAgainstAuthoredCRD(t *testing.T) {
	t.Parallel()

	gvk := schema.GroupVersionKind{Group: "kro.run", Version: "v1alpha1", Kind: "Widget"}
	str := func() extv1.JSONSchemaProps { return extv1.JSONSchemaProps{Type: "string"} }

	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widget"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				Kind:       gvk.Kind,
				APIVersion: gvk.Version,
				Group:      gvk.Group,
				Spec:       runtime.RawExtension{Raw: []byte(`{"message":"string"}`)},
				Status:     runtime.RawExtension{Raw: []byte(`{"out":"${schema.spec.message}","out2":"${schema.spec.message}"}`)},
			},
		},
	}
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.GroupVersion().String(),
		"kind":       gvk.Kind,
		"metadata":   map[string]any{"name": "w1", "namespace": "default"},
		"spec":       map[string]any{"message": "hi"},
	}}

	crd := &extv1.CustomResourceDefinition{
		Spec: extv1.CustomResourceDefinitionSpec{
			Group: gvk.Group,
			Names: extv1.CustomResourceDefinitionNames{Kind: gvk.Kind, Plural: "widgets"},
			Scope: extv1.NamespaceScoped,
			Versions: []extv1.CustomResourceDefinitionVersion{{
				Name: gvk.Version,
				Schema: &extv1.CustomResourceValidation{OpenAPIV3Schema: &extv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]extv1.JSONSchemaProps{
						"apiVersion": str(),
						"kind":       str(),
						"spec":       {Type: "object", Properties: map[string]extv1.JSONSchemaProps{"message": str()}},
						"status":     {Type: "object", Properties: map[string]extv1.JSONSchemaProps{"out": str(), "out2": str()}},
					},
				}},
			}},
		},
	}

	s := func() spec.Schema { return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}} }
	obj := func(props map[string]spec.Schema) spec.Schema {
		return spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}, Properties: props}}
	}
	served := obj(map[string]spec.Schema{
		"apiVersion": s(),
		"kind":       s(),
		"metadata":   obj(map[string]spec.Schema{"name": s(), "namespace": s()}),
		"spec":       obj(map[string]spec.Schema{"message": s()}),
		"status":     obj(map[string]spec.Schema{"out": s()}),
	})
	fakeResolver, disco := testk8s.NewFakeResolver()
	fakeResolver.AddSchema(gvk, &served)
	disco.Resources = append(disco.Resources, &metav1.APIResourceList{
		GroupVersion: gvk.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "widgets", Namespaced: true, Kind: gvk.Kind, Verbs: []string{"get", "patch"}}},
	})
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	c := compiler.NewCompilerWithDependencies(fakeResolver, rm)

	_, _, err := BuildRuntimeForInstanceCached(rgd, instance, c, registry.New())
	require.ErrorContains(t, err, "schema not found for field out2")

	rt, _, err := BuildRuntimeForInstanceCached(rgd, instance, WithInstanceCRD(c, crd), registry.New())
	require.NoError(t, err)
	assert.NotNil(t, rt.Node(StatusPatchNodeID))

	// A revision with a previous version carries it as a second CRD version,
	// behind the hub. The writeback is still typed against the hub.
	multiVersion := crd.DeepCopy()
	previous := *crd.Spec.Versions[0].DeepCopy()
	previous.Name = "v1alpha0"
	delete(previous.Schema.OpenAPIV3Schema.Properties["status"].Properties, "out2")
	multiVersion.Spec.Versions = append(multiVersion.Spec.Versions, previous)

	rt, _, err = BuildRuntimeForInstanceCached(rgd, instance, WithInstanceCRD(c, multiVersion), registry.New())
	require.NoError(t, err)
	assert.NotNil(t, rt.Node(StatusPatchNodeID))
}
