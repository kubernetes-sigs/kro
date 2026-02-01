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

package runtime

import (
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
)

func TestNode_IsIgnored(t *testing.T) {
	tests := []struct {
		name        string
		node        *Node
		wantIgnored bool
		wantErr     bool
	}{
		{
			name:        "instance nodes are never ignored",
			node:        newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).withIncludeWhen("false").build(),
			wantIgnored: false,
		},
		{
			name:        "no includeWhen means not ignored",
			node:        newTestNode("test", graph.NodeTypeResource).build(),
			wantIgnored: false,
		},
		{
			name: "ignored dependency is contagious",
			node: newTestNode("child", graph.NodeTypeResource).
				withDep(newTestNode("parent", graph.NodeTypeResource).
					withDep(newTestNode("grandparent", graph.NodeTypeInstance).build()).
					withResolvedIncludeWhen("false", false).build()).build(),
			wantIgnored: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ignored, err := tt.node.IsIgnored()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantIgnored, ignored)
			}
		})
	}
}

func TestNode_IsReady(t *testing.T) {
	tests := []struct {
		name      string
		node      *Node
		wantReady bool
		wantErr   bool
	}{
		{
			name:      "no readyWhen expressions means ready",
			node:      newTestNode("test", graph.NodeTypeResource).build(),
			wantReady: true,
		},
		{
			name:      "no observed state means not ready",
			node:      newTestNode("test", graph.NodeTypeResource).withReadyWhen("test.status.ready").build(),
			wantReady: false,
		},
		{
			name: "ignored nodes are always ready",
			node: newTestNode("child", graph.NodeTypeResource).
				withDep(newTestNode("ignoredParent", graph.NodeTypeResource).
					withDep(newTestNode("schema", graph.NodeTypeInstance).build()).
					withResolvedIncludeWhen("false", false).build()).
				withReadyWhen("test.status.ready").build(),
			wantReady: true, // ignored, so ready
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := tt.node.IsReady()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantReady, ready)
			}
		})
	}
}

func TestNode_SetObserved(t *testing.T) {
	tests := []struct {
		name      string
		node      *Node
		observed  []*unstructured.Unstructured
		wantNames []string
	}{
		{
			name: "non-collection stores directly",
			node: newTestNode("test", graph.NodeTypeResource).build(),
			observed: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "pod-1"),
			},
			wantNames: []string{"pod-1"},
		},
		{
			name: "collection orders by desired",
			node: newTestNode("test", graph.NodeTypeCollection).
				withDesired(
					newUnstructured("v1", "Pod", "ns", "a"),
					newUnstructured("v1", "Pod", "ns", "b"),
					newUnstructured("v1", "Pod", "ns", "c"),
				).build(),
			observed: []*unstructured.Unstructured{
				newUnstructured("v1", "Pod", "ns", "c"),
				newUnstructured("v1", "Pod", "ns", "a"),
				newUnstructured("v1", "Pod", "ns", "b"),
			},
			wantNames: []string{"a", "b", "c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.node.SetObserved(tt.observed)
			require.Len(t, tt.node.observed, len(tt.wantNames))
			for i, name := range tt.wantNames {
				assert.Equal(t, name, tt.node.observed[i].GetName())
			}
		})
	}
}

func TestNode_BuildContext(t *testing.T) {
	tests := []struct {
		name         string
		node         *Node
		onlyFilter   []string
		wantKeys     []string
		notWantKeys  []string
		checkBuckets bool
	}{
		{
			name: "builds context from observed deps",
			node: newTestNode("subnet", graph.NodeTypeResource).
				withDep(newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"name": "myapp"}}).build()).
				withDep(newTestNode("vpc", graph.NodeTypeResource).
					withObserved(map[string]any{"status": map[string]any{"vpcId": "vpc-123"}}).build()).
				build(),
			wantKeys: []string{"schema", "vpc"},
		},
		{
			name: "skips deps without observed",
			node: newTestNode("test", graph.NodeTypeResource).
				withDep(newTestNode("missing", graph.NodeTypeResource).build()).
				build(),
			notWantKeys: []string{"missing"},
		},
		{
			name: "collection deps return list",
			node: newTestNode("policy", graph.NodeTypeResource).
				withDep(newTestNode("buckets", graph.NodeTypeCollection).
					withObserved(
						map[string]any{"metadata": map[string]any{"name": "bucket-1"}},
						map[string]any{"metadata": map[string]any{"name": "bucket-2"}},
					).build()).
				build(),
			wantKeys:     []string{"buckets"},
			checkBuckets: true,
		},
		{
			name: "only filter limits context",
			node: newTestNode("test", graph.NodeTypeResource).
				withDep(newTestNode("a", graph.NodeTypeResource).
					withObserved(map[string]any{"id": "a"}).build()).
				withDep(newTestNode("b", graph.NodeTypeResource).
					withObserved(map[string]any{"id": "b"}).build()).
				build(),
			onlyFilter:  []string{"a"},
			wantKeys:    []string{"a"},
			notWantKeys: []string{"b"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.node.buildContext(tt.onlyFilter...)

			for _, key := range tt.wantKeys {
				assert.Contains(t, ctx, key)
			}
			for _, key := range tt.notWantKeys {
				assert.NotContains(t, ctx, key)
			}
			if tt.checkBuckets {
				buckets, ok := ctx["buckets"].([]any)
				require.True(t, ok, "collection should be a list")
				assert.Len(t, buckets, 2)
			}
		})
	}
}

