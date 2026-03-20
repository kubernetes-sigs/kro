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

package resolver

import (
	"context"
	"errors"
	"sync"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	openapiresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// testCache is a minimal cache.Cache implementation for unit testing the
// CRDSchemaResolver. It supports List with MatchingFields and Get.
type testCache struct {
	cache.Cache
	mu        sync.RWMutex
	objects   []*apiextensionsv1.CustomResourceDefinition
	indexFn   client.IndexerFunc
	indexName string
}

func newTestCache(crds ...*apiextensionsv1.CustomResourceDefinition) *testCache {
	return &testCache{
		objects: crds,
	}
}

func (c *testCache) IndexField(_ context.Context, _ client.Object, field string, extractValue client.IndexerFunc) error {
	c.indexName = field
	c.indexFn = extractValue
	return nil
}

func (c *testCache) GetInformer(_ context.Context, _ client.Object, _ ...cache.InformerGetOption) (cache.Informer, error) {
	// Not used by Reconcile-based eviction, but needed to satisfy the interface
	// if NewCRDSchemaResolver ever calls it during setup in tests.
	return nil, nil
}

func (c *testCache) Get(_ context.Context, key client.ObjectKey, obj client.Object, _ ...client.GetOption) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, crd := range c.objects {
		if crd.Name == key.Name {
			*obj.(*apiextensionsv1.CustomResourceDefinition) = *crd
			return nil
		}
	}
	return apierrors.NewNotFound(schema.GroupResource{
		Group:    "apiextensions.k8s.io",
		Resource: "customresourcedefinitions",
	}, key.Name)
}

func (c *testCache) List(_ context.Context, list client.ObjectList, opts ...client.ListOption) error {
	crdList, ok := list.(*apiextensionsv1.CustomResourceDefinitionList)
	if !ok {
		return errors.New("unsupported list type")
	}

	listOpts := &client.ListOptions{}
	for _, o := range opts {
		o.ApplyToList(listOpts)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	// If there's a MatchingFields selector, filter using the index function.
	if listOpts.FieldSelector != nil {
		reqs := listOpts.FieldSelector.Requirements()
		for _, req := range reqs {
			if req.Field == c.indexName {
				wantValue := req.Value
				for _, crd := range c.objects {
					keys := c.indexFn(crd)
					for _, key := range keys {
						if key == wantValue {
							crdList.Items = append(crdList.Items, *crd)
						}
					}
				}
				return nil
			}
		}
	}

	// No filter — return all.
	for _, crd := range c.objects {
		crdList.Items = append(crdList.Items, *crd)
	}
	return nil
}

func (c *testCache) addCRD(crd *apiextensionsv1.CustomResourceDefinition) {
	c.mu.Lock()
	c.objects = append(c.objects, crd)
	c.mu.Unlock()
}

func (c *testCache) updateCRD(name string, crd *apiextensionsv1.CustomResourceDefinition) {
	c.mu.Lock()
	for i, existing := range c.objects {
		if existing.Name == name {
			c.objects[i] = crd
			break
		}
	}
	c.mu.Unlock()
}

func (c *testCache) removeCRD(name string) {
	c.mu.Lock()
	for i, existing := range c.objects {
		if existing.Name == name {
			c.objects = append(c.objects[:i], c.objects[i+1:]...)
			break
		}
	}
	c.mu.Unlock()
}

func newTestCRD(name, kind string, versions ...string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "example.com",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: kind},
		},
	}
	for _, v := range versions {
		crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
			Name:   v,
			Served: true,
			Schema: &apiextensionsv1.CustomResourceValidation{
				OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{
						"spec": {Type: "object"},
					},
				},
			},
		})
	}
	return crd
}

func startResolver(t *testing.T, crds ...*apiextensionsv1.CustomResourceDefinition) (*CRDSchemaResolver, *testCache) {
	t.Helper()
	tc := newTestCache(crds...)
	r := NewCRDSchemaResolver(tc)
	return r, tc
}

// reconcileCRD is a test helper that triggers a Reconcile for the given CRD name.
func reconcileCRD(t *testing.T, r *CRDSchemaResolver, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: client.ObjectKey{Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile(%s): %v", name, err)
	}
}

func TestExtractVersionSchema(t *testing.T) {
	crd := newTestCRD("foos.example.com", "Foo", "v1", "v2")

	tests := []struct {
		name    string
		version string
		wantNil bool
	}{
		{"existing v1", "v1", false},
		{"existing v2", "v2", false},
		{"nonexistent version", "v999", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := extractVersionSchema(crd, tt.version)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if (s == nil) != tt.wantNil {
				t.Fatalf("schema nil: got %v, want %v", s == nil, tt.wantNil)
			}
		})
	}
}

