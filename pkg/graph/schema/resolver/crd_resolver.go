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
	"time"

	"golang.org/x/sync/singleflight"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	crdinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/environment"
	openapiresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kube-openapi/pkg/validation/spec"

	kroschema "github.com/kubernetes-sigs/kro/pkg/graph/schema"
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
	sf      singleflight.Group
}

// NewCRDSchemaResolver creates a resolver from a CRD informer. It adds a
// custom GVK indexer to the informer for O(1) lookups and registers event
// handlers for cache eviction. The caller is responsible for starting the
// informer (e.g. via the informer factory registered with the controller
// manager).
func NewCRDSchemaResolver(crdInformer crdinformers.CustomResourceDefinitionInformer) (*CRDSchemaResolver, error) {
	informer := crdInformer.Informer()

	// Add a custom indexer that maps GVK strings to CRD objects.
	if err := informer.AddIndexers(cache.Indexers{
		gvkIndexName: func(obj any) ([]string, error) {
			crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return nil, nil
			}
			return gvkIndexKeys(crd), nil
		},
	}); err != nil {
		return nil, fmt.Errorf("adding GVK indexer: %w", err)
	}

	r := &CRDSchemaResolver{
		schemas: make(map[schema.GroupVersionKind]*spec.Schema),
		indexer: informer.GetIndexer(),
	}

	if _, err := informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		UpdateFunc: r.onUpdate,
		DeleteFunc: r.onDelete,
	}); err != nil {
		return nil, fmt.Errorf("adding event handler: %w", err)
	}

	return r, nil
}

// ResolveSchema returns the OpenAPI schema for the given GVK.
//
// On cache hit the schema is returned immediately. On miss, singleflight
// deduplicates concurrent extractions for the same GVK: only one goroutine
// queries the informer index, extracts the schema, and populates the cache.
func (r *CRDSchemaResolver) ResolveSchema(gvk schema.GroupVersionKind) (*spec.Schema, error) {
	// Fast path: cached schema.
	r.mu.RLock()
	if s, ok := r.schemas[gvk]; ok {
		r.mu.RUnlock()
		crdCacheHitsTotal.Inc()
		return s, nil
	}
	r.mu.RUnlock()

	crdCacheMissesTotal.Inc()

	key := gvkIndexKey(gvk)
	result, err, shared := r.sf.Do(key, func() (any, error) {
		// Double-check: another goroutine may have populated the cache.
		r.mu.RLock()
		if s, ok := r.schemas[gvk]; ok {
			r.mu.RUnlock()
			return s, nil
		}
		r.mu.RUnlock()

		// Look up the CRD from the informer's GVK index.
		items, err := r.indexer.ByIndex(gvkIndexName, key)
		if err != nil || len(items) == 0 {
			return nil, openapiresolver.ErrSchemaNotFound
		}

		crd, ok := items[0].(*apiextensionsv1.CustomResourceDefinition)
		if !ok {
			return nil, openapiresolver.ErrSchemaNotFound
		}

		// Extract the schema for the requested version and inject ObjectMeta.
		start := time.Now()
		s, err := extractVersionSchema(crd, gvk.Version)
		if err != nil {
			crdExtractionErrorsTotal.Inc()
			return nil, fmt.Errorf("extracting schema for %v: %w", gvk, err)
		}
		if s == nil {
			return nil, openapiresolver.ErrSchemaNotFound
		}
		injectKubeEnvelope(s, crd.Spec.Scope == apiextensionsv1.NamespaceScoped)
		crdExtractionDuration.Observe(time.Since(start).Seconds())

		r.mu.Lock()
		r.schemas[gvk] = s
		r.mu.Unlock()

		crdCacheSize.Set(float64(len(r.schemas)))
		return s, nil
	})

	if shared {
		crdSingleflightDeduplicatedTotal.Inc()
	}
	if err != nil {
		return nil, err
	}
	return result.(*spec.Schema), nil
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
	for _, gvk := range gvks {
		if _, ok := r.schemas[gvk]; ok {
			delete(r.schemas, gvk)
			crdCacheEvictionsTotal.Inc()
		}
	}
	crdCacheSize.Set(float64(len(r.schemas)))
	r.mu.Unlock()
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

// injectKubeEnvelope adds the standard Kubernetes envelope fields (metadata,
// apiVersion, kind) to a CRD schema. CRD specs only contain spec/status —
// the API server injects the rest when serving aggregated OpenAPI.
func injectKubeEnvelope(s *spec.Schema, namespaced bool) {
	if s.Properties == nil {
		s.Properties = make(map[string]spec.Schema)
	}
	// Replace metadata if absent or if it's a bare "type: object" stub
	// (common in CRDs generated by controller-gen).
	if meta, ok := s.Properties["metadata"]; !ok || len(meta.Properties) == 0 {
		if namespaced {
			s.Properties["metadata"] = kroschema.ObjectMetaSchema
		} else {
			s.Properties["metadata"] = kroschema.NamespacelessObjectMetaSchema
		}
	}
	if _, ok := s.Properties["apiVersion"]; !ok {
		s.Properties["apiVersion"] = kroschema.StringSchema
	}
	if _, ok := s.Properties["kind"]; !ok {
		s.Properties["kind"] = kroschema.StringSchema
	}
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
