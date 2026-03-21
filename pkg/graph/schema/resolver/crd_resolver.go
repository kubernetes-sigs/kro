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
	"fmt"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/cel/environment"
	openapiresolver "k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kroschema "github.com/kubernetes-sigs/kro/pkg/graph/schema"
)

const (
	// CRDGVKIndexField is the field index name used by controller-runtime's
	// cache to map GVK strings to CRD objects.
	CRDGVKIndexField = ".metadata.gvkIndex"
)

// CRDSchemaResolver resolves schemas from CRD OpenAPI definitions using the
// controller-runtime cache with a custom GVK field index for lookups and lazy
// schema extraction.
//
// Schemas are only parsed when ResolveSchema is called (lazy) and cached until
// the CRD is updated or deleted. A controller watches CRD changes and evicts
// cached schemas so the next resolve re-extracts from the updated CRD.
type CRDSchemaResolver struct {
	mu      sync.RWMutex
	schemas map[schema.GroupVersionKind]*spec.Schema
	// crdGVKs is a reverse index: CRD name → GVKs tracked for that CRD.
	// Used to evict schemas when a CRD is deleted (the object is already
	// gone from the cache by the time the reconciler runs).
	crdGVKs map[string][]schema.GroupVersionKind
	cache   cache.Cache
	sf      singleflight.Group
}

// NewCRDSchemaResolver creates a resolver backed by the controller-runtime
// cache. Call SetupWithManager to register the field index and eviction
// controller before starting the manager.
func NewCRDSchemaResolver(c cache.Cache) *CRDSchemaResolver {
	return &CRDSchemaResolver{
		schemas: make(map[schema.GroupVersionKind]*spec.Schema),
		crdGVKs: make(map[string][]schema.GroupVersionKind),
		cache:   c,
	}
}

// SetupWithManager registers a GVK field index on the cache and a controller
// that watches CRD updates/deletes to evict stale cached schemas. Must be
// called before the manager is started.
func (r *CRDSchemaResolver) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetCache().IndexField(
		context.Background(),
		&apiextensionsv1.CustomResourceDefinition{},
		CRDGVKIndexField,
		func(obj client.Object) []string {
			crd, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
			if !ok {
				return nil
			}
			return gvkIndexKeys(crd)
		},
	); err != nil {
		return fmt.Errorf("adding GVK field index: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("crd-schema-resolver").
		Watches(
			&apiextensionsv1.CustomResourceDefinition{},
			&handler.EnqueueRequestForObject{},
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(event.CreateEvent) bool { return false },
				UpdateFunc: func(event.UpdateEvent) bool { return true },
				DeleteFunc: func(event.DeleteEvent) bool { return true },
			}),
		).
		Complete(r)
}

// Reconcile evicts cached schemas when a CRD is updated or deleted.
func (r *CRDSchemaResolver) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := r.cache.Get(ctx, client.ObjectKey{Name: req.Name}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			// CRD deleted — evict using the reverse index.
			r.mu.Lock()
			if gvks, ok := r.crdGVKs[req.Name]; ok {
				for _, gvk := range gvks {
					if _, exists := r.schemas[gvk]; exists {
						delete(r.schemas, gvk)
						crdCacheEvictionsTotal.Inc()
					}
				}
				delete(r.crdGVKs, req.Name)
				crdCacheSize.Set(float64(len(r.schemas)))
			}
			r.mu.Unlock()
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// CRD updated — evict all tracked GVKs (old + new versions).
	gvks := allGVKs(crd)
	r.mu.Lock()
	// Evict previously tracked GVKs (handles removed versions).
	if old, ok := r.crdGVKs[req.Name]; ok {
		for _, gvk := range old {
			if _, exists := r.schemas[gvk]; exists {
				delete(r.schemas, gvk)
				crdCacheEvictionsTotal.Inc()
			}
		}
	}
	// Evict current GVKs and update the reverse index.
	for _, gvk := range gvks {
		if _, exists := r.schemas[gvk]; exists {
			delete(r.schemas, gvk)
			crdCacheEvictionsTotal.Inc()
		}
	}
	r.crdGVKs[req.Name] = gvks
	crdCacheSize.Set(float64(len(r.schemas)))
	r.mu.Unlock()

	return reconcile.Result{}, nil
}

// ResolveSchema returns the OpenAPI schema for the given GVK.
//
// On cache hit the schema is returned immediately. On miss, singleflight
// deduplicates concurrent extractions for the same GVK: only one goroutine
// queries the cache index, extracts the schema, and populates the local cache.
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

		// Look up the CRD from the cache's GVK field index.
		var crdList apiextensionsv1.CustomResourceDefinitionList
		if err := r.cache.List(context.Background(), &crdList,
			client.MatchingFields{CRDGVKIndexField: key},
		); err != nil {
			return nil, fmt.Errorf("listing CRDs by GVK index: %w", err)
		}
		if len(crdList.Items) == 0 {
			return nil, openapiresolver.ErrSchemaNotFound
		}

		crd := &crdList.Items[0]

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
		// Track this CRD in the reverse index for delete eviction.
		r.crdGVKs[crd.Name] = allGVKs(crd)
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
