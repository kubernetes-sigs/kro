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
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	fake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiextensionsinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	openapiresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

var testGVK = schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}

func newTestCRD(name, group, kind string, versions ...string) *apiextensionsv1.CustomResourceDefinition {
	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
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

// startResolver creates a CRDSchemaResolver with a running informer.
func startResolver(t *testing.T, crds ...*apiextensionsv1.CustomResourceDefinition) (*CRDSchemaResolver, *fake.Clientset) {
	t.Helper()
	objs := make([]k8sruntime.Object, len(crds))
	for i := range crds {
		objs[i] = crds[i]
	}
	fakeClient := fake.NewSimpleClientset(objs...)
	factory := apiextensionsinformers.NewSharedInformerFactory(fakeClient, 0)
	r := NewCRDSchemaResolver(factory.Apiextensions().V1().CustomResourceDefinitions())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	factory.Start(ctx.Done())
	factory.WaitForCacheSync(ctx.Done())

	return r, fakeClient
}

// eventually polls condition until it returns true or timeout.
func eventually(t *testing.T, condition func() bool, timeout, interval time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatal("condition not met within timeout")
}

// --- extractVersionSchema ---

func TestExtractVersionSchema(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1", "v2")

	s, err := extractVersionSchema(crd, "v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil schema for v1")
	}

	s, err = extractVersionSchema(crd, "v2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil schema for v2")
	}
}

func TestExtractVersionSchema_VersionNotFound(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1")
	s, err := extractVersionSchema(crd, "v999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != nil {
		t.Fatal("expected nil schema for nonexistent version")
	}
}

func TestExtractVersionSchema_NilSchema(t *testing.T) {
	crd := &apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1", Schema: nil},
			},
		},
	}
	s, err := extractVersionSchema(crd, "v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s != nil {
		t.Fatal("expected nil schema when Schema is nil")
	}
}

// --- convertCRDSchema ---

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

// --- gvkIndexKeys ---

func TestGvkIndexKeys(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1", "v2beta1")
	keys := gvkIndexKeys(crd)
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
	if keys[0] != "example.com/v1/Foo" {
		t.Errorf("unexpected key: %s", keys[0])
	}
	if keys[1] != "example.com/v2beta1/Foo" {
		t.Errorf("unexpected key: %s", keys[1])
	}
}

func TestGvkIndexKeys_FiltersUnserved(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1")
	// Add an unserved version.
	crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
		Name:   "v2",
		Served: false,
	})
	keys := gvkIndexKeys(crd)
	if len(keys) != 1 {
		t.Fatalf("expected 1 key (unserved filtered), got %d", len(keys))
	}
}

// --- ResolveSchema ---

func TestResolveSchema(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1")
	r, _ := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	s, err := r.ResolveSchema(gvk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil schema")
	}
}

func TestResolveSchema_CacheHit(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1")
	r, _ := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	s1, _ := r.ResolveSchema(gvk)
	s2, _ := r.ResolveSchema(gvk)
	if s1 != s2 {
		t.Fatal("expected same pointer on cache hit")
	}
}

func TestResolveSchema_NotFound(t *testing.T) {
	r, _ := startResolver(t)

	gvk := schema.GroupVersionKind{Group: "unknown.com", Version: "v1", Kind: "Unknown"}
	_, err := r.ResolveSchema(gvk)
	if !errors.Is(err, openapiresolver.ErrSchemaNotFound) {
		t.Fatalf("expected ErrSchemaNotFound, got %v", err)
	}
}

