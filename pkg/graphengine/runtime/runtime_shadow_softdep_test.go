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

package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/kubernetes-sigs/kro/pkg/graphengine/compiler"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/testutil/generator"
)

// TestNew_SeedScopeDoesNotLeakUnderShadowedLocalID is the finding #3 regression
// guard. WithSeedScope(parent.Scope()) copies the parent's whole scope so
// child expressions can read captured parent values. But if the child declares
// its OWN node whose ID collides with a seeded parent key, the seeded ancestor
// value must not shadow-leak into the child: before the child-local node
// publishes, resolving its ID must NOT return the stale parent value.
func TestNew_SeedScopeDoesNotLeakUnderShadowedLocalID(t *testing.T) {
	t.Parallel()

	// A child graph that declares a local node "x".
	g := generator.NewGraph("child",
		generator.WithNamespace("default"),
		generator.WithDef("x", map[string]any{"local": "child-value"}),
	)
	prog := compileGraph(t, g)

	// Parent scope carries a DIFFERENT value under the same key "x" plus an
	// unrelated captured key the child legitimately inherits.
	parentScope := map[string]any{
		"x":        map[string]any{"leaked": "parent-value"},
		"captured": map[string]any{"name": "from-parent"},
	}

	rt := New(prog, g, WithSeedScope(parentScope))

	// The child-local node ID must NOT resolve to the seeded ancestor value
	// before it publishes.
	got, present := rt.Scope()["x"]
	assert.False(t, present,
		"a child-local node ID must not inherit the seeded ancestor value; got %v", got)

	// A parent key that the child does NOT redeclare is still inherited.
	assert.Equal(t, map[string]any{"name": "from-parent"}, rt.Scope()["captured"],
		"a non-shadowed captured parent value must remain readable")

	// Once the child publishes its own value, the local node resolves to it.
	setFirst(rt, "x")
	assert.Equal(t, map[string]any{"local": "child-value"}, rt.Scope()["x"],
		"after publishing, the local node resolves to the child value")
}

// TestNew_SeedScopeKeepsOverriddenNodeValue verifies the exemption: a node
// carrying an objectOverride (e.g. the top-level `schema` node seeded with the
// instance's data) keeps its seeded value, because that seed IS its own
// designated value rather than an inherited sibling value.
func TestNew_SeedScopeKeepsOverriddenNodeValue(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("schema", map[string]any{}),
	)
	prog := compileGraph(t, g)

	seeded := map[string]any{"seededSpec": "value"}
	rt := New(prog, g,
		WithSeedScope(map[string]any{"schema": seeded}),
		WithNodeObjectOverride("schema", &unstructured.Unstructured{Object: map[string]any{"x": "y"}}),
	)

	assert.Equal(t, seeded, rt.Scope()["schema"],
		"an overridden node's seed is its own value and must be preserved")
}

// TestNew_SoftDepCollectionLeftUnbound guards against a false concrete-empty
// reading of an unresolved collection soft-dependency (cheeseandcereal's
// review finding: removing a collection's source while children still exist
// reported workerCount: "0" instead of leaving status unknown, unlike v0.9.3).
//
// A soft-dep target that is a COLLECTION must be left UNBOUND, not seeded with
// an empty list. An empty-list seed reads as a concrete resolved-empty
// collection — size(coll) evaluates to 0 — so status/includeWhen/readyWhen
// expressions compute a false definite result for a collection whose source
// hasn't resolved yet. Leaving it unbound makes size/filter/map/id.field
// surface "no such attribute", which IsCELDataPending classifies as
// data-pending, so the dependent node and its status defer until the
// collection genuinely resolves. A singleton soft target is still seeded with
// an empty object so optional sub-field access yields optional.none().
func TestNew_SoftDepCollectionLeftUnbound(t *testing.T) {
	t.Parallel()

	g := generator.NewGraph("g",
		generator.WithNamespace("default"),
		generator.WithDef("src", map[string]any{"names": []any{"a", "b"}}),
		// A forEach template — a COLLECTION node.
		generator.WithTemplate("coll", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "${'cm-' + n}"},
			"data":     map[string]any{"k": "v"},
		}, generator.ForEachDim("n", "${src.names}")),
		// A singleton template.
		generator.WithTemplate("single", map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "single"},
			"data":     map[string]any{"k": "v"},
		}),
		// A consumer that soft-references both, so both become its soft deps.
		generator.WithDef("consumer", map[string]any{
			"collCount":  "${size(coll)}",
			"singleName": "${single.metadata.name}",
		}),
		// includeWhen gates on the collection size. With an empty-list seed this
		// would fold to a concrete false (the bug); left unbound it must
		// data-pend.
		generator.WithIncludeWhen("${size(coll) > 0}"),
	)
	prog, err := mustCompiler(t).CompileWithOptions(g, compiler.WithSoftDependencies("consumer"))
	require.NoError(t, err)

	consumer := prog.Nodes["consumer"]
	require.NotNil(t, consumer, "consumer node must be compiled")
	softIDs := consumer.SoftDepIDs()
	require.Contains(t, softIDs, "coll", "coll must be reclassified as a soft dep")
	require.Contains(t, softIDs, "single", "single must be reclassified as a soft dep")

	rt := New(prog, g)

	// The unresolved collection soft-dep is left UNBOUND, not seeded empty.
	_, seeded := rt.Scope()["coll"]
	assert.False(t, seeded,
		"an unresolved collection soft target must be left unbound, not seeded with an empty list")
	// A singleton soft target is still seeded with an empty object.
	assert.Equal(t, map[string]any{}, rt.Scope()["single"],
		"a singleton soft target must be seeded as an empty object")

	require.True(t, rt.Node("coll").IsCollection())
	require.False(t, rt.Node("single").IsCollection())

	// A size()/includeWhen expression over the unbound collection must
	// data-pend (defer) rather than fold to a concrete false result.
	_, err = rt.Node("consumer").IsIgnored()
	require.Error(t, err, "an unresolved collection soft-dep must data-pend, not read as empty")
	assert.True(t, IsCELDataPending(err),
		"size() over an unresolved collection soft-dep must classify as data-pending; got %v", err)

	// Once the collection publishes, the reference resolves to a concrete value.
	rt.Set("coll", []any{map[string]any{"metadata": map[string]any{"name": "cm-a"}}})
	ignored, err := rt.Node("consumer").IsIgnored()
	require.NoError(t, err, "a published collection must resolve without data-pending")
	assert.False(t, ignored, "size(coll) > 0 is true once the collection has one item")
}
