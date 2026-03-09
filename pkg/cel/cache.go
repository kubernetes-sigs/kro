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

package cel

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/common/types"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

// CompilationCache caches CEL compilation artifacts (DeclTypes, environments,
// programs) to avoid redundant work across reconciles. Each Builder holds its
// own instance, so caches are scoped per-Builder rather than being global.
type CompilationCache interface {
	// SchemaDeclType returns a cached DeclType for the given schema pointer.
	SchemaDeclType(schema *spec.Schema) *apiservercel.DeclType
	// MaybeAssignTypeName returns a cached named DeclType for the given
	// schema pointer and type name.
	MaybeAssignTypeName(schema *spec.Schema, declType *apiservercel.DeclType, typeName string) *apiservercel.DeclType
	// TypedEnvironmentWithProvider creates a typed CEL environment and returns
	// the DeclTypeProvider. Results are cached by canonical schema set.
	TypedEnvironmentWithProvider(schemas map[string]*spec.Schema) (*cel.Env, *DeclTypeProvider, error)
	// ParseCheckAndCompile returns a cached compiled program and checked AST.
	ParseCheckAndCompile(env *cel.Env, expr string) (cel.Program, *cel.Ast, error)
	// ExtendWithTypedVar returns a cached environment extending the parent
	// with a single typed variable declaration.
	ExtendWithTypedVar(parent *cel.Env, varName string, schema *spec.Schema) (*cel.Env, error)
}

// compilationCache is the concrete sync.Map-backed implementation of CompilationCache.
type compilationCache struct {
	declTypes    sync.Map // key: *spec.Schema, value: *apiservercel.DeclType
	namedTypes   sync.Map // key: namedDeclTypeCacheKey, value: *apiservercel.DeclType
	typedEnvs    sync.Map // key: string, value: *typedEnvCacheEntry
	programs     sync.Map // key: programCacheKey, value: *programCacheEntry
	extendedEnvs sync.Map // key: extendedEnvCacheKey, value: *cel.Env
}

// NewCompilationCache returns a fresh CompilationCache instance.
func NewCompilationCache() CompilationCache {
	return &compilationCache{}
}

// namedDeclTypeCacheKey is the key for caching MaybeAssignTypeName results.
type namedDeclTypeCacheKey struct {
	schema *spec.Schema
	name   string
}

// typedEnvCacheEntry holds a cached (env, provider) pair.
type typedEnvCacheEntry struct {
	env      *cel.Env
	provider *DeclTypeProvider
}

// programCacheKey keys compiled programs by (expression, *cel.Env).
type programCacheKey struct {
	expr string
	env  *cel.Env
}

// programCacheEntry holds a cached (program, AST) pair.
type programCacheEntry struct {
	program cel.Program
	ast     *cel.Ast
}

// extendedEnvCacheKey keys extended environments by (parent, varName, schema).
type extendedEnvCacheKey struct {
	parentEnv *cel.Env
	varName   string
	schema    *spec.Schema
}

// SchemaDeclType returns a cached DeclType for the given schema pointer.
// If not cached, it calls SchemaDeclTypeWithMetadata and stores the result.
func (c *compilationCache) SchemaDeclType(schema *spec.Schema) *apiservercel.DeclType {
	if schema == nil {
		return nil
	}
	if v, ok := c.declTypes.Load(schema); ok {
		return v.(*apiservercel.DeclType)
	}
	declType := SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: schema}, false)
	if declType != nil {
		c.declTypes.Store(schema, declType)
	}
	return declType
}

// MaybeAssignTypeName returns a cached named DeclType for the given
// schema pointer and type name. If not cached, it calls MaybeAssignTypeName
// and stores the result.
func (c *compilationCache) MaybeAssignTypeName(schema *spec.Schema, declType *apiservercel.DeclType, typeName string) *apiservercel.DeclType {
	key := namedDeclTypeCacheKey{schema: schema, name: typeName}
	if v, ok := c.namedTypes.Load(key); ok {
		return v.(*apiservercel.DeclType)
	}
	named := declType.MaybeAssignTypeName(typeName)
	c.namedTypes.Store(key, named)
	return named
}

