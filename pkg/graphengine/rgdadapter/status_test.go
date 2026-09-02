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

// statusRaw serialises a Go map as runtime.RawExtension (JSON).
func statusRaw(m map[string]any) runtime.RawExtension {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return runtime.RawExtension{Raw: b}
}

// buildRGDWithStatus constructs a minimal RGD with one ConfigMap resource
// and the supplied status block.
func buildRGDWithStatus(statusFields map[string]any) *v1alpha1.ResourceGraphDefinition {
	return &v1alpha1.ResourceGraphDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "statustest"},
		Spec: v1alpha1.ResourceGraphDefinitionSpec{
			Resources: []*v1alpha1.Resource{
				{
					ID: "cm1",
					Template: rawResource(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "cm1", "namespace": "default"},
						"data":       map[string]any{"key": "val"},
					}),
				},
			},
			Schema: &v1alpha1.Schema{
				Spec:   runtime.RawExtension{Raw: []byte(`{"name":{"type":"string"}}`)},
				Status: statusRaw(statusFields),
			},
		},
	}
}

// compileAndSeedRuntime translates the RGD to a Graph, compiles it, seeds the
// schema def node, and publishes the supplied resource values into scope.
// Returns a runtime ready for status/condition projection.
func compileAndSeedRuntime(
	t *testing.T,
	rgd *v1alpha1.ResourceGraphDefinition,
	instance *unstructured.Unstructured,
	// resourceValues: nodeID → object map to publish into scope.
	resourceValues map[string]map[string]any,
) *graphruntime.Runtime {
	t.Helper()

	g, err := ResourceGraphDefinitionToGraph(rgd)
	require.NoError(t, err)

	schemaNode, err := InstanceSchemaNode(instance)
	require.NoError(t, err)
	g.Spec.Nodes = append([]v1alpha1.Node{schemaNode}, g.Spec.Nodes...)

	fakeResolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	prog, err := compiler.NewCompilerWithDependencies(fakeResolver, rm).Compile(g)
	require.NoError(t, err, "graph must compile")

	rt := graphruntime.New(prog, g)

	// Publish schema def.
	schemaObjs, err := rt.Node(SchemaNodeID).Resolve()
	require.NoError(t, err)
	rt.Set(SchemaNodeID, schemaObjs[0].Object)

	// Publish any caller-supplied node values (simulates executor.Apply output).
	for id, obj := range resourceValues {
		rt.Set(id, obj)
	}

	return rt
}

// TestProjectInstanceConditions_Basic verifies that a status.conditions block
// with a single runtime.newCondition(…) expression is evaluated and returned
// as a []library.Condition in declaration order.
func TestProjectInstanceConditions_Basic(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		// A single condition that is always True.
		"conditions": []any{
			"${runtime.newCondition({type: 'Ready', status: 'True', reason: 'ResourcesReady', message: 'all resources applied'})}",
		},
	})

	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "StatusTest",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"name": "myapp"},
	}}

	rt := compileAndSeedRuntime(t, rgd, instance, nil)

	conditions, incomplete, err := ProjectInstanceConditions(rt, rgd, nil, 0)
	require.NoError(t, err)
	assert.False(t, incomplete)
	require.Len(t, conditions, 1)

	cond := conditions[0]
	assert.Equal(t, "Ready", cond.ConditionType)
	assert.Equal(t, "True", cond.Status)
	assert.Equal(t, "ResourcesReady", cond.Reason)
	assert.Equal(t, "all resources applied", cond.Message)
}

// TestProjectInstanceConditions_BuiltinReference verifies runtime.condition(
// schema, 'X') reads the kro built-ins passed by the caller, not the instance
// snapshot's wire conditions.
func TestProjectInstanceConditions_BuiltinReference(t *testing.T) {
	rgd := buildRGDWithStatus(map[string]any{
		"conditions": []any{
			"${runtime.newCondition({type: 'Ready', status: runtime.condition(schema, 'ResourcesReady').status})}",
		},
	})

	instance := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1alpha1",
		"kind":       "StatusTest",
		"metadata":   map[string]any{"name": "demo", "namespace": "default"},
		"spec":       map[string]any{"name": "myapp"},
	}}

	rt := compileAndSeedRuntime(t, rgd, instance, nil)

	builtins := []v1alpha1.Condition{{Type: "ResourcesReady", Status: "True"}}
	conditions, incomplete, err := ProjectInstanceConditions(rt, rgd, builtins, 0)
	require.NoError(t, err)
	assert.False(t, incomplete)
	require.Len(t, conditions, 1)
	assert.Equal(t, "Ready", conditions[0].ConditionType)
	assert.Equal(t, "True", conditions[0].Status)
}
