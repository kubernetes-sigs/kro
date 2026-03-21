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
	"testing"

	"github.com/google/cel-go/cel"
	apiservercel "k8s.io/apiserver/pkg/cel"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

func schemaDeclTypeCreate(s *spec.Schema) *apiservercel.DeclType {
	return openapi.SchemaDeclType(s, false)
}

func TestBuilderCache_SchemaDeclType(t *testing.T) {
	cache := NewBuilderCache()

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
	dt1 := cache.SchemaDeclType(schema, schemaDeclTypeCreate)
	if dt1 == nil {
		t.Fatal("expected non-nil DeclType")
	}

	// Second call with same pointer should return cached result
	dt2 := cache.SchemaDeclType(schema, schemaDeclTypeCreate)
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
	dt3 := cache.SchemaDeclType(schema2, schemaDeclTypeCreate)
	if dt3 == nil {
		t.Fatal("expected non-nil DeclType for schema2")
	}
	if dt3 == dt1 {
		t.Error("expected different DeclType for different schema pointer")
	}

	// Nil schema should return nil
	if cache.SchemaDeclType(nil, schemaDeclTypeCreate) != nil {
		t.Error("expected nil for nil schema")
	}
}

func TestBuilderCache_MaybeAssignTypeName(t *testing.T) {
	cache := NewBuilderCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	dt := cache.SchemaDeclType(schema, schemaDeclTypeCreate)
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

func TestBuilderCache_TypedEnvironmentWithProvider(t *testing.T) {
	cache := NewBuilderCache()

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

	createCalls := 0
	create := func() (*cel.Env, any, error) {
		createCalls++
		env, err := cel.NewEnv()
		return env, "provider1", err
	}

	env1, prov1, err := cache.TypedEnvironmentWithProvider(schemas, create)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 == nil || prov1 == nil {
		t.Fatal("expected non-nil env and provider")
	}
	if createCalls != 1 {
		t.Errorf("expected 1 create call, got %d", createCalls)
	}

	// Same schema map (same name→pointer mapping) should hit cache
	schemas2 := map[string]*spec.Schema{
		"pod": schema,
	}
	env2, prov2, err := cache.TypedEnvironmentWithProvider(schemas2, create)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 != env2 {
		t.Error("expected same env pointer from cache")
	}
	if prov1 != prov2 {
		t.Error("expected same provider pointer from cache")
	}
	if createCalls != 1 {
		t.Errorf("expected create to not be called again, got %d calls", createCalls)
	}

	// Equivalent schema content with a different pointer should still hit cache.
	schemaClone := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}
	schemasClone := map[string]*spec.Schema{
		"pod": schemaClone,
	}
	envClone, provClone, err := cache.TypedEnvironmentWithProvider(schemasClone, create)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if envClone != env1 {
		t.Error("expected same env for equivalent schema content")
	}
	if provClone != prov1 {
		t.Error("expected same provider for equivalent schema content")
	}
	if createCalls != 1 {
		t.Errorf("expected create to not be called again, got %d calls", createCalls)
	}

	// Different schema content should produce different env.
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
	env3, _, err := cache.TypedEnvironmentWithProvider(schemas3, create)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env3 == env1 {
		t.Error("expected different env for different schema pointer")
	}
}

func TestBuilderCache_FieldTypeMap(t *testing.T) {
	cache := NewBuilderCache()

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"name":  {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"count": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
			},
		},
	}

	declType := cache.SchemaDeclType(schema, schemaDeclTypeCreate)
	if declType == nil {
		t.Fatal("expected non-nil DeclType")
	}
	named := cache.MaybeAssignTypeName(schema, declType, "TestObj")

	createCalls := 0
	create := func() map[string]*apiservercel.DeclType {
		createCalls++
		return map[string]*apiservercel.DeclType{named.TypeName(): named}
	}

	// First call
	m1 := cache.FieldTypeMap(named, create)
	if len(m1) == 0 {
		t.Fatal("expected non-empty type map")
	}
	if createCalls != 1 {
		t.Errorf("expected 1 create call, got %d", createCalls)
	}

	// Second call should return cached result
	m2 := cache.FieldTypeMap(named, create)
	if len(m1) != len(m2) {
		t.Error("expected same length maps")
	}
	if createCalls != 1 {
		t.Errorf("expected create to not be called again, got %d calls", createCalls)
	}
}

