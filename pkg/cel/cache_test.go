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
	"sync"
	"testing"

	"k8s.io/kube-openapi/pkg/validation/spec"
)

func TestSchemaDeclType(t *testing.T) {
	cache := NewCompilationCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"age":  {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
		},
	}

	// First call should compute and cache
	dt1 := cache.SchemaDeclType(schema)
	if dt1 == nil {
		t.Fatal("expected non-nil DeclType")
	}

	// Second call with same pointer should return cached result
	dt2 := cache.SchemaDeclType(schema)
	if dt1 != dt2 {
		t.Error("expected same pointer for same schema")
	}

	// Different schema pointer should produce independent result
	schema2 := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"value": {SchemaProps: spec.SchemaProps{Type: []string{"boolean"}}},
			},
		},
	}
	dt3 := cache.SchemaDeclType(schema2)
	if dt3 == nil {
		t.Fatal("expected non-nil DeclType for schema2")
	}
	if dt3 == dt1 {
		t.Error("expected different DeclType for different schema pointer")
	}

	// Nil schema should return nil
	if cache.SchemaDeclType(nil) != nil {
		t.Error("expected nil for nil schema")
	}
}

func TestMaybeAssignTypeName(t *testing.T) {
	cache := NewCompilationCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	dt := cache.SchemaDeclType(schema)
	if dt == nil {
		t.Fatal("expected non-nil DeclType")
	}

	named1 := cache.MaybeAssignTypeName(schema, dt, "MyType")
	if named1 == nil {
		t.Fatal("expected non-nil named DeclType")
	}

	// Same (schema, name) should return cached result
	named2 := cache.MaybeAssignTypeName(schema, dt, "MyType")
	if named1 != named2 {
		t.Error("expected same pointer for same (schema, name)")
	}

	// Different name should produce different result
	named3 := cache.MaybeAssignTypeName(schema, dt, "OtherType")
	if named3 == named1 {
		t.Error("expected different DeclType for different name")
	}
}

func TestTypedEnvCache(t *testing.T) {
	cache := NewCompilationCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	schemas := map[string]*spec.Schema{
		"pod": schema,
	}

	env1, prov1, err := cache.TypedEnvironmentWithProvider(schemas)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 == nil || prov1 == nil {
		t.Fatal("expected non-nil env and provider")
	}

	// Same schema map (same name→pointer mapping) should hit cache
	schemas2 := map[string]*spec.Schema{
		"pod": schema,
	}
	env2, prov2, err := cache.TypedEnvironmentWithProvider(schemas2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 != env2 {
		t.Error("expected same env pointer from cache")
	}
	if prov1 != prov2 {
		t.Error("expected same provider pointer from cache")
	}

	// Different schema pointer should produce different env
	schema2 := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"value": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
		},
	}
	schemas3 := map[string]*spec.Schema{
		"pod": schema2,
	}
	env3, _, err := cache.TypedEnvironmentWithProvider(schemas3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env3 == env1 {
		t.Error("expected different env for different schema pointer")
	}
}

func TestMakeEnvCacheKey(t *testing.T) {
	s1 := &spec.Schema{}
	s2 := &spec.Schema{}

	// Same map should produce same key
	m1 := map[string]*spec.Schema{"a": s1, "b": s2}
	m2 := map[string]*spec.Schema{"b": s2, "a": s1}
	k1 := makeEnvCacheKey(m1)
	k2 := makeEnvCacheKey(m2)
	if k1 != k2 {
		t.Errorf("expected same key regardless of iteration order, got %q vs %q", k1, k2)
	}

	// Different schemas should produce different key
	m3 := map[string]*spec.Schema{"a": s2, "b": s1}
	k3 := makeEnvCacheKey(m3)
	if k1 == k3 {
		t.Error("expected different key for swapped schema pointers")
	}
}

func TestConcurrentCacheAccess(t *testing.T) {
	cache := NewCompilationCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dt := cache.SchemaDeclType(schema)
			if dt == nil {
				t.Error("expected non-nil DeclType")
			}
			named := cache.MaybeAssignTypeName(schema, dt, "TestType")
			if named == nil {
				t.Error("expected non-nil named DeclType")
			}
		}()
	}
	wg.Wait()
}

func TestParseCheckAndCompile(t *testing.T) {
	cache := NewCompilationCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"age":  {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
		},
	}

	env, err := TypedEnvironment(map[string]*spec.Schema{"obj": schema})
	if err != nil {
		t.Fatalf("unexpected error creating env: %v", err)
	}

	// First call should compile and cache
	prog1, ast1, err := cache.ParseCheckAndCompile(env, "obj.name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prog1 == nil || ast1 == nil {
		t.Fatal("expected non-nil program and AST")
	}

	// Second call with same (env, expr) should return cached result
	prog2, ast2, err := cache.ParseCheckAndCompile(env, "obj.name")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ast1 != ast2 {
		t.Error("expected same AST pointer from cache")
	}
	// Programs are interface values; compare via the AST pointer as proxy
	_ = prog2

	// Different expression should produce different result
	prog3, ast3, err := cache.ParseCheckAndCompile(env, "obj.age")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ast3 == ast1 {
		t.Error("expected different AST for different expression")
	}
	_ = prog3

	// Invalid expression should return error
	_, _, err = cache.ParseCheckAndCompile(env, "obj.nonexistent")
	if err == nil {
		t.Error("expected error for invalid expression")
	}
}

func TestParseCheckAndCompile_Concurrent(t *testing.T) {
	cache := NewCompilationCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"value": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	env, err := TypedEnvironment(map[string]*spec.Schema{"res": schema})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prog, ast, err := cache.ParseCheckAndCompile(env, "res.value")
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if prog == nil || ast == nil {
				t.Error("expected non-nil program and AST")
			}
		}()
	}
	wg.Wait()
}

func TestExtendWithTypedVar(t *testing.T) {
	cache := NewCompilationCache()

	// Create a base typed environment
	baseSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	parentEnv, err := TypedEnvironment(map[string]*spec.Schema{"pod": baseSchema})
	if err != nil {
		t.Fatalf("unexpected error creating parent env: %v", err)
	}

	childSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"status": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	// First call should create and cache
	env1, err := cache.ExtendWithTypedVar(parentEnv, "each", childSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 == nil {
		t.Fatal("expected non-nil env")
	}

	// Second call with same args should return cached env
	env2, err := cache.ExtendWithTypedVar(parentEnv, "each", childSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 != env2 {
		t.Error("expected same env pointer from cache")
	}

	// Should be able to compile expressions using the extended variable
	prog, _, err := cache.ParseCheckAndCompile(env1, "each.status")
	if err != nil {
		t.Fatalf("unexpected error compiling with extended env: %v", err)
	}
	if prog == nil {
		t.Error("expected non-nil program")
	}

	// Different variable name should produce different env
	env3, err := cache.ExtendWithTypedVar(parentEnv, "item", childSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env3 == env1 {
		t.Error("expected different env for different variable name")
	}
}

func TestFieldTypeMapCache(t *testing.T) {
	cache := NewCompilationCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"count": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
		},
	}

	declType := cache.SchemaDeclType(schema)
	if declType == nil {
		t.Fatal("expected non-nil DeclType")
	}
	named := cache.MaybeAssignTypeName(schema, declType, "TestObj")

	// First call
	m1 := FieldTypeMap(named.TypeName(), named)
	if len(m1) == 0 {
		t.Fatal("expected non-empty type map")
	}

	// Second call should return cached result (same map pointer)
	m2 := FieldTypeMap(named.TypeName(), named)
	if len(m1) != len(m2) {
		t.Error("expected same length maps")
	}
}
