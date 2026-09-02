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

// Package rgdadapter contains behaviour tests for the RGD-on-graph-engine
// adapter, verifying that translated RGD constructs (schema injection,
// cross-node references, forEach expansion, includeWhen conditions,
// readyWhen gates, and externalRef imports) compile and evaluate as expected.
//
// These are behaviour tests of the RGD-on-graph-engine adapter, NOT
// differential parity tests against the classic engine; real engine parity
// is demonstrated in the integration suites under test/integration.

package rgdadapter

import (
	"encoding/json"
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
	graphruntime "github.com/kubernetes-sigs/kro/pkg/graphengine/runtime"
	testk8s "github.com/kubernetes-sigs/kro/pkg/testutil/k8s"
)

func rawResource(m map[string]any) runtime.RawExtension {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return runtime.RawExtension{Raw: raw}
}

// TestBuildRuntimeForInstance_NoResources proves a NoOp/arbitrary-object RGD
// (spec.resources: []) translates + compiles: the only node is the instance
// `schema` def node, so the compiled Graph is non-empty and the executor has
// nothing to apply. Regression guard for the e2e check-arbitrary-objects case.
func TestBuildRuntimeForInstance_NoResources(t *testing.T) {
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "noop"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Schema: &v1alpha1.Schema{
				APIVersion: "v1alpha1",
				Kind:       "NoOp",
				Spec:       runtime.RawExtension{Raw: []byte(`{"values":"object"}`)},
			},
			Resources: []*v1alpha1.Resource{},
		},
	}
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kro.run/v1alpha1",
		"kind":       "NoOp",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"values": map[string]any{"foo": "bar"}},
	}}

	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	c := compiler.NewCompilerWithDependencies(fakeResolver, rm)

	rt, g, err := BuildRuntimeForInstance(rgd, instance, c)
	require.NoError(t, err, "zero-resource RGD must translate + compile (schema node only)")
	require.NotNil(t, rt)
	require.Len(t, g.Spec.Nodes, 1)
	assert.Equal(t, SchemaNodeID, g.Spec.Nodes[0].ID)
}

// TestReconcileBehavior_SchemaAndCrossNode verifies that an RGD with two
// template resources — one reading the instance via ${schema.spec.*}, the other
// reading the first via cross-node CEL — is translated to a Graph, the instance
// is injected as a `schema` def node, and the compiler+runtime resolve both
// references. This covers RGD composition: instance scope plus inter-node
// wiring.
func TestReconcileBehavior_SchemaAndCrossNode(t *testing.T) {
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "webapp"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm1",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm1", "namespace": "default"},
						"data":       map[string]any{"value": "${schema.spec.value}"},
					}),
				},
				{
					ID: "cm2",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm2", "namespace": "default"},
						"data":       map[string]any{"ref": "${cm1.metadata.name}"},
					}),
				},
			},
		},
	}

	// 1. Translate RGD resources -> Graph nodes.
	g, err := ResourceGraphDefinitionToGraph(rgd)
	require.NoError(t, err)
	require.Len(t, g.Spec.Nodes, 2)

	// 2. Inject the instance as a `schema` def node (the instance-scope seam).
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "WebApp",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"value": "hello-from-instance"},
	}}
	schemaNode, err := InstanceSchemaNode(instance)
	require.NoError(t, err)
	g.Spec.Nodes = append([]v1alpha1.Node{schemaNode}, g.Spec.Nodes...)

	// 3. Compile through the Graph engine (fake resolver + REST mapper).
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	prog, err := compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
	require.NoError(t, err, "translated Graph (with schema def node) must compile")

	// 4. Instantiate the runtime and resolve in dependency order (mirroring the
	// executor's topological walk: publish each node's value before dependents).
	rt := graphruntime.New(prog, g)

	// Publish the schema def node first — this is the instance-scope injection.
	schemaObjs, err := rt.Node(SchemaNodeID).Resolve()
	require.NoError(t, err)
	require.Len(t, schemaObjs, 1)
	rt.Set(SchemaNodeID, schemaObjs[0].Object)

	cm1Objs, err := rt.Node("cm1").Resolve()
	require.NoError(t, err)
	require.Len(t, cm1Objs, 1)
	// ${schema.spec.value} resolved from the injected instance.
	assert.Equal(t, "hello-from-instance", nestedString(t, cm1Objs[0].Object, "data", "value"))

	// Publish cm1 so cm2's cross-node reference resolves.
	rt.Set("cm1", cm1Objs[0].Object)

	cm2Objs, err := rt.Node("cm2").Resolve()
	require.NoError(t, err)
	require.Len(t, cm2Objs, 1)
	// ${cm1.metadata.name} resolved from the published cm1.
	assert.Equal(t, "cm1", nestedString(t, cm2Objs[0].Object, "data", "ref"))
}