func TestExtractVersionSchema_NilFields(t *testing.T) {
	tests := []struct {
		name     string
		versions []apiextensionsv1.CustomResourceDefinitionVersion
	}{
		{"nil Schema", []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Schema: nil}}},
		{"nil OpenAPIV3Schema", []apiextensionsv1.CustomResourceDefinitionVersion{{Name: "v1", Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: nil}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crd := &apiextensionsv1.CustomResourceDefinition{
				Spec: apiextensionsv1.CustomResourceDefinitionSpec{Versions: tt.versions},
			}
			s, err := extractVersionSchema(crd, "v1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s != nil {
				t.Fatal("expected nil schema")
			}
		})
	}
}

func TestConvertCRDSchema(t *testing.T) {
	jsonSchema := &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"name": {Type: "string"},
			"age":  {Type: "integer"},
		},
	}
	s, err := convertCRDSchema(jsonSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil || len(s.Properties) != 2 {
		t.Fatalf("expected 2 properties, got %v", s)
	}
}

func TestGvkIndexKeys(t *testing.T) {
	tests := []struct {
		name     string
		crd      *apiextensionsv1.CustomResourceDefinition
		wantKeys []string
	}{
		{
			name:     "multiple served versions",
			crd:      newTestCRD("foos.example.com", "Foo", "v1", "v2beta1"),
			wantKeys: []string{"example.com/v1/Foo", "example.com/v2beta1/Foo"},
		},
		{
			name: "filters unserved versions",
			crd: func() *apiextensionsv1.CustomResourceDefinition {
				crd := newTestCRD("foos.example.com", "Foo", "v1")
				crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
					Name: "v2", Served: false,
				})
				return crd
			}(),
			wantKeys: []string{"example.com/v1/Foo"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := gvkIndexKeys(tt.crd)
			if len(keys) != len(tt.wantKeys) {
				t.Fatalf("keys: got %d, want %d", len(keys), len(tt.wantKeys))
			}
			for i, want := range tt.wantKeys {
				if keys[i] != want {
					t.Errorf("key[%d]: got %s, want %s", i, keys[i], want)
				}
			}
		})
	}
}