func TestNode_ContextDependencyIDs(t *testing.T) {
	tests := []struct {
		name            string
		node            *Node
		iterCtx         map[string]any
		wantSingles     []string
		wantCollections []string
		wantIterators   []string
	}{
		{
			name: "categorizes deps by type",
			node: newTestNode("test", graph.NodeTypeResource).
				withDep(newTestNode("schema", graph.NodeTypeInstance).build()).
				withDep(newTestNode("vpc", graph.NodeTypeResource).build()).
				withDep(newTestNode("buckets", graph.NodeTypeCollection).build()).
				withDep(newTestNode("external", graph.NodeTypeExternal).build()).
				build(),
			wantSingles:     []string{"schema", "vpc", "external"},
			wantCollections: []string{"buckets"},
		},
		{
			name:          "includes iterator context",
			node:          newTestNode("test", graph.NodeTypeCollection).build(),
			iterCtx:       map[string]any{"region": "us-east-1", "az": "a"},
			wantIterators: []string{"region", "az"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			singles, collections, iterators := tt.node.contextDependencyIDs(tt.iterCtx)

			assert.Len(t, singles, len(tt.wantSingles))
			for _, s := range tt.wantSingles {
				assert.Contains(t, singles, s)
			}
			assert.Len(t, collections, len(tt.wantCollections))
			for _, c := range tt.wantCollections {
				assert.Contains(t, collections, c)
			}
			assert.Len(t, iterators, len(tt.wantIterators))
			for _, i := range tt.wantIterators {
				assert.Contains(t, iterators, i)
			}
		})
	}
}

func TestNode_GetDesired_Caching(t *testing.T) {
	cached := []*unstructured.Unstructured{
		newUnstructured("v1", "Pod", "ns", "cached"),
	}
	node := newTestNode("test", graph.NodeTypeResource).
		withDesired(cached...).build()

	result, err := node.GetDesired()

	require.NoError(t, err)
	assert.Same(t, cached[0], result[0])
}

func TestNode_GetDesired_DependencyNotReady(t *testing.T) {
	tests := []struct {
		name    string
		node    *Node
		wantNil bool
		wantErr error
	}{
		{
			name: "returns ErrDataPending when dependency not ready",
			node: newTestNode("subnet", graph.NodeTypeResource).
				withDep(newTestNode("vpc", graph.NodeTypeResource).
					withReadyWhen("vpc.status.ready == true").build()).
				build(),
			wantNil: true,
			wantErr: ErrDataPending,
		},
		{
			name: "schema dependency does not block GetDesired",
			node: newTestNode("deployment", graph.NodeTypeResource).
				withDep(newTestNode("schema", graph.NodeTypeInstance).build()).
				withTemplate(map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "test"},
				}).
				build(),
			wantNil: false,
			wantErr: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.node.GetDesired()
			if tt.wantErr != nil {
				assert.ErrorIs(t, err, tt.wantErr)
			} else {
				assert.NoError(t, err)
			}
			if tt.wantNil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
			}
		})
	}
}

