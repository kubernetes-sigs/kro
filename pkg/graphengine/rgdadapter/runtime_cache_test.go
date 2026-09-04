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
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/restmapper"

	"github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/registry"
	graphruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

// countingCompiler wraps a real compiler and counts CompileWithOptions calls so
// a test can prove the program cache actually skips recompilation.
type countingCompiler struct {
	inner Compiler
	mu    sync.Mutex
	calls int
}

func (c *countingCompiler) CompileWithOptions(g *v1alpha1.Graph, opts ...compiler.CompileOption) (*compiler.Program, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.CompileWithOptions(g, opts...)
}

func (c *countingCompiler) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// schemaValueRGD is an RGD whose sole ConfigMap copies ${schema.spec.value}
// into its data, so the resolved value is a direct, per-instance signal of
// which instance's schema data reached the runtime.
func schemaValueRGD(name string) *v1alpha1.ResourceGraphDefinition {
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "WebApp",
				Spec:       runtime.RawExtension{Raw: []byte(`{"value":"string"}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm", "namespace": "default"},
						"data":       map[string]any{"value": "${schema.spec.value}"},
					}),
				},
			},
		},
	}
}

func webInstance(name, namespace, value string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kro.run/v1alpha1",
		"kind":       "WebApp",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec":       map[string]any{"value": value},
	}}
}

func newTestCompiler(t *testing.T) *compiler.Compiler {
	t.Helper()
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return compiler.NewCompilerWithDependencies(fakeResolver, rm)
}

// resolveCMValue walks the runtime the way the executor would: publish the
// schema def node's value into scope, then resolve the ConfigMap and read the
// value it copied from ${schema.spec.value}.
func resolveCMValue(t *testing.T, rt *graphruntime.Runtime) string {
	t.Helper()
	schemaObjs, err := rt.Node(SchemaNodeID).Resolve()
	require.NoError(t, err)
	require.Len(t, schemaObjs, 1)
	rt.Set(SchemaNodeID, schemaObjs[0].Object)

	cmObjs, err := rt.Node("cm").Resolve()
	require.NoError(t, err)
	require.Len(t, cmObjs, 1)
	return nestedString(t, cmObjs[0].Object, "data", "value")
}

// TestBuildRuntimeForInstanceCached_ReusesProgram proves the cache skips the
// compile for a second instance of the same RGD spec, and — critically — that
// sharing one compiled Program does NOT leak one instance's schema data into
// another. Each runtime must resolve its own ${schema.spec.value}.
func TestBuildRuntimeForInstanceCached_ReusesProgram(t *testing.T) {
	rgd := schemaValueRGD("webapp")
	cc := &countingCompiler{inner: newTestCompiler(t)}
	cache := registry.New()

	rtA, _, err := BuildRuntimeForInstanceCached(rgd, webInstance("a", "default", "value-A"), cc, cache)
	require.NoError(t, err)
	rtB, _, err := BuildRuntimeForInstanceCached(rgd, webInstance("b", "default", "value-B"), cc, cache)
	require.NoError(t, err)

	assert.Equal(t, 1, cc.callCount(), "second instance of the same spec must hit the program cache")
	assert.Same(t, rtA.Program(), rtB.Program(), "both runtimes must share the one cached Program")

	// No cross-instance data leakage: each runtime resolves its OWN value.
	assert.Equal(t, "value-A", resolveCMValue(t, rtA))
	assert.Equal(t, "value-B", resolveCMValue(t, rtB))
}

// TestBuildRuntimeForInstanceCached_NoLeakUnderConcurrency runs many instances
// through the shared cached Program concurrently and asserts each resolves its
// own schema value. Combined with `go test -race`, this pins that the injected
// per-instance data lives only on the per-Runtime wrapper, never on the shared
// Program.
func TestBuildRuntimeForInstanceCached_NoLeakUnderConcurrency(t *testing.T) {
	rgd := schemaValueRGD("webapp")
	cc := &countingCompiler{inner: newTestCompiler(t)}
	cache := registry.New()

	// Prime the cache once so all goroutines take the hit path.
	_, _, err := BuildRuntimeForInstanceCached(rgd, webInstance("seed", "default", "seed"), cc, cache)
	require.NoError(t, err)

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := "value-" + string(rune('A'+i%26)) + string(rune('0'+i%10))
			inst := webInstance("inst", "default", want)
			rt, _, berr := BuildRuntimeForInstanceCached(rgd, inst, cc, cache)
			if berr != nil {
				errs <- berr
				return
			}
			if got := resolveCMValue(t, rt); got != want {
				errs <- fmt.Errorf("instance value leaked: got %q want %q", got, want)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	assert.Equal(t, 1, cc.callCount(), "all concurrent instances must reuse the primed cache entry")
}

// TestBuildRuntimeForInstanceCached_RecompilesOnSpecChange proves a changed RGD
// spec is not served a stale Program: the cache key includes the spec hash, so
// a new spec forces a recompile.
func TestBuildRuntimeForInstanceCached_RecompilesOnSpecChange(t *testing.T) {
	cc := &countingCompiler{inner: newTestCompiler(t)}
	cache := registry.New()

	rgdV1 := schemaValueRGD("webapp")
	_, _, err := BuildRuntimeForInstanceCached(rgdV1, webInstance("a", "default", "v"), cc, cache)
	require.NoError(t, err)

	// Add a second resource: same owner key, different spec → different hash.
	rgdV2 := schemaValueRGD("webapp")
	rgdV2.Spec.Resources = append(rgdV2.Spec.Resources, &v1alpha1.Resource{
		ID: "cm2",
		Template: rawResource(map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]any{"name": "cm2", "namespace": "default"},
			"data":       map[string]any{"value": "${schema.spec.value}"},
		}),
	})
	_, _, err = BuildRuntimeForInstanceCached(rgdV2, webInstance("a", "default", "v"), cc, cache)
	require.NoError(t, err)

	assert.Equal(t, 2, cc.callCount(), "a changed spec hash must force a recompile")
}

// TestBuildRuntimeForInstanceCached_RecompilesOnSchemaChange proves that RGDs
// with identical resources but different schemas are not served stale Programs
// from the cache. The cache key includes a schema fingerprint, so a schema
// change forces a recompile (cache miss).
func TestBuildRuntimeForInstanceCached_RecompilesOnSchemaChange(t *testing.T) {
	cc := &countingCompiler{inner: newTestCompiler(t)}
	cache := registry.New()

	// Two RGDs with IDENTICAL resources, but field "count" typed differently in schema.
	rgdV1 := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "WebApp",
				Spec:       runtime.RawExtension{Raw: []byte(`{"name":"string","count":"integer"}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm", "namespace": "default"},
						"data":       map[string]any{"name": "${schema.spec.name}"},
					}),
				},
			},
		},
	}

	rgdV2 := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "WebApp",
				Spec:       runtime.RawExtension{Raw: []byte(`{"name":"string","count":"string"}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm", "namespace": "default"},
						"data":       map[string]any{"name": "${schema.spec.name}"},
					}),
				},
			},
		},
	}

	rt1, _, err := BuildRuntimeForInstanceCached(rgdV1, webInstance("a", "default", "v1"), cc, cache)
	require.NoError(t, err)
	require.NotNil(t, rt1)

	rt2, _, err := BuildRuntimeForInstanceCached(rgdV2, webInstance("a", "default", "v2"), cc, cache)
	require.NoError(t, err)
	require.NotNil(t, rt2)

	assert.Equal(t, 2, cc.callCount(), "a changed schema must force a recompile (cache miss)")
	assert.NotSame(t, rt1.Program(), rt2.Program(), "both runtimes must resolve to distinct compiled Programs")

	// Behavioral assertion: with the warm cache holding the string-schema Program for "webapp-probe",
	// compiling an integer-typed schema that copies ${schema.spec.value} into ConfigMap data.value
	// must recompile and fail type-checking, rather than falsely hitting the warm cache and succeeding.
	rgdString := schemaValueRGD("webapp-probe")
	rtString, _, err := BuildRuntimeForInstanceCached(rgdString, webInstance("a", "default", "str"), cc, cache)
	require.NoError(t, err)
	require.NotNil(t, rtString)

	rgdInt := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp-probe"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "WebApp",
				Spec:       runtime.RawExtension{Raw: []byte(`{"value":"integer"}`)},
			},
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm", "namespace": "default"},
						"data":       map[string]any{"value": "${schema.spec.value}"},
					}),
				},
			},
		},
	}
	rtInt, _, err := BuildRuntimeForInstanceCached(rgdInt, webInstance("a", "default", "123"), cc, cache)
	require.Error(t, err, "integer schema copying into string field must fail compilation")
	assert.Nil(t, rtInt)
	assert.Contains(t, err.Error(), "type mismatch")
	assert.Contains(t, err.Error(), "returns \"int\" but expected \"string\"")
	assert.Equal(t, 4, cc.callCount(), "integer schema must attempt compilation and not serve stale string Program")
}

// TestBuildRuntimeForInstanceCached_FallsBack verifies the cached entrypoint
// degrades to the inline compile path when it cannot use the cache: a nil cache,
// or an RGD with no declared schema (the empty schema node cannot be typed).
func TestBuildRuntimeForInstanceCached_FallsBack(t *testing.T) {
	c := newTestCompiler(t)

	t.Run("nil cache compiles inline", func(t *testing.T) {
		rt, g, err := BuildRuntimeForInstanceCached(schemaValueRGD("webapp"), webInstance("a", "default", "x"), c, nil)
		require.NoError(t, err)
		require.NotNil(t, rt)
		require.NotNil(t, g)
		assert.Equal(t, "x", resolveCMValue(t, rt))
	})

	t.Run("schema-less RGD compiles inline", func(t *testing.T) {
		// testRGD(nil) has no schema; it references no ${schema.*}, so it
		// compiles via the inline (type-inferred) fallback with a real cache.
		rt, _, err := BuildRuntimeForInstanceCached(testRGD(nil), testInstance("demo", "default"), c, registry.New())
		require.NoError(t, err)
		require.NotNil(t, rt)
	})
}