func TestResolveSchema(t *testing.T) {
	tests := []struct {
		name    string
		crds    []*apiextensionsv1.CustomResourceDefinition
		gvk     schema.GroupVersionKind
		wantErr bool
	}{
		{
			name: "resolves existing CRD",
			crds: []*apiextensionsv1.CustomResourceDefinition{newTestCRD("foos.example.com", "Foo", "v1")},
			gvk:  schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"},
		},
		{
			name:    "not found for unknown GVK",
			gvk:     schema.GroupVersionKind{Group: "unknown.com", Version: "v1", Kind: "Unknown"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := startResolver(t, tt.crds...)
			s, err := r.ResolveSchema(tt.gvk)
			if tt.wantErr {
				if !errors.Is(err, openapiresolver.ErrSchemaNotFound) {
					t.Fatalf("expected ErrSchemaNotFound, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s == nil {
				t.Fatal("expected non-nil schema")
			}
		})
	}
}

func TestResolveSchema_CacheHit(t *testing.T) {
	crd := newTestCRD("foos.example.com", "Foo", "v1")
	r, _ := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	s1, _ := r.ResolveSchema(gvk)
	s2, _ := r.ResolveSchema(gvk)
	if s1 != s2 {
		t.Fatal("expected same pointer on cache hit")
	}
}

func TestResolveSchema_MultipleVersions(t *testing.T) {
	crd := newTestCRD("foos.example.com", "Foo", "v1", "v2", "v1beta1")
	r, _ := startResolver(t, crd)

	for _, version := range []string{"v1", "v2", "v1beta1"} {
		gvk := schema.GroupVersionKind{Group: "example.com", Version: version, Kind: "Foo"}
		s, err := r.ResolveSchema(gvk)
		if err != nil {
			t.Fatalf("unexpected error for %s: %v", version, err)
		}
		if s == nil {
			t.Fatalf("expected non-nil schema for %s", version)
		}
	}
}

func TestReconcile_EvictOnUpdate(t *testing.T) {
	crd := newTestCRD("foos.example.com", "Foo", "v1")
	r, tc := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	s1, _ := r.ResolveSchema(gvk)
	if s1 == nil {
		t.Fatal("expected non-nil schema")
	}

	// Update the CRD in the fake cache and reconcile.
	updatedCRD := crd.DeepCopy()
	updatedCRD.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = apiextensionsv1.JSONSchemaProps{Type: "object"}
	tc.updateCRD("foos.example.com", updatedCRD)
	reconcileCRD(t, r, "foos.example.com")

	// After eviction, the next resolve should return a different schema pointer.
	s2, err := r.ResolveSchema(gvk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s2 == s1 {
		t.Fatal("expected different schema pointer after eviction")
	}
}

func TestReconcile_EvictOnDelete(t *testing.T) {
	crd := newTestCRD("foos.example.com", "Foo", "v1")
	r, tc := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	_, err := r.ResolveSchema(gvk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Remove CRD from cache and reconcile (simulates delete).
	tc.removeCRD("foos.example.com")
	reconcileCRD(t, r, "foos.example.com")

	_, err = r.ResolveSchema(gvk)
	if !errors.Is(err, openapiresolver.ErrSchemaNotFound) {
		t.Fatalf("expected ErrSchemaNotFound after delete, got %v", err)
	}
}

func TestReconcile_DeleteUnknownCRD(t *testing.T) {
	// Reconciling a CRD name that was never resolved should be a no-op.
	r, _ := startResolver(t)
	reconcileCRD(t, r, "unknown.example.com")
}

func TestResolveSchema_DynamicAdd(t *testing.T) {
	r, tc := startResolver(t)

	barCRD := newTestCRD("bars.example.com", "Bar", "v1")
	tc.addCRD(barCRD)

	barGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Bar"}
	s, err := r.ResolveSchema(barGVK)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil schema")
	}
}

func TestInjectKubeEnvelope(t *testing.T) {
	tests := []struct {
		name          string
		schema        *spec.Schema
		namespaced    bool
		wantNamespace bool
		wantPreserve  bool
	}{
		{
			name: "namespaced",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type:       []string{"object"},
				Properties: map[string]spec.Schema{"spec": {}},
			}},
			namespaced:    true,
			wantNamespace: true,
		},
		{
			name: "cluster-scoped",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Type:       []string{"object"},
				Properties: map[string]spec.Schema{"spec": {}},
			}},
			namespaced:    false,
			wantNamespace: false,
		},
		{
			name:          "nil properties",
			schema:        &spec.Schema{},
			namespaced:    true,
			wantNamespace: true,
		},
		{
			name: "overwrites bare metadata stub",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Properties: map[string]spec.Schema{
					"metadata": {SchemaProps: spec.SchemaProps{Type: []string{"object"}}},
				},
			}},
			namespaced:    true,
			wantNamespace: true,
		},
		{
			name: "preserves metadata with properties",
			schema: &spec.Schema{SchemaProps: spec.SchemaProps{
				Properties: map[string]spec.Schema{
					"metadata": {SchemaProps: spec.SchemaProps{
						Type: []string{"custom"},
						Properties: map[string]spec.Schema{
							"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						},
					}},
				},
			}},
			namespaced:   true,
			wantPreserve: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			injectKubeEnvelope(tt.schema, tt.namespaced)

			if _, ok := tt.schema.Properties["metadata"]; !ok {
				t.Fatal("expected metadata")
			}
			if _, ok := tt.schema.Properties["apiVersion"]; !ok {
				t.Fatal("expected apiVersion")
			}
			if _, ok := tt.schema.Properties["kind"]; !ok {
				t.Fatal("expected kind")
			}

			if tt.wantPreserve {
				if got := tt.schema.Properties["metadata"].Type[0]; got != "custom" {
					t.Fatalf("metadata type: got %s, want custom", got)
				}
				return
			}

			meta := tt.schema.Properties["metadata"]
			if _, ok := meta.Properties["name"]; !ok {
				t.Fatal("expected metadata.name")
			}
			_, hasNS := meta.Properties["namespace"]
			if hasNS != tt.wantNamespace {
				t.Fatalf("metadata.namespace: got %v, want %v", hasNS, tt.wantNamespace)
			}
		})
	}
}

func TestInjectKubeEnvelope_EndToEnd(t *testing.T) {
	tests := []struct {
		name          string
		scope         apiextensionsv1.ResourceScope
		wantNamespace bool
	}{
		{"namespaced CRD", apiextensionsv1.NamespaceScoped, true},
		{"cluster-scoped CRD", apiextensionsv1.ClusterScoped, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			crd := newTestCRD("foos.example.com", "Foo", "v1")
			crd.Spec.Scope = tt.scope
			r, _ := startResolver(t, crd)

			gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
			s, err := r.ResolveSchema(gvk)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := s.Properties["metadata"]; !ok {
				t.Fatal("expected metadata")
			}
			if _, ok := s.Properties["apiVersion"]; !ok {
				t.Fatal("expected apiVersion")
			}
			if _, ok := s.Properties["kind"]; !ok {
				t.Fatal("expected kind")
			}
			meta := s.Properties["metadata"]
			if _, ok := meta.Properties["name"]; !ok {
				t.Fatal("expected metadata.name")
			}
			_, hasNS := meta.Properties["namespace"]
			if hasNS != tt.wantNamespace {
				t.Fatalf("metadata.namespace: got %v, want %v", hasNS, tt.wantNamespace)
			}
		})
	}
}