func TestNode_IsReady_Collection(t *testing.T) {
	tests := []struct {
		name      string
		node      *Node
		wantReady bool
	}{
		{
			name: "collection with nil desired is not ready",
			node: newTestNode("buckets", graph.NodeTypeCollection).
				withReadyWhen("each.status.ready == true").build(),
			wantReady: false,
		},
		{
			name: "collection with resolved empty desired is ready",
			node: func() *Node {
				n := newTestNode("buckets", graph.NodeTypeCollection).
					withReadyWhen("each.status.ready == true").build()
				n.desired = []*unstructured.Unstructured{}
				return n
			}(),
			wantReady: true,
		},
		{
			name: "collection with fewer observed than desired is not ready",
			node: newTestNode("buckets", graph.NodeTypeCollection).
				withDesired(
					newUnstructured("v1", "Pod", "ns", "bucket-1"),
					newUnstructured("v1", "Pod", "ns", "bucket-2"),
				).
				withObservedUnstructured(newUnstructured("v1", "Pod", "ns", "bucket-1")).
				withReadyWhen("each.status.ready == true").build(),
			wantReady: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := tt.node.IsReady()
			assert.NoError(t, err)
			assert.Equal(t, tt.wantReady, ready)
		})
	}
}

func TestNode_IsIgnored_WithCEL(t *testing.T) {
	tests := []struct {
		name        string
		node        func() *Node
		wantIgnored bool
		wantErr     bool
		errIs       error
		errNotIs    error
		errContain  string
	}{
		{
			name: "includeWhen evaluates to true - not ignored",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"enabled": true}}).build()
				return newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).
					withIncludeWhen("schema.spec.enabled").build()
			},
			wantIgnored: false,
		},
		{
			name: "includeWhen evaluates to false - ignored",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"enabled": false}}).build()
				return newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).
					withIncludeWhen("schema.spec.enabled").build()
			},
			wantIgnored: true,
		},
		{
			name: "multiple includeWhen - all must be true",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"enabled": true, "ready": false}}).build()
				return newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).
					withIncludeWhen("schema.spec.enabled", "schema.spec.ready").build()
			},
			wantIgnored: true,
		},
		{
			name: "division by zero in includeWhen",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"count": int64(10), "divisor": int64(0)}}).build()
				return newTestNode("optional", graph.NodeTypeResource).
					withDep(schema).
					withIncludeWhen("schema.spec.count / schema.spec.divisor > 5").build()
			},
			wantErr:    true,
			errContain: "division by zero",
		},
		{
			name: "missing field in includeWhen returns error (not pending)",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"items": []any{}}}).build()
				return newTestNode("optional", graph.NodeTypeResource).
					withDep(schema).
					withIncludeWhen("schema.spec.items[0].enabled").build()
			},
			wantErr:  true,
			errNotIs: ErrDataPending, // should be a direct CEL error, not ErrDataPending
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ignored, err := tt.node().IsIgnored()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errIs != nil {
					assert.ErrorIs(t, err, tt.errIs)
				}
				if tt.errNotIs != nil {
					assert.NotErrorIs(t, err, tt.errNotIs)
				}
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantIgnored, ignored)
		})
	}
}

func TestNode_IsSingleResourceReady_WithCEL(t *testing.T) {
	tests := []struct {
		name       string
		node       func() *Node
		wantReady  bool
		wantErr    bool
		errContain string
	}{
		{
			name: "readyWhen evaluates to true",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("vpc", graph.NodeTypeResource).
					withDep(schema).
					withObserved(map[string]any{"status": map[string]any{"ready": true}}).
					withReadyWhen("vpc.status.ready == true").build()
			},
			wantReady: true,
		},
		{
			name: "readyWhen evaluates to false",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("vpc", graph.NodeTypeResource).
					withDep(schema).
					withObserved(map[string]any{"status": map[string]any{"ready": false}}).
					withReadyWhen("vpc.status.ready == true").build()
			},
			wantReady: false,
		},
		{
			name: "multiple readyWhen - all must be true",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("vpc", graph.NodeTypeResource).
					withDep(schema).
					withObserved(map[string]any{"status": map[string]any{"ready": true, "state": "pending"}}).
					withReadyWhen("vpc.status.ready == true", "vpc.status.state == 'available'").build()
			},
			wantReady: false,
		},
		{
			name: "division by zero in readyWhen",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).
					withObserved(map[string]any{"status": map[string]any{"replicas": int64(3), "errorDivisor": int64(0)}}).
					withReadyWhen("deployment.status.replicas / deployment.status.errorDivisor > 0").build()
			},
			wantErr:    true,
			errContain: "division by zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := tt.node().IsReady()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantReady, ready)
		})
	}
}