func TestResolveSchema_MultipleVersions(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1", "v2", "v1beta1")
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

// --- Event-driven eviction ---

func TestResolveSchema_EvictOnUpdate(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1")
	r, fakeClient := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	s1, _ := r.ResolveSchema(gvk)
	if s1 == nil {
		t.Fatal("expected non-nil schema")
	}

	// Update the CRD.
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["status"] = apiextensionsv1.JSONSchemaProps{Type: "object"}
	_, err := fakeClient.ApiextensionsV1().CustomResourceDefinitions().Update(
		context.Background(), crd, metav1.UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("failed to update CRD: %v", err)
	}

	// Wait for eviction + re-index.
	eventually(t, func() bool {
		s2, err := r.ResolveSchema(gvk)
		return err == nil && s2 != s1
	}, 5*time.Second, 50*time.Millisecond)
}

func TestResolveSchema_EvictOnDelete(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1")
	r, fakeClient := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	_, err := r.ResolveSchema(gvk)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Delete the CRD.
	err = fakeClient.ApiextensionsV1().CustomResourceDefinitions().Delete(
		context.Background(), "foos.example.com", metav1.DeleteOptions{},
	)
	if err != nil {
		t.Fatalf("failed to delete CRD: %v", err)
	}

	eventually(t, func() bool {
		_, err := r.ResolveSchema(gvk)
		return errors.Is(err, openapiresolver.ErrSchemaNotFound)
	}, 5*time.Second, 50*time.Millisecond)
}

func TestResolveSchema_DynamicAdd(t *testing.T) {
	r, fakeClient := startResolver(t)

	barCRD := newTestCRD("bars.example.com", "example.com", "Bar", "v1")
	_, err := fakeClient.ApiextensionsV1().CustomResourceDefinitions().Create(
		context.Background(), barCRD, metav1.CreateOptions{},
	)
	if err != nil {
		t.Fatalf("failed to create CRD: %v", err)
	}

	barGVK := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Bar"}
	eventually(t, func() bool {
		s, err := r.ResolveSchema(barGVK)
		return err == nil && s != nil
	}, 5*time.Second, 50*time.Millisecond)
}

// --- onDelete tombstone handling ---

func TestOnDelete_Tombstone(t *testing.T) {
	crd := newTestCRD("foos.example.com", "example.com", "Foo", "v1")
	r, _ := startResolver(t, crd)

	gvk := schema.GroupVersionKind{Group: "example.com", Version: "v1", Kind: "Foo"}
	_, _ = r.ResolveSchema(gvk) // populate cache

	// Simulate tombstone delete.
	tombstone := cache.DeletedFinalStateUnknown{Key: "foos.example.com", Obj: crd}
	r.onDelete(tombstone)

	// Cache should be evicted.
	r.mu.RLock()
	_, ok := r.schemas[gvk]
	r.mu.RUnlock()
	if ok {
		t.Fatal("expected cache eviction after tombstone delete")
	}
}

func TestOnDelete_InvalidObject(t *testing.T) {
	r, _ := startResolver(t)
	// Should not panic.
	r.onDelete(nil)
	r.onDelete("not-a-crd")
	r.onDelete(cache.DeletedFinalStateUnknown{Obj: "not-a-crd"})
	r.onDelete(cache.DeletedFinalStateUnknown{Obj: (*apiextensionsv1.CustomResourceDefinition)(nil)})
}

// --- TTL cached resolver error path ---

type failingResolver struct{}

func (f *failingResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return nil, errors.New("connection refused")
}

func TestTTLCachedSchemaResolver_DelegateError(t *testing.T) {
	cached := NewTTLCachedSchemaResolver(&failingResolver{}, 100, time.Hour)
	_, err := cached.ResolveSchema(testGVK)
	if err == nil || err.Error() != "connection refused" {
		t.Fatalf("expected delegate error, got %v", err)
	}
	_, err = cached.ResolveSchema(testGVK)
	if err == nil {
		t.Fatal("expected error on second call")
	}
}

// --- ChainedResolver ---

func dummySchema(typ string) *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{Type: []string{typ}},
	}
}

type staticResolver struct{ schema *spec.Schema }

func (r *staticResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return r.schema, nil
}

type notFoundResolver struct{}

func (r *notFoundResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return nil, openapiresolver.ErrSchemaNotFound
}

type errorResolver struct{ err error }

func (r *errorResolver) ResolveSchema(_ schema.GroupVersionKind) (*spec.Schema, error) {
	return nil, r.err
}

func TestChainedResolver_FirstResolverWins(t *testing.T) {
	s := dummySchema("first")
	r := NewChainedResolver(&staticResolver{schema: s}, &staticResolver{schema: dummySchema("second")})
	got, err := r.ResolveSchema(testGVK)
	if err != nil || got != s {
		t.Fatalf("expected first resolver's schema, got err=%v", err)
	}
}

func TestChainedResolver_FallsThroughOnNotFound(t *testing.T) {
	s := dummySchema("second")
	r := NewChainedResolver(&notFoundResolver{}, &staticResolver{schema: s})
	got, err := r.ResolveSchema(testGVK)
	if err != nil || got != s {
		t.Fatalf("expected fallthrough to second resolver, got err=%v", err)
	}
}

func TestChainedResolver_AllNotFound(t *testing.T) {
	r := NewChainedResolver(&notFoundResolver{}, &notFoundResolver{})
	_, err := r.ResolveSchema(testGVK)
	if !errors.Is(err, openapiresolver.ErrSchemaNotFound) {
		t.Fatalf("expected ErrSchemaNotFound, got %v", err)
	}
}

func TestChainedResolver_StopsOnRealError(t *testing.T) {
	realErr := errors.New("connection refused")
	r := NewChainedResolver(&errorResolver{err: realErr}, &staticResolver{schema: dummySchema("x")})
	_, err := r.ResolveSchema(testGVK)
	if !errors.Is(err, realErr) {
		t.Fatalf("expected real error, got %v", err)
	}
}

func TestChainedResolver_EmptyChain(t *testing.T) {
	r := NewChainedResolver()
	_, err := r.ResolveSchema(testGVK)
	if !errors.Is(err, openapiresolver.ErrSchemaNotFound) {
		t.Fatalf("expected ErrSchemaNotFound, got %v", err)
	}
}