// TestReconcileBehavior_ForEach verifies that an RGD resource carrying a forEach
// dimension translates to a Graph template node and the runtime expands it into
// one rendered object per list element.
func TestReconcileBehavior_ForEach(t *testing.T) {
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "fortest"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Resources: []*v1alpha1.Resource{
				{
					ID: "entry",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						// Name and data key use the iteration variable; unique
						// per-element names satisfy the runtime duplicate check.
						"metadata": map[string]any{"name": "${elem}", "namespace": "default"},
						"data":     map[string]any{"label": "${elem}"},
					}),
					ForEach: []v1alpha1.ForEachDimension{
						{"elem": "${schema.spec.items}"},
					},
				},
			},
		},
	}

	g, err := ResourceGraphDefinitionToGraph(rgd)
	require.NoError(t, err)
	require.Len(t, g.Spec.Nodes, 1)

	// The translated node must carry the forEach dimension.
	translated := g.Spec.Nodes[0]
	require.Len(t, translated.ForEach, 1, "forEach dimension must survive translation")
	// Sanity: the dimension key survives.
	_, hasDim := translated.ForEach[0]["elem"]
	require.True(t, hasDim, "forEach dimension key 'elem' must survive translation")

	// Inject the instance with spec.items = ["a", "b"].
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "ForTest",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"items": []any{"a", "b"}},
	}}
	schemaNode, err := InstanceSchemaNode(instance)
	require.NoError(t, err)
	g.Spec.Nodes = append([]v1alpha1.Node{schemaNode}, g.Spec.Nodes...)

	// Compile.
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	prog, err := compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
	require.NoError(t, err, "forEach graph must compile")

	rt := graphruntime.New(prog, g)

	// Publish the schema def first.
	schemaObjs, err := rt.Node(SchemaNodeID).Resolve()
	require.NoError(t, err)
	rt.Set(SchemaNodeID, schemaObjs[0].Object)

	// Resolve the forEach node — must expand to 2 objects.
	entryObjs, err := rt.Node("entry").Resolve()
	require.NoError(t, err)
	require.Len(t, entryObjs, 2, "forEach with 2 elements must produce 2 objects")

	assert.Equal(t, "a", nestedString(t, entryObjs[0].Object, "metadata", "name"))
	assert.Equal(t, "a", nestedString(t, entryObjs[0].Object, "data", "label"))
	assert.Equal(t, "b", nestedString(t, entryObjs[1].Object, "metadata", "name"))
	assert.Equal(t, "b", nestedString(t, entryObjs[1].Object, "data", "label"))
}