func TestNode_EvaluateExprs(t *testing.T) {
	tests := []struct {
		name           string
		node           func() *Node
		wantValues     map[string]any
		notWantKeys    []string
		wantHasPending bool
		wantErr        bool
	}{
		{
			name: "evaluates static expressions",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"name": "myapp", "replicas": int64(3)}}).build()
				return newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).
					withTemplateExpr("schema.spec.name", variable.ResourceVariableKindStatic).
					withTemplateExpr("schema.spec.replicas", variable.ResourceVariableKindStatic).build()
			},
			wantValues: map[string]any{"schema.spec.name": "myapp", "schema.spec.replicas": int64(3)},
		},
		{
			name: "skips iteration expressions",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"name": "myapp"}}).build()
				return newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).
					withTemplateExpr("schema.spec.name", variable.ResourceVariableKindStatic).
					withTemplateExpr("iterator", variable.ResourceVariableKindIteration).build()
			},
			wantValues:  map[string]any{"schema.spec.name": "myapp"},
			notWantKeys: []string{"iterator"},
		},
		{
			name: "uses cached values",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				n := newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).build()
				n.templateExprs = []*expressionEvaluationState{
					{
						Expression:    krocel.NewUncompiled("schema.spec.name"),
						Kind:          variable.ResourceVariableKindStatic,
						Resolved:      true,
						ResolvedValue: "cached-name",
					},
				}
				return n
			},
			wantValues: map[string]any{"schema.spec.name": "cached-name"},
		},
		{
			name: "string division type error",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"value": "not-a-number"}}).build()
				return newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withTemplateExpr("schema.spec.value / 2", variable.ResourceVariableKindStatic).build()
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			values, hasPending, err := tt.node().evaluateExprsFiltered(nil, false)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantHasPending, hasPending)
			for k, v := range tt.wantValues {
				assert.Equal(t, v, values[k])
			}
			for _, k := range tt.notWantKeys {
				assert.NotContains(t, values, k)
			}
		})
	}
}

func TestNode_UpsertToTemplate(t *testing.T) {
	tests := []struct {
		name        string
		node        *Node
		base        *unstructured.Unstructured
		values      map[string]any
		wantVpcId   string
		wantNoVpcId bool
	}{
		{
			name: "upserts values to template",
			node: newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
				withTemplateVar("status.vpcId", "vpc.status.vpcId").build(),
			base: &unstructured.Unstructured{
				Object: map[string]any{
					"apiVersion": "example.com/v1",
					"kind":       "Test",
					"metadata":   map[string]any{"name": "test"},
				},
			},
			values:    map[string]any{"vpc.status.vpcId": "vpc-12345"},
			wantVpcId: "vpc-12345",
		},
		{
			name: "skips vars without expressions",
			node: newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
				withTemplateVar("status.vpcId").build(),
			base: &unstructured.Unstructured{
				Object: map[string]any{"metadata": map[string]any{"name": "test"}},
			},
			values:      map[string]any{},
			wantNoVpcId: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.node.upsertToTemplate(tt.base, tt.values)
			vpcId, found, err := unstructured.NestedString(result.Object, "status", "vpcId")
			assert.NoError(t, err)
			if tt.wantNoVpcId {
				assert.False(t, found)
			} else {
				assert.True(t, found)
				assert.Equal(t, tt.wantVpcId, vpcId)
			}
		})
	}
}

