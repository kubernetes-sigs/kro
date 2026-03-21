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

package cache

import (
	"sync"

	"github.com/google/cel-go/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// BuilderCache caches cross-RGD CEL artifacts: DeclTypes, named types,
// typed environments, and field type maps. Long-lived, scoped to a Builder
// instance so caches persist across reconciles but are GC-able when the
// Builder is replaced.
type BuilderCache struct {
	declTypes     sync.Map // key: *spec.Schema, value: *apiservercel.DeclType
	namedTypes    sync.Map // key: namedTypeCacheKey, value: *apiservercel.DeclType
	typedEnvs     sync.Map // key: string, value: *TypedEnvEntry
	fieldTypeMaps sync.Map // key: *apiservercel.DeclType, value: map[string]*apiservercel.DeclType
}

// NewBuilderCache returns a fresh BuilderCache instance.
func NewBuilderCache() *BuilderCache {
	return &BuilderCache{}
}

// SchemaDeclType returns a cached DeclType for the given schema pointer.
// On cache miss, the create callback is called to produce the DeclType.
// The create callback receives the schema and should return the DeclType
// (typically via SchemaDeclTypeWithMetadata).
func (c *BuilderCache) SchemaDeclType(schema *spec.Schema, create func(*spec.Schema) *apiservercel.DeclType) *apiservercel.DeclType {
	if schema == nil {
		return nil
	}
	if v, ok := c.declTypes.Load(schema); ok {
		builderCacheHitsTotal.WithLabelValues("decl_type").Inc()
		return v.(*apiservercel.DeclType)
	}
	builderCacheMissesTotal.WithLabelValues("decl_type").Inc()
	declType := create(schema)
	if declType != nil {
		if actual, loaded := c.declTypes.LoadOrStore(schema, declType); loaded {
			return actual.(*apiservercel.DeclType)
		}
		builderCacheSize.WithLabelValues("decl_type").Inc()
	}
	return declType
}

// MaybeAssignTypeName returns a cached named DeclType for the given
// schema pointer and type name. On cache miss, it calls
// declType.MaybeAssignTypeName(typeName) and caches the result.
func (c *BuilderCache) MaybeAssignTypeName(schema *spec.Schema, declType *apiservercel.DeclType, typeName string) *apiservercel.DeclType {
	key := namedTypeCacheKey{schema: schema, name: typeName}
	if v, ok := c.namedTypes.Load(key); ok {
		builderCacheHitsTotal.WithLabelValues("named_type").Inc()
		return v.(*apiservercel.DeclType)
	}
	builderCacheMissesTotal.WithLabelValues("named_type").Inc()
	named := declType.MaybeAssignTypeName(typeName)
	if actual, loaded := c.namedTypes.LoadOrStore(key, named); loaded {
		return actual.(*apiservercel.DeclType)
	}
	builderCacheSize.WithLabelValues("named_type").Inc()
	return named
}

// TypedEnvironmentWithProvider creates a typed CEL environment and returns
// both the environment and an opaque provider (typically *DeclTypeProvider,
// stored as any to avoid importing pkg/cel). Results are cached by canonical
// schema set. On cache miss, the create callback is called.
func (c *BuilderCache) TypedEnvironmentWithProvider(schemas map[string]*spec.Schema, create func() (*cel.Env, any, error)) (*cel.Env, any, error) {
	if len(schemas) > 0 {
		key := MakeEnvCacheKey(schemas)
		if v, ok := c.typedEnvs.Load(key); ok {
			builderCacheHitsTotal.WithLabelValues("typed_env").Inc()
			entry := v.(*TypedEnvEntry)
			return entry.Env, entry.Provider, nil
		}
		builderCacheMissesTotal.WithLabelValues("typed_env").Inc()
		env, provider, err := create()
		if err != nil {
			builderCacheErrorsTotal.WithLabelValues("typed_env").Inc()
			return nil, nil, err
		}
		entry := &TypedEnvEntry{Env: env, Provider: provider}
		if actual, loaded := c.typedEnvs.LoadOrStore(key, entry); loaded {
			cached := actual.(*TypedEnvEntry)
			return cached.Env, cached.Provider, nil
		}
		builderCacheSize.WithLabelValues("typed_env").Inc()
		return env, provider, nil
	}
	return create()
}

// FieldTypeMap returns a cached field type map for the given DeclType.
// On cache miss, the create callback is called to build the map.
func (c *BuilderCache) FieldTypeMap(t *apiservercel.DeclType, create func() map[string]*apiservercel.DeclType) map[string]*apiservercel.DeclType {
	if v, ok := c.fieldTypeMaps.Load(t); ok {
		builderCacheHitsTotal.WithLabelValues("field_type_map").Inc()
		return v.(map[string]*apiservercel.DeclType)
	}
	builderCacheMissesTotal.WithLabelValues("field_type_map").Inc()
	m := create()
	if actual, loaded := c.fieldTypeMaps.LoadOrStore(t, m); loaded {
		return actual.(map[string]*apiservercel.DeclType)
	}
	builderCacheSize.WithLabelValues("field_type_map").Inc()
	return m
}