// TypedEnvironmentWithProvider creates a typed CEL environment and also returns
// the DeclTypeProvider that was created internally. This avoids the need to
// create a separate provider via CreateDeclTypeProvider for the same schemas.
//
// Results are cached by canonical (name, schema-pointer) set. CEL environments
// are immutable after creation, so cached entries are safe to share.
func (c *compilationCache) TypedEnvironmentWithProvider(schemas map[string]*spec.Schema) (*cel.Env, *DeclTypeProvider, error) {
	if len(schemas) > 0 {
		key := makeEnvCacheKey(schemas)
		if v, ok := c.typedEnvs.Load(key); ok {
			entry := v.(*typedEnvCacheEntry)
			return entry.env, entry.provider, nil
		}
		env, provider, err := defaultEnvironmentWithCache(c, WithTypedResources(schemas))
		if err != nil {
			return nil, nil, err
		}
		c.typedEnvs.Store(key, &typedEnvCacheEntry{env: env, provider: provider})
		return env, provider, nil
	}
	return defaultEnvironmentWithCache(c, WithTypedResources(schemas))
}

// ParseCheckAndCompile returns a cached compiled program and checked AST
// for the given expression and environment. On cache miss, it parses,
// type-checks, and compiles the expression, then stores the result.
func (c *compilationCache) ParseCheckAndCompile(env *cel.Env, expr string) (cel.Program, *cel.Ast, error) {
	key := programCacheKey{expr: expr, env: env}
	if v, ok := c.programs.Load(key); ok {
		entry := v.(*programCacheEntry)
		return entry.program, entry.ast, nil
	}

	parsedAST, issues := env.Parse(expr)
	if issues != nil && issues.Err() != nil {
		return nil, nil, issues.Err()
	}

	checkedAST, issues := env.Check(parsedAST)
	if issues != nil && issues.Err() != nil {
		return nil, nil, issues.Err()
	}

	program, err := env.Program(checkedAST)
	if err != nil {
		return nil, nil, fmt.Errorf("compile: %w", err)
	}

	c.programs.Store(key, &programCacheEntry{program: program, ast: checkedAST})
	return program, checkedAST, nil
}

// ExtendWithTypedVar returns a cached environment that extends the parent
// with a single typed variable declaration derived from the given schema.
// Uses env.Extend() which reuses the parent's function bindings (cheaper than
// building a full environment from scratch).
func (c *compilationCache) ExtendWithTypedVar(parent *cel.Env, varName string, schema *spec.Schema) (*cel.Env, error) {
	key := extendedEnvCacheKey{parentEnv: parent, varName: varName, schema: schema}
	if v, ok := c.extendedEnvs.Load(key); ok {
		return v.(*cel.Env), nil
	}

	declType := c.SchemaDeclType(schema)
	if declType == nil {
		return nil, fmt.Errorf("failed to build DeclType for schema")
	}

	typeName := TypeNamePrefix + varName
	declType = c.MaybeAssignTypeName(schema, declType, typeName)

	provider := NewDeclTypeProvider(declType)
	provider.SetRecognizeKeywordAsFieldName(true)

	celType := declType.CelType()

	registry := types.NewEmptyRegistry()
	wrappedProvider, err := provider.WithTypeProvider(registry)
	if err != nil {
		return nil, err
	}

	extended, err := parent.Extend(
		cel.Variable(varName, celType),
		cel.CustomTypeProvider(wrappedProvider),
	)
	if err != nil {
		return nil, err
	}

	c.extendedEnvs.Store(key, extended)
	return extended, nil
}

// makeEnvCacheKey builds a canonical string key from a schema map.
// Schemas are sorted by name for determinism, and each entry encodes
// the schema pointer address to distinguish different schema objects.
func makeEnvCacheKey(schemas map[string]*spec.Schema) string {
	names := make([]string, 0, len(schemas))
	for name := range schemas {
		names = append(names, name)
	}
	sort.Strings(names)

	var b strings.Builder
	for i, name := range names {
		if i > 0 {
			b.WriteByte(';')
		}
		b.WriteString(name)
		b.WriteByte(':')
		fmt.Fprintf(&b, "%p", schemas[name])
	}
	return b.String()
}