func TestNode_IsCollectionReady_WithCEL(t *testing.T) {
	tests := []struct {
		name       string
		node       func() *Node
		wantReady  bool
		wantErr    bool
		errContain string
	}{
		{
			name: "empty collection (size 0) is ready",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				n := newTestNode("pods", graph.NodeTypeCollection).
					withDep(schema).
					withReadyWhen("each.status.ready == true").build()
				n.desired = []*unstructured.Unstructured{}
				return n
			},
			wantReady: true,
		},
		{
			name: "all items ready",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("pods", graph.NodeTypeCollection).
					withDep(schema).
					withDesired(
						newUnstructured("v1", "Pod", "ns", "pod-1"),
						newUnstructured("v1", "Pod", "ns", "pod-2"),
					).
					withObserved(
						map[string]any{"status": map[string]any{"ready": true}},
						map[string]any{"status": map[string]any{"ready": true}},
					).
					withReadyWhen("each.status.ready == true").build()
			},
			wantReady: true,
		},
		{
			name: "one item not ready",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("pods", graph.NodeTypeCollection).
					withDep(schema).
					withDesired(
						newUnstructured("v1", "Pod", "ns", "pod-1"),
						newUnstructured("v1", "Pod", "ns", "pod-2"),
					).
					withObserved(
						map[string]any{"status": map[string]any{"ready": true}},
						map[string]any{"status": map[string]any{"ready": false}},
					).
					withReadyWhen("each.status.ready == true").build()
			},
			wantReady: false,
		},
		{
			name: "fewer observed than desired",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("pods", graph.NodeTypeCollection).
					withDep(schema).
					withDesired(
						newUnstructured("v1", "Pod", "ns", "pod-1"),
						newUnstructured("v1", "Pod", "ns", "pod-2"),
					).
					withObserved(
						map[string]any{"status": map[string]any{"ready": true}},
					).
					withReadyWhen("each.status.ready == true").build()
			},
			wantReady: false,
		},
		{
			name: "CEL data pending returns false",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("pods", graph.NodeTypeCollection).
					withDep(schema).
					withDesired(newUnstructured("v1", "Pod", "ns", "pod-1")).
					withObserved(map[string]any{}).
					withReadyWhen("each.status.ready == true").build()
			},
			wantReady: false,
		},
		{
			name: "multiple readyWhen - all must pass",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("pods", graph.NodeTypeCollection).
					withDep(schema).
					withDesired(newUnstructured("v1", "Pod", "ns", "pod-1")).
					withObserved(map[string]any{
						"status": map[string]any{"ready": true, "phase": "Pending"},
					}).
					withReadyWhen("each.status.ready == true").
					withReadyWhen("each.status.phase == 'Running'").build()
			},
			wantReady: false,
		},
		{
			name: "division by zero in readyWhen",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				return newTestNode("pods", graph.NodeTypeCollection).
					withDep(schema).
					withDesired(newUnstructured("v1", "Pod", "ns", "pod-1")).
					withObserved(map[string]any{
						"status": map[string]any{"total": int64(10), "divisor": int64(0)},
					}).
					withReadyWhen("each.status.total / each.status.divisor > 0").build()
			},
			wantErr:    true,
			errContain: "division by zero",
		},
		// Note: The test case "schema is not available in readyWhen" was removed because
		// this constraint is now enforced at compile time by the graph builder, not at runtime.
		// The builder validates that readyWhen expressions only reference self or "each".
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := tt.node().IsReady()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantReady, ready)
		})
	}
}

func TestNode_HardResolveSingleResource(t *testing.T) {
	tests := []struct {
		name       string
		node       *Node
		wantLen    int
		wantName   string
		wantErr    bool
		errIs      error
		errContain string
	}{
		{
			name: "template without expressions returns template copy",
			node: newTestNode("test", graph.NodeTypeResource).
				withTemplate(map[string]any{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata":   map[string]any{"name": "test"},
				}).build(),
			wantLen:  1,
			wantName: "test",
		},
		{
			name: "resolves template with schema values",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"name": "myapp"},
					}).build()
				return newTestNode("deployment", graph.NodeTypeResource).
					withDep(schema).
					withTemplate(map[string]any{
						"apiVersion": "apps/v1",
						"kind":       "Deployment",
						"metadata":   map[string]any{"name": "${schema.spec.name}"},
					}).
					withTemplateVar("metadata.name", "schema.spec.name").
					withTemplateExpr("schema.spec.name", variable.ResourceVariableKindStatic).
					build()
			}(),
			wantLen:  1,
			wantName: "myapp",
		},
		{
			name: "returns ErrDataPending when dep not ready",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{}).build()
				vpc := newTestNode("vpc", graph.NodeTypeResource).
					withDep(schema).build() // no observed state
				return newTestNode("subnet", graph.NodeTypeResource).
					withDep(schema).
					withDep(vpc).
					withTemplate(map[string]any{
						"metadata": map[string]any{"name": "${vpc.status.vpcId}"},
					}).
					withTemplateVar("metadata.name", "vpc.status.vpcId").
					withTemplateExpr("vpc.status.vpcId", variable.ResourceVariableKindDynamic).
					build()
			}(),
			wantErr: true,
			errIs:   ErrDataPending,
		},
		{
			name: "division by zero in template",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"total": int64(100), "divisor": int64(0)},
					}).build()
				return newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "test"},
						"data":     map[string]any{"result": "${schema.spec.total / schema.spec.divisor}"},
					}).
					withTemplateVar("data.result", "schema.spec.total / schema.spec.divisor").
					withTemplateExpr("schema.spec.total / schema.spec.divisor", variable.ResourceVariableKindStatic).
					build()
			}(),
			wantErr:    true,
			errContain: "division by zero",
		},
		{
			name: "modulus by zero",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"value": int64(10), "modulo": int64(0)},
					}).build()
				return newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "test"},
						"data":     map[string]any{"result": "${schema.spec.value % schema.spec.modulo}"},
					}).
					withTemplateVar("data.result", "schema.spec.value % schema.spec.modulo").
					withTemplateExpr("schema.spec.value % schema.spec.modulo", variable.ResourceVariableKindStatic).
					build()
			}(),
			wantErr:    true,
			errContain: "modulus by zero",
		},
		{
			name: "conditional guard prevents division error",
			node: func() *Node {
				const expr = "schema.spec.divisor != 0 ? schema.spec.total / schema.spec.divisor : 0"
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"total": int64(100), "divisor": int64(0)},
					}).build()
				return newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "test"},
						"data":     map[string]any{"result": "${" + expr + "}"},
					}).
					withTemplateVar("data.result", expr).
					withTemplateExpr(expr, variable.ResourceVariableKindStatic).
					build()
			}(),
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.node.hardResolveSingleResource(tt.node.templateVars)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errIs != nil {
					assert.ErrorIs(t, err, tt.errIs)
				}
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
				return
			}
			assert.NoError(t, err)
			if tt.wantLen == 0 {
				assert.Nil(t, result)
			} else {
				require.Len(t, result, tt.wantLen)
				if tt.wantName != "" {
					assert.Equal(t, tt.wantName, result[0].GetName())
				}
			}
		})
	}
}