func TestMakeEnvCacheKey(t *testing.T) {
	s1 := &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"object"}}}
	s1Clone := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type:        []string{"object"},
			Description: "ignored for CEL typing",
		},
	}
	s2 := &spec.Schema{SchemaProps: spec.SchemaProps{Type: []string{"string"}}}

	// Same logical map should produce the same key regardless of iteration order.
	m1 := map[string]*spec.Schema{"a": s1, "b": s2}
	m2 := map[string]*spec.Schema{"b": s2, "a": s1}
	k1 := MakeEnvCacheKey(m1)
	k2 := MakeEnvCacheKey(m2)
	if k1 != k2 {
		t.Errorf("expected same key regardless of iteration order, got %q vs %q", k1, k2)
	}

	// Equivalent schema content should produce the same key even with a different pointer.
	mEquivalent := map[string]*spec.Schema{"a": s1Clone, "b": s2}
	if k := MakeEnvCacheKey(mEquivalent); k != k1 {
		t.Errorf("expected equivalent schema content to produce same key, got %q vs %q", k, k1)
	}

	// Different CEL-relevant schemas should produce a different key.
	m3 := map[string]*spec.Schema{
		"a": {SchemaProps: spec.SchemaProps{Type: []string{"integer"}}},
		"b": s2,
	}
	k3 := MakeEnvCacheKey(m3)
	if k1 == k3 {
		t.Error("expected different key for different schema content")
	}
}

func TestBuilderCache_ConcurrentAccess(t *testing.T) {
	cache := NewBuilderCache()

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
			dt := cache.SchemaDeclType(schema, schemaDeclTypeCreate)
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

func TestSessionCache_ParseCheckAndCompile(t *testing.T) {
	cache := NewSessionCache()

	env, err := cel.NewEnv(cel.Variable("x", cel.IntType))
	if err != nil {
		t.Fatalf("unexpected error creating env: %v", err)
	}

	// First call should compile and cache
	prog1, ast1, err := cache.ParseCheckAndCompile(env, "x + 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prog1 == nil || ast1 == nil {
		t.Fatal("expected non-nil program and AST")
	}

	// Second call with same (env, expr) should return cached result
	prog2, ast2, err := cache.ParseCheckAndCompile(env, "x + 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ast1 != ast2 {
		t.Error("expected same AST pointer from cache")
	}
	_ = prog2

	// Different expression should produce different result
	_, ast3, err := cache.ParseCheckAndCompile(env, "x + 2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ast3 == ast1 {
		t.Error("expected different AST for different expression")
	}

	// Invalid expression should return error
	_, _, err = cache.ParseCheckAndCompile(env, "invalid(((")
	if err == nil {
		t.Error("expected error for invalid expression")
	}
}

func TestSessionCache_ParseAndCheck(t *testing.T) {
	cache := NewSessionCache()

	env, err := cel.NewEnv(cel.Variable("x", cel.IntType))
	if err != nil {
		t.Fatalf("unexpected error creating env: %v", err)
	}

	// First call should parse+check and cache
	ast1, err := cache.ParseAndCheck(env, "x + 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ast1 == nil {
		t.Fatal("expected non-nil AST")
	}

	// Second call should return cached result
	ast2, err := cache.ParseAndCheck(env, "x + 1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ast1 != ast2 {
		t.Error("expected same AST pointer from cache")
	}

	// ParseAndCheck should return AST from program cache too
	_, _, err = cache.ParseCheckAndCompile(env, "x + 3")
	if err != nil {
		t.Fatalf("unexpected error compiling x + 3: %v", err)
	}
	ast3, err := cache.ParseAndCheck(env, "x + 3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ast3 == nil {
		t.Fatal("expected non-nil AST from program cache")
	}
}

func TestSessionCache_ExtendWithTypedVar(t *testing.T) {
	cache := NewSessionCache()

	env, err := cel.NewEnv(cel.Variable("x", cel.IntType))
	if err != nil {
		t.Fatalf("unexpected error creating env: %v", err)
	}

	schema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"status": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
			},
		},
	}

	createCalls := 0
	create := func() (*cel.Env, error) {
		createCalls++
		return env.Extend(cel.Variable("each", cel.DynType))
	}

	// First call should create and cache
	env1, err := cache.ExtendWithTypedVar(env, "each", schema, create)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 == nil {
		t.Fatal("expected non-nil env")
	}
	if createCalls != 1 {
		t.Errorf("expected 1 create call, got %d", createCalls)
	}

	// Second call with same args should return cached env
	env2, err := cache.ExtendWithTypedVar(env, "each", schema, create)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if env1 != env2 {
		t.Error("expected same env pointer from cache")
	}
	if createCalls != 1 {
		t.Errorf("expected create to not be called again, got %d calls", createCalls)
	}
}

func TestSessionCache_ParseCheckAndCompile_ConcurrentAccess(t *testing.T) {
	cache := NewSessionCache()

	env, err := cel.NewEnv(cel.Variable("x", cel.IntType))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prog, ast, err := cache.ParseCheckAndCompile(env, "x + 1")
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