// TestReconcileBehavior_IncludeWhen verifies that an RGD resource with
// includeWhen translates to a Graph node whose IsIgnored reflects the
// condition value at runtime.
func TestReconcileBehavior_IncludeWhen(t *testing.T) {
	makeRGD := func() *v1alpha1.ResourceGraphDefinition {
		return &v1alpha1.ResourceGraphDefinition{
			ObjectMeta: metav1.ObjectMeta{Name: "iftest"},
			Spec: v1alpha1.ResourceGraphDefinitionSpec{
				Resources: []*v1alpha1.Resource{
					{
						ID: "always",
						Template: rawResource(map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "always", "namespace": "default"},
							"data":       map[string]any{"k": "v"},
						}),
					},
					{
						ID: "guarded",
						Template: rawResource(map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "guarded", "namespace": "default"},
							"data":       map[string]any{"k": "v"},
						}),
						IncludeWhen: []string{"${schema.spec.enabled}"},
					},
				},
			},
		}
	}

	compileWithEnabled := func(t *testing.T, enabled bool) (*graphruntime.Runtime, *v1alpha1.Graph) {
		t.Helper()
		g, err := ResourceGraphDefinitionToGraph(makeRGD())
		require.NoError(t, err)

		// Verify the includeWhen expression survives translation.
		var guardedNode v1alpha1.Node
		for _, n := range g.Spec.Nodes {
			if n.ID == "guarded" {
				guardedNode = n
			}
		}
		require.Equal(t, []string{"${schema.spec.enabled}"}, guardedNode.IncludeWhen,
			"includeWhen must survive translation")

		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "example.com/v1alpha1",
			"kind":       "IfTest",
			"metadata":   map[string]any{"name": "demo", "namespace": "default"},
			"spec":       map[string]any{"enabled": enabled},
		}}
		schemaNode, err := InstanceSchemaNode(instance)
		require.NoError(t, err)
		g.Spec.Nodes = append([]v1alpha1.Node{schemaNode}, g.Spec.Nodes...)

		fakeResolver, disco := testk8s.NewFakeResolver()
		rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
		prog, err := compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
		require.NoError(t, err, "includeWhen graph must compile")

		rt := graphruntime.New(prog, g)
		// Publish schema so the includeWhen expression can be evaluated.
		schemaObjs, err := rt.Node(SchemaNodeID).Resolve()
		require.NoError(t, err)
		rt.Set(SchemaNodeID, schemaObjs[0].Object)
		return rt, g
	}

	t.Run("enabled=false → guarded node is ignored", func(t *testing.T) {
		rt, _ := compileWithEnabled(t, false)
		ignored, err := rt.Node("guarded").IsIgnored()
		require.NoError(t, err)
		assert.True(t, ignored, "guarded node must be ignored when spec.enabled=false")
	})

	t.Run("enabled=true → guarded node is NOT ignored", func(t *testing.T) {
		rt, _ := compileWithEnabled(t, true)
		ignored, err := rt.Node("guarded").IsIgnored()
		require.NoError(t, err)
		assert.False(t, ignored, "guarded node must not be ignored when spec.enabled=true")
	})
}

// TestReconcileBehavior_ReadyWhen verifies that an RGD resource with readyWhen
// translates to a Graph node whose compiled readyWhen expressions are
// present. We assert: (a) compile succeeds, (b) before SetObserved the node
// returns ErrWaitingForReadiness, (c) after SetObserved with the condition
// met the node reports ready.
//
// This test covers translation + compile + the runtime readiness gate. The
// end-to-end readiness behaviour driven by observed cluster state is exercised
// by the integration suites.
func TestReconcileBehavior_ReadyWhen(t *testing.T) {
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "rwtest"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm", "namespace": "default"},
						"data":       map[string]any{"ready": "yes"},
					}),
					ReadyWhen: []string{"${cm.data.ready == 'yes'}"},
				},
			},
		},
	}

	g, err := ResourceGraphDefinitionToGraph(rgd)
	require.NoError(t, err)

	// Verify readyWhen survives translation.
	require.Equal(t, []string{"${cm.data.ready == 'yes'}"}, g.Spec.Nodes[0].ReadyWhen,
		"readyWhen must survive translation")

	// Inject a minimal schema (no spec fields needed for this test).
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "RWTest",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{},
	}}
	schemaNode, err := InstanceSchemaNode(instance)
	require.NoError(t, err)
	g.Spec.Nodes = append([]v1alpha1.Node{schemaNode}, g.Spec.Nodes...)

	// Compile — the readyWhen expression must type-check.
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	prog, err := compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
	require.NoError(t, err, "readyWhen graph must compile")

	rt := graphruntime.New(prog, g)
	// Publish schema def.
	schemaObjs, err := rt.Node(SchemaNodeID).Resolve()
	require.NoError(t, err)
	rt.Set(SchemaNodeID, schemaObjs[0].Object)

	// (b) Before SetObserved: readyWhen must gate with ErrWaitingForReadiness.
	err = rt.Node("cm").CheckReadiness()
	require.Error(t, err)
	require.ErrorIs(t, err, graphruntime.ErrWaitingForReadiness,
		"no observed state → must be ErrWaitingForReadiness")

	// (c) After Resolve + SetObserved with the ready condition met: ready.
	objs, err := rt.Node("cm").Resolve()
	require.NoError(t, err)
	require.Len(t, objs, 1)
	rt.Set("cm", objs[0].Object)
	rt.Node("cm").SetObserved(objs, objs)

	err = rt.Node("cm").CheckReadiness()
	require.NoError(t, err, "readyWhen condition met → node must be ready")
}