func TestNode_SoftResolve(t *testing.T) {
	tests := []struct {
		name       string
		node       *Node
		wantNil    bool
		wantVpcId  string
		wantErr    bool
		errContain string
	}{
		{
			name: "empty template returns template copy",
			node: newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
				withTemplate(map[string]any{"status": map[string]any{}}).build(),
			wantNil: false,
		},
		{
			name: "resolves values from template",
			node: newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
				withDep(newTestNode("vpc", graph.NodeTypeResource).
					withObserved(map[string]any{"status": map[string]any{"vpcId": "vpc-123"}}).build()).
				withTemplate(map[string]any{"status": map[string]any{"vpcId": "${vpc.status.vpcId}"}}).
				withTemplateVar("status.vpcId", "vpc.status.vpcId").
				withTemplateExpr("vpc.status.vpcId", variable.ResourceVariableKindDynamic).
				build(),
			wantNil:   false,
			wantVpcId: "vpc-123",
		},
		{
			name: "skips fields with pending expressions",
			node: newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
				withDep(newTestNode("vpc", graph.NodeTypeResource).
												withObserved(map[string]any{"status": map[string]any{"vpcId": "vpc-123"}}).build()).
				withDep(newTestNode("subnet", graph.NodeTypeResource).build()). // no observed - pending
				withTemplate(map[string]any{"status": map[string]any{
					"vpcId":    "${vpc.status.vpcId}",
					"subnetId": "${subnet.status.subnetId}",
				}}).
				withTemplateVar("status.vpcId", "vpc.status.vpcId").
				withTemplateVar("status.subnetId", "subnet.status.subnetId").
				withTemplateExpr("vpc.status.vpcId", variable.ResourceVariableKindDynamic).
				withTemplateExpr("subnet.status.subnetId", variable.ResourceVariableKindDynamic).
				build(),
			wantNil:   false,
			wantVpcId: "vpc-123", // vpc resolved, subnet skipped (stays as template string but filtered out)
		},
		{
			name: "fatal error still propagates",
			node: newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
				withDep(newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"total": int64(10), "divisor": int64(0)}}).build()).
				withTemplate(map[string]any{"status": map[string]any{"result": "${schema.spec.total / schema.spec.divisor}"}}).
				withTemplateVar("status.result", "schema.spec.total / schema.spec.divisor").
				withTemplateExpr("schema.spec.total / schema.spec.divisor", variable.ResourceVariableKindDynamic).
				build(),
			wantErr:    true,
			errContain: "division by zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.node.softResolve()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
				return
			}
			assert.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, result)
			} else {
				require.NotNil(t, result)
				require.Len(t, result, 1)
				if tt.wantVpcId != "" {
					vpcId, _, _ := unstructured.NestedString(result[0].Object, "status", "vpcId")
					assert.Equal(t, tt.wantVpcId, vpcId)
				}
			}
		})
	}
}

