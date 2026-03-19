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
	"fmt"
	"sync"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	crdinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/environment"
	openapiresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

const (
	// gvkIndexName is the name of the custom indexer that maps GVK strings
	// to CRD objects.
	gvkIndexName = "byGVK"
)

// CRDSchemaResolver resolves schemas from CRD OpenAPI definitions using a CRD
// informer with a custom GVK indexer for lookups and lazy schema extraction.
//
// Schemas are only parsed when ResolveSchema is called (lazy) and cached until
// the CRD is updated or deleted. Event handlers evict cached schemas so the
// next resolve re-extracts from the updated CRD.
type CRDSchemaResolver struct {
	mu      sync.RWMutex
	schemas map[schema.GroupVersionKind]*spec.Schema
	indexer cache.Indexer
}

// NewCRDSchemaResolver creates a resolver from a CRD informer. It adds a
// custom GVK indexer to the informer for O(1) lookups and registers event
// handlers for cache eviction. The caller is responsible for starting the
// informer (e.g. via the informer factory registered with the controller
// manager).
func NewCRDSchemaResolver(crdInformer crdinformers.CustomResourceDefinitionInformer) *CRDSchemaResolver {
	informer := crdInformer.Informer()

	// Add a custom indexer that maps GVK strings to CRD objects.
	informer.AddIndexers(cache.Indexers{
		gvkIndexName: func(obj any) ([]string, error) {
			crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return nil, nil
			}
			return gvkIndexKeys(crd), nil
		},
	})

	r := &CRDSchemaResolver{
		schemas: make(map[schema.GroupVersionKind]*spec.Schema),
		indexer: informer.GetIndexer(),
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: r.onUpdate,
		DeleteFunc: r.onDelete,
	})

	return r
}

// ResolveSchema returns the OpenAPI schema for the given GVK.
//
// On cache hit the schema is returned immediately. On miss the resolver
// queries the informer's GVK index to find the CRD, extracts the schema for
// the requested version, caches it, and returns it.
func (r *CRDSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	// Fast path: cached schema.
	r.mu.RLock()
	if s, ok := r.schemas[gvk]; ok {
		r.mu.RUnlock()
		return s, nil
	}
	r.mu.RUnlock()

	// Look up the CRD from the informer's GVK index.
	key := gvkIndexKey(gvk)
	items, err := r.indexer.ByIndex(gvkIndexName, key)
	if err != nil || len(items) == 0 {
		return nil, openapiresolver.ErrSchemaNotFound
	}

	crd, ok := items[0].(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		return nil, openapiresolver.ErrSchemaNotFound
	}

	// Extract the schema for the requested version.
	s, err := extractVersionSchema(crd, gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("extracting schema for %v: %w", gvk, err)
	}
	if s == nil {
		return nil, openapiresolver.ErrSchemaNotFound
	}

	r.mu.Lock()
	r.schemas[gvk] = s
	r.mu.Unlock()
	return s, nil
}

// gvkIndexKey returns the index key for a GVK.
func gvkIndexKey(gvk schema.GroupVersionKind) string {
	return gvk.Group + "/" + gvk.Version + "/" + gvk.Kind
}

// gvkIndexKeys returns all index keys for served versions of a CRD.
func gvkIndexKeys(crd *apiextensionsv1.CustomResourceDefinition) []string {
	keys := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		if !v.Served {
			continue
		}
		keys = append(keys, gvkIndexKey(schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: v.Name,
			Kind:    crd.Spec.Names.Kind,
		}))
	}
	return keys
}

// allGVKs returns GVKs for all versions of a CRD (served or not), used for
// eviction to ensure stale entries for removed versions are cleaned up.
func allGVKs(crd *apiextensionsv1.CustomResourceDefinition) []schema.GroupVersionKind {
	gvks := make([]schema.GroupVersionKind, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		gvks = append(gvks, schema.GroupVersionKind{
			Group:   crd.Spec.Group,
			Version: v.Name,
			Kind:    crd.Spec.Names.Kind,
		})
	}
	return gvks
}

func (r *CRDSchemaResolver) evictGVKs(gvks []schema.GroupVersionKind) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, gvk := range gvks {
		delete(r.schemas, gvk)
	}
}

func (r *CRDSchemaResolver) onUpdate(oldObj, newObj any) {
	// Evict GVKs from both old and new CRD to handle served-version changes.
	if oldCRD, ok := oldObj.(*apiextensionsv1.CustomResourceDefinition); ok && oldCRD != nil {
		r.evictGVKs(allGVKs(oldCRD))
	}
	if newCRD, ok := newObj.(*apiextensionsv1.CustomResourceDefinition); ok && newCRD != nil {
		r.evictGVKs(allGVKs(newCRD))
	}
}

func (r *CRDSchemaResolver) onDelete(obj any) {
	crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			return
		}
		crd, ok = tombstone.Obj.(*apiextensionsv1.CustomResourceDefinition)
		if !ok {
			return
		}
	}
	if crd == nil {
		return
	}
	r.evictGVKs(allGVKs(crd))
}

// extractVersionSchema extracts the OpenAPI schema for a specific version from a CRD.
func extractVersionSchema(crd *apiextensionsv1.CustomResourceDefinition, version string) (*spec.Schema, error) {
	for _, v := range crd.Spec.Versions {
		if v.Name != version {
			continue
		}
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			return nil, nil
		}
		return convertCRDSchema(v.Schema.OpenAPIV3Schema)
	}
	return nil, nil
}

// convertCRDSchema converts a CRD JSONSchemaProps to a kube-openapi spec.Schema.
func convertCRDSchema(jsonSchema *apiextensionsv1.JSONSchemaProps) (*spec.Schema, error) {
	internalSchema := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(
		jsonSchema, internalSchema, nil,
	); err != nil {
		return nil, fmt.Errorf("converting CRD schema to internal: %w", err)
	}

	openapiSchema := &spec.Schema{}
	postProcess := validation.StripUnsupportedFormatsPostProcessorForVersion(environment.DefaultCompatibilityVersion())
	if err := validation.ConvertJSONSchemaPropsWithPostProcess(internalSchema, openapiSchema, postProcess); err != nil {
		return nil, fmt.Errorf("converting to openapi schema: %w", err)
	}

	return openapiSchema, nil
}