// TestReconcileBehavior_ExternalRef verifies that a named RGD externalRef
// translates to a Graph Node.Ref and the resulting Graph compiles.
//
// This test covers translation + compile only. The executor apply path for ref
// nodes (a read-only GET/LIST that publishes the fetched object into scope) is
// exercised by the integration suites.
func TestReconcileBehavior_ExternalRef(t *testing.T) {
	rgd := &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "reftest"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Resources: []*v1alpha1.Resource{
				// A ref node: import an existing ConfigMap into scope.
				{
					ID: "cfg",
					ExternalRef: &v1alpha1.ExternalRef{
						APIVersion: "v1",
						Kind:       "ConfigMap",
						Metadata: v1alpha1.ExternalRefMetadata{
							Name:      "cluster-config",
							Namespace: "kube-system",
						},
					},
				},
				// A template node that uses the ref's fields.
				{
					ID: "consumer",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "consumer", "namespace": "default"},
						"data":       map[string]any{"from": "${cfg.metadata.name}"},
					}),
				},
			},
		},
	}

	g, err := ResourceGraphDefinitionToGraph(rgd)
	require.NoError(t, err)
	require.Len(t, g.Spec.Nodes, 2)

	// Translation: the externalRef resource must become a Node.Ref.
	var refNode v1alpha1.Node
	for _, n := range g.Spec.Nodes {
		if n.ID == "cfg" {
			refNode = n
		}
	}
	require.NotNil(t, refNode.Ref, "externalRef must translate to Node.Ref")
	assert.Equal(t, "v1", refNode.Ref.APIVersion)
	assert.Equal(t, "ConfigMap", refNode.Ref.Kind)
	assert.Equal(t, "cluster-config", refNode.Ref.Metadata.Name)
	assert.Equal(t, "kube-system", refNode.Ref.Metadata.Namespace)
	assert.Nil(t, refNode.Template, "Ref node must not have Template set")

	// Inject a schema def (no instance fields needed for this test).
	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "RefTest",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{},
	}}
	schemaNode, err := InstanceSchemaNode(instance)
	require.NoError(t, err)
	g.Spec.Nodes = append([]v1alpha1.Node{schemaNode}, g.Spec.Nodes...)

	// Compile — the Ref node + downstream template must compile cleanly.
	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	_, err = compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
	require.NoError(t, err, "externalRef (named) translated graph must compile")

	// This test stops at translation + compile; it does not call executor.Apply.
	// The executor's ref-node apply path (read-only cluster fetch published into
	// scope) is covered by the integration suites.
}

func nestedString(t *testing.T, obj map[string]any, path ...string) string {
	t.Helper()
	v, found, err := unstructured.NestedString(obj, path...)
	require.NoError(t, err)
	require.Truef(t, found, "path %v not found in %v", path, obj)
	return v
}