func TestNode_EvaluateForEach(t *testing.T) {
	tests := []struct {
		name    string
		node    *Node
		wantLen int
		wantErr bool
		errIs   error
	}{
		{
			name:    "no forEach returns nil",
			node:    newTestNode("test", graph.NodeTypeCollection).build(),
			wantLen: 0,
		},
		{
			name: "single dimension forEach",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"regions": []any{"us-east-1", "us-west-2"},
						},
					}).build()
				n := newTestNode("buckets", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").build()
				n.Spec.ForEach = []graph.ForEachDimension{{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")}}
				return n
			}(),
			wantLen: 2,
		},
		{
			name: "multi-dimension forEach (cartesian product)",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"regions": []any{"us-east-1", "us-west-2"},
							"azs":     []any{"a", "b"},
						},
					}).build()
				n := newTestNode("subnets", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions", "schema.spec.azs").build()
				n.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
					{Name: "az", Expression: krocel.NewUncompiled("schema.spec.azs")},
				}
				return n
			}(),
			wantLen: 4, // 2 regions × 2 azs
		},
		{
			name: "empty dimension returns nil",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"regions": []any{}, // empty
						},
					}).build()
				n := newTestNode("buckets", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").build()
				n.Spec.ForEach = []graph.ForEachDimension{{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")}}
				return n
			}(),
			wantLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.node.evaluateForEach()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errIs != nil {
					assert.ErrorIs(t, err, tt.errIs)
				}
				return
			}
			assert.NoError(t, err)
			if tt.wantLen == 0 {
				assert.Nil(t, result)
			} else {
				assert.Len(t, result, tt.wantLen)
			}
		})
	}
}

func TestNode_HardResolveCollection(t *testing.T) {
	tests := []struct {
		name       string
		node       *Node
		wantLen    int
		wantErr    bool
		errIs      error
		errContain string
	}{
		{
			name: "empty forEach returns empty slice",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"regions": []any{}},
					}).build()
				n := newTestNode("buckets", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).build()
				n.Spec.ForEach = []graph.ForEachDimension{{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")}}
				return n
			}(),
			wantLen: 0,
		},
		{
			name: "resolves collection with forEach",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"name":    "app",
							"regions": []any{"east", "west"},
						},
					}).build()
				n := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplateVar("metadata.name", "schema.spec.name + '-' + region").
					withTemplateExpr("schema.spec.name", variable.ResourceVariableKindStatic).
					withTemplateExpr("schema.spec.name + '-' + region", variable.ResourceVariableKindIteration).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${schema.spec.name + '-' + region}"},
					}).build()
				n.Spec.ForEach = []graph.ForEachDimension{{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")}}
				return n
			}(),
			wantLen: 2,
		},
		{
			name: "division by zero in collection iteration",
			node: func() *Node {
				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"items": []any{map[string]any{"value": int64(10), "divisor": int64(0)}},
						},
					}).build()
				n := newTestNode("results", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.items").
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "test"},
						"data":     map[string]any{"result": "${item.value / item.divisor}"},
					}).
					withTemplateVar("data.result", "item.value / item.divisor").
					withTemplateExpr("item.value / item.divisor", variable.ResourceVariableKindIteration).
					build()
				n.Spec.ForEach = []graph.ForEachDimension{{Name: "item", Expression: krocel.NewUncompiled("schema.spec.items")}}
				return n
			}(),
			wantErr:    true,
			errContain: "division by zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.node.hardResolveCollection(tt.node.templateVars, true)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errIs != nil {
					assert.ErrorIs(t, err, tt.errIs)
				}
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
				return
			}
			assert.NoError(t, err)
			if tt.wantLen == 0 {
				assert.NotNil(t, result)
				assert.Len(t, result, 0)
			} else {
				assert.Len(t, result, tt.wantLen)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// Test Helpers - Builder pattern for creating test Nodes
// -----------------------------------------------------------------------------

var testEnv = func() *cel.Env {
	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs([]string{
		"schema", "vpc", "subnet", "deployment", "configmap",
		"pods", "test", "each", "item", "region", "az", "iterator",
		"child", "parent", "grandparent", "ignoredParent", "missing",
		"buckets", "external", "a", "b", "policy", "configs", "results",
		"optional", "subnets",
	}))
	if err != nil {
		panic(err)
	}
	return env
}()

func mustCompileTestExpr(expr string) *krocel.Expression {
	celAST, issues := testEnv.Compile(expr)
	if issues != nil && issues.Err() != nil {
		panic(issues.Err())
	}
	program, err := testEnv.Program(celAST)
	if err != nil {
		panic(err)
	}
	return &krocel.Expression{
		Original: expr,
		Program:  program,
	}
}

// testNodeBuilder provides a fluent API for constructing test Nodes.
type testNodeBuilder struct {
	id               string
	nodeType         graph.NodeType
	deps             map[string]*Node
	observed         []*unstructured.Unstructured
	desired          []*unstructured.Unstructured
	includeWhenExprs []*expressionEvaluationState
	readyWhenExprs   []*expressionEvaluationState
	forEachExprs     []*expressionEvaluationState
	templateExprs    []*expressionEvaluationState
	templateVars     []*variable.ResourceField
	template         *unstructured.Unstructured
}

// newTestNode creates a new test node builder with the given ID and type.
func newTestNode(id string, nodeType graph.NodeType) *testNodeBuilder {
	return &testNodeBuilder{
		id:       id,
		nodeType: nodeType,
		deps:     make(map[string]*Node),
	}
}

// withDep adds a dependency node.
func (b *testNodeBuilder) withDep(node *Node) *testNodeBuilder {
	b.deps[node.Spec.Meta.ID] = node
	return b
}

// withObserved sets observed state from map[string]any objects.
func (b *testNodeBuilder) withObserved(objects ...map[string]any) *testNodeBuilder {
	b.observed = make([]*unstructured.Unstructured, len(objects))
	for i, obj := range objects {
		b.observed[i] = &unstructured.Unstructured{Object: obj}
	}
	return b
}

// withObservedUnstructured sets observed state from unstructured objects.
func (b *testNodeBuilder) withObservedUnstructured(objects ...*unstructured.Unstructured) *testNodeBuilder {
	b.observed = objects
	return b
}

// withDesired sets desired state.
func (b *testNodeBuilder) withDesired(objects ...*unstructured.Unstructured) *testNodeBuilder {
	b.desired = objects
	return b
}

// withIncludeWhen adds includeWhen expressions.
func (b *testNodeBuilder) withIncludeWhen(exprs ...string) *testNodeBuilder {
	for _, expr := range exprs {
		b.includeWhenExprs = append(b.includeWhenExprs, &expressionEvaluationState{
			Expression: mustCompileTestExpr(expr),
			Kind:       variable.ResourceVariableKindIncludeWhen,
		})
	}
	return b
}

// withResolvedIncludeWhen adds a pre-resolved includeWhen expression.
func (b *testNodeBuilder) withResolvedIncludeWhen(expr string, value bool) *testNodeBuilder {
	b.includeWhenExprs = append(b.includeWhenExprs, &expressionEvaluationState{
		Expression:    krocel.NewUncompiled(expr),
		Kind:          variable.ResourceVariableKindIncludeWhen,
		Resolved:      true,
		ResolvedValue: value,
	})
	return b
}

// withReadyWhen adds readyWhen expressions.
func (b *testNodeBuilder) withReadyWhen(exprs ...string) *testNodeBuilder {
	for _, expr := range exprs {
		b.readyWhenExprs = append(b.readyWhenExprs, &expressionEvaluationState{
			Expression: mustCompileTestExpr(expr),
			Kind:       variable.ResourceVariableKindReadyWhen,
		})
	}
	return b
}

// withForEach adds forEach expressions.
func (b *testNodeBuilder) withForEach(exprs ...string) *testNodeBuilder {
	for _, expr := range exprs {
		b.forEachExprs = append(b.forEachExprs, &expressionEvaluationState{
			Expression: mustCompileTestExpr(expr),
			Kind:       variable.ResourceVariableKindIteration,
		})
	}
	return b
}

// withTemplateExpr adds template expressions.
func (b *testNodeBuilder) withTemplateExpr(expr string, kind variable.ResourceVariableKind) *testNodeBuilder {
	b.templateExprs = append(b.templateExprs, &expressionEvaluationState{
		Expression: mustCompileTestExpr(expr),
		Kind:       kind,
	})
	return b
}

// withTemplateVar adds a template variable (always standalone).
func (b *testNodeBuilder) withTemplateVar(path string, exprs ...string) *testNodeBuilder {
	b.templateVars = append(b.templateVars, &variable.ResourceField{
		FieldDescriptor: variable.FieldDescriptor{
			Path:                 path,
			Expressions:          krocel.NewUncompiledSlice(exprs...),
			StandaloneExpression: true,
		},
	})
	return b
}

// withTemplate sets the template.
func (b *testNodeBuilder) withTemplate(obj map[string]any) *testNodeBuilder {
	b.template = &unstructured.Unstructured{Object: obj}
	return b
}

// build constructs the Node.
func (b *testNodeBuilder) build() *Node {
	node := &Node{
		Spec: &graph.Node{
			Meta: graph.NodeMeta{
				ID:   b.id,
				Type: b.nodeType,
			},
			Template: b.template,
		},
		deps:             b.deps,
		observed:         b.observed,
		desired:          b.desired,
		includeWhenExprs: b.includeWhenExprs,
		readyWhenExprs:   b.readyWhenExprs,
		forEachExprs:     b.forEachExprs,
		templateExprs:    b.templateExprs,
		templateVars:     b.templateVars,
	}
	return node
}
