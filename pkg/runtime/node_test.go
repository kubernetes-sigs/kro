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
	"errors"
	"fmt"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/kube-openapi/pkg/validation/spec"

	krocel "github.com/kubernetes-sigs/kro/pkg/cel"
	"github.com/kubernetes-sigs/kro/pkg/graph"
	"github.com/kubernetes-sigs/kro/pkg/graph/variable"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
)

const testMaxCollectionSize = 1000

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
	waitingForReadinessErr := func(t assert.TestingT, err error, i ...interface{}) bool {
		return assert.ErrorIs(t, err, ErrWaitingForReadiness)
	}
	tests := []struct {
		name    string
		node    *Node
		wantErr assert.ErrorAssertionFunc
	}{
		{
			name:    "single resource without readyWhen, not observed - not ready",
			node:    newTestNode("test", graph.NodeTypeResource).build(),
			wantErr: waitingForReadinessErr,
		},
		{
			name: "single resource without readyWhen, observed - ready",
			node: newTestNode("test", graph.NodeTypeResource).
				withObserved(map[string]any{"metadata": map[string]any{"name": "test"}}).build(),
			wantErr: assert.NoError,
		},
		{
			name:    "single resource with readyWhen, not observed - not ready",
			node:    newTestNode("test", graph.NodeTypeResource).withReadyWhen("test.status.ready").build(),
			wantErr: waitingForReadinessErr,
		},
		{
			name: "ignored nodes are always ready",
			node: newTestNode("child", graph.NodeTypeResource).
				withDep(newTestNode("ignoredParent", graph.NodeTypeResource).
					withDep(newTestNode("schema", graph.NodeTypeInstance).build()).
					withResolvedIncludeWhen("false", false).build()).
				withReadyWhen("test.status.ready").build(),
			wantErr: assert.NoError, // ignored, so ready
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.wantErr(t, tt.node.CheckReadiness())
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
		name        string
		node        *Node
		onlyFilter  []string
		wantKeys    []string
		notWantKeys []string
		wantLen     int
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
			wantKeys: []string{"buckets"},
			wantLen:  2,
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
		{
			name: "empty collection included in context as empty list",
			node: func() *Node {
				entries := newTestNode("entries", graph.NodeTypeCollection).build()
				entries.desired = []*unstructured.Unstructured{}
				entries.SetObserved([]*unstructured.Unstructured{})
				return newTestNode("summary", graph.NodeTypeResource).
					withDep(entries).
					build()
			}(),
			wantKeys: []string{"entries"},
			wantLen:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.node.buildContext(tt.onlyFilter...)

			for _, key := range tt.wantKeys {
				require.Contains(t, ctx, key)
			}
			for _, key := range tt.notWantKeys {
				require.NotContains(t, ctx, key)
			}
			for _, key := range tt.wantKeys {
				if col, ok := ctx[key].([]any); ok {
					assert.Len(t, col, tt.wantLen)
				}
			}
		})
	}
}

func TestNode_BuildContext_SchemaAwareConversion(t *testing.T) {
	// This test verifies that the schema-aware conversion correctly converts
	// Secret data fields from base64 strings to bytes, matching CEL compile-time types.
	// Before this fix, string(secret.data.clientId) would fail because CEL compiled
	// it as string(bytes) but received a string at runtime.
	secretSch := secretSchema()

	// Create a typed CEL environment where "secret" has the proper Secret schema.
	typedEnv, err := krocel.TypedEnvironment(map[string]*spec.Schema{
		"secret": secretSch,
	})
	require.NoError(t, err)

	// Compile: string(secret.data.clientId) — typed as string(bytes), the Kubernetes convention.
	celAST, issues := typedEnv.Compile("string(secret.data.clientId)")
	require.NoError(t, issues.Err())
	program, err := typedEnv.Program(celAST)
	require.NoError(t, err)

	expr := &krocel.Expression{
		Original:   "string(secret.data.clientId)",
		Program:    program,
		References: []string{"secret"},
	}

	// Build a secret dep node with the schema and base64-encoded observed data.
	secretDep := newTestNode("secret", graph.NodeTypeResource).
		withObserved(map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata":   map[string]any{"name": "test-secret", "namespace": "default"},
			"data": map[string]any{
				"clientId":     "bWF4",             // base64("max")
				"clientSecret": "bXVzdGVybWFubg==", // base64("mustermann")
			},
		}).
		withResourceSchema(secretSch).
		build()

	// Build the node that depends on the secret.
	node := newTestNode("deployment", graph.NodeTypeResource).
		withDep(secretDep).
		build()

	// Build context — this should wrap the secret with UnstructuredToVal.
	ctx := node.buildContext()
	require.Contains(t, ctx, "secret")

	// Evaluate the expression — this is the key assertion.
	// Before the fix, this would fail with "no such overload: string(string)".
	result, err := expr.Eval(ctx)
	require.NoError(t, err)
	assert.Equal(t, "max", result, "string(secret.data.clientId) should decode base64 to 'max'")

	// Also test the second field.
	celAST2, issues2 := typedEnv.Compile("string(secret.data.clientSecret)")
	require.NoError(t, issues2.Err())
	program2, err := typedEnv.Program(celAST2)
	require.NoError(t, err)

	expr2 := &krocel.Expression{
		Original:   "string(secret.data.clientSecret)",
		Program:    program2,
		References: []string{"secret"},
	}

	result2, err := expr2.Eval(ctx)
	require.NoError(t, err)
	assert.Equal(t, "mustermann", result2, "string(secret.data.clientSecret) should decode base64 to 'mustermann'")
}

func TestNode_BuildContext_PreserveUnknownFields(t *testing.T) {
	// This test verifies that fields inside x-kubernetes-preserve-unknown-fields objects
	// are accessible via CEL expressions. Before this fix, UnstructuredToVal would hide
	// these fields behind an opaque wrapper that rejected all field access.

	// Schema: spec.config is {type: "object", x-kubernetes-preserve-unknown-fields: true}
	// with no properties — simulating a CRD that allows arbitrary nested data.
	preserveUnknownSchema := &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"spec": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"config": {
							VendorExtensible: spec.VendorExtensible{
								Extensions: spec.Extensions{
									"x-kubernetes-preserve-unknown-fields": true,
								},
							},
							SchemaProps: spec.SchemaProps{
								Type: []string{"object"},
							},
						},
					},
				}},
				"status": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"additional": {SchemaProps: spec.SchemaProps{
							Type: []string{"object"},
							AdditionalProperties: &spec.SchemaOrBool{
								Schema: &spec.Schema{
									VendorExtensible: spec.VendorExtensible{
										Extensions: spec.Extensions{
											"x-kubernetes-preserve-unknown-fields": true,
										},
									},
								},
							},
						}},
					},
				}},
			},
		},
	}

	// Create a typed CEL env where "external" is declared as dyn.
	typedEnv, err := krocel.TypedEnvironment(map[string]*spec.Schema{
		"external": preserveUnknownSchema,
	})
	require.NoError(t, err)

	t.Run("preserve-unknown object field access", func(t *testing.T) {
		celAST, issues := typedEnv.Compile("external.spec.config.foo")
		require.NoError(t, issues.Err())
		program, err := typedEnv.Program(celAST)
		require.NoError(t, err)

		expr := &krocel.Expression{
			Original:   "external.spec.config.foo",
			Program:    program,
			References: []string{"external"},
		}

		dep := newTestNode("external", graph.NodeTypeExternal).
			withObserved(map[string]any{
				"spec": map[string]any{
					"config": map[string]any{
						"foo": "bar",
					},
				},
			}).
			withResourceSchema(preserveUnknownSchema).
			build()

		node := newTestNode("test", graph.NodeTypeResource).
			withDep(dep).
			build()

		ctx := node.buildContext()
		result, err := expr.Eval(ctx)
		require.NoError(t, err)
		assert.Equal(t, "bar", result)
	})

	t.Run("additionalProperties with preserve-unknown values", func(t *testing.T) {
		celAST, issues := typedEnv.Compile("external.status.additional.key")
		require.NoError(t, issues.Err())
		program, err := typedEnv.Program(celAST)
		require.NoError(t, err)

		expr := &krocel.Expression{
			Original:   "external.status.additional.key",
			Program:    program,
			References: []string{"external"},
		}

		dep := newTestNode("external", graph.NodeTypeExternal).
			withObserved(map[string]any{
				"status": map[string]any{
					"additional": map[string]any{
						"key": "value",
					},
				},
			}).
			withResourceSchema(preserveUnknownSchema).
			build()

		node := newTestNode("test", graph.NodeTypeResource).
			withDep(dep).
			build()

		ctx := node.buildContext()
		result, err := expr.Eval(ctx)
		require.NoError(t, err)
		assert.Equal(t, "value", result)
	})
}

func TestNode_BuildContext_NilSchemaFallback(t *testing.T) {
	// When no schema is provided, buildContext should pass raw objects (backward compatible).
	dep := newTestNode("configmap", graph.NodeTypeResource).
		withObserved(map[string]any{
			"data": map[string]any{"key": "value"},
		}).
		build()

	node := newTestNode("test", graph.NodeTypeResource).
		withDep(dep).
		build()

	ctx := node.buildContext()
	require.Contains(t, ctx, "configmap")

	// Without schema, the value should be a raw map.
	_, isMap := ctx["configmap"].(map[string]any)
	assert.True(t, isMap, "without schema, context value should be a raw map")
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

func TestNode_GetDesired_NamespaceNormalization(t *testing.T) {
	// instanceNode returns a schema/instance node observed in the given namespace.
	instanceNode := func(ns string) *Node {
		return newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
			withObserved(map[string]any{
				"metadata": map[string]any{"name": "my-instance", "namespace": ns},
			}).build()
	}

	tests := []struct {
		name          string
		node          *Node
		wantNamespace string // expected namespace on the first result object
	}{
		// ── Namespaced resources: namespace IS injected ──────────────────
		{
			name: "namespaced resource without namespace gets instance namespace",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("deploy", graph.NodeTypeResource).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "apps/v1", "kind": "Deployment",
						"metadata": map[string]any{"name": "web"},
					}).build()
			}(),
			wantNamespace: "tenant-ns",
		},
		{
			name: "namespaced resource with explicit namespace keeps it",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("deploy", graph.NodeTypeResource).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "apps/v1", "kind": "Deployment",
						"metadata": map[string]any{"name": "web", "namespace": "other-ns"},
					}).build()
			}(),
			wantNamespace: "other-ns",
		},

		// ── Namespaced external: namespace IS injected ───────────────────
		{
			name: "namespaced external without namespace gets instance namespace",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("ext", graph.NodeTypeExternal).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "shared-config"},
					}).build()
			}(),
			wantNamespace: "tenant-ns",
		},
		{
			name: "namespaced external with explicit namespace keeps it",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("ext", graph.NodeTypeExternal).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{"name": "shared-config", "namespace": "specific-ns"},
					}).build()
			}(),
			wantNamespace: "specific-ns",
		},

		// ── Cluster-scoped resource: no namespace injected ──────────────
		{
			name: "cluster-scoped resource stays without namespace",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("crb", graph.NodeTypeResource).
					// NOT withNamespaced()
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "ClusterRoleBinding",
						"metadata": map[string]any{"name": "my-binding"},
					}).build()
			}(),
			wantNamespace: "",
		},

		// ── Instance node: never normalized ─────────────────────────────
		{
			name: "instance node is never namespace-normalized",
			node: newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
				withNamespaced().
				withTemplate(map[string]any{
					"apiVersion": "v1", "kind": "Instance",
					"metadata": map[string]any{"name": "my-instance"},
				}).build(),
			wantNamespace: "",
		},

		// ── External collection: NEVER normalized (the fix) ─────────────
		{
			name: "namespaced external collection without namespace stays empty (list all)",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("extcol", graph.NodeTypeExternalCollection).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "Pod",
						"metadata": map[string]any{"name": ""},
					}).build()
			}(),
			wantNamespace: "", // must NOT be "tenant-ns"
		},
		{
			name: "namespaced external collection with explicit namespace keeps it",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("extcol", graph.NodeTypeExternalCollection).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "Pod",
						"metadata": map[string]any{"name": "", "namespace": "specific-ns"},
					}).build()
			}(),
			wantNamespace: "specific-ns",
		},
		{
			name: "cluster-scoped external collection stays without namespace",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("extcol", graph.NodeTypeExternalCollection).
					// NOT withNamespaced()
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "Node",
						"metadata": map[string]any{"name": ""},
					}).build()
			}(),
			wantNamespace: "",
		},

		// ── CEL-resolved namespace scenarios ────────────────────────────
		{
			name: "namespaced resource with CEL-resolved namespace keeps resolved value",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("cm", graph.NodeTypeResource).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "ConfigMap",
						"metadata": map[string]any{
							"name":      "test",
							"namespace": "${schema.metadata.namespace}",
						},
					}).
					withTemplateVar("metadata.namespace", "schema.metadata.namespace").
					withTemplateExpr("schema.metadata.namespace", variable.ResourceVariableKindStatic).
					build()
			}(),
			wantNamespace: "tenant-ns", // resolved via CEL, not injected by normalizeNamespaces
		},
		{
			name: "namespaced external collection with CEL namespace preserves resolved value",
			node: func() *Node {
				inst := instanceNode("tenant-ns")
				return newTestNode("extcol", graph.NodeTypeExternalCollection).
					withNamespaced().
					withDep(inst).
					withTemplate(map[string]any{
						"apiVersion": "v1", "kind": "Pod",
						"metadata": map[string]any{
							"name":      "",
							"namespace": "${schema.metadata.namespace}",
						},
					}).
					withTemplateVar("metadata.namespace", "schema.metadata.namespace").
					withTemplateExpr("schema.metadata.namespace", variable.ResourceVariableKindStatic).
					build()
			}(),
			wantNamespace: "tenant-ns", // resolved via CEL, normalizeNamespaces skipped but no-op anyway
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.node.GetDesired()
			require.NoError(t, err)
			require.Len(t, result, 1, "expected exactly one desired object")
			assert.Equal(t, tt.wantNamespace, result[0].GetNamespace(),
				"unexpected namespace on desired object")
		})
	}
}

// TestNode_GetDesired_NamespaceNormalization_Regression verifies that the
// namespace normalization bug cannot silently regress. If someone removes the
// ExternalCollection guard from the normalizeNamespaces condition, this test
// will catch it by asserting that an empty-namespace external collection object
// does NOT get the instance namespace injected.
func TestNode_GetDesired_NamespaceNormalization_Regression(t *testing.T) {
	inst := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
		withObserved(map[string]any{
			"metadata": map[string]any{"name": "inst", "namespace": "default"},
		}).build()

	// External collection with empty namespace — the "list all namespaces" pattern.
	extCol := newTestNode("pods", graph.NodeTypeExternalCollection).
		withNamespaced().
		withDep(inst).
		withTemplate(map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": ""},
		}).build()

	result, err := extCol.GetDesired()
	require.NoError(t, err)
	require.Len(t, result, 1)

	// This is the regression assertion: namespace must remain empty.
	assert.Empty(t, result[0].GetNamespace(),
		"BUG: normalizeNamespaces injected instance namespace into external collection; "+
			"empty namespace means 'list all namespaces' and must not be overwritten")
}

func TestNode_IsReady_Collection(t *testing.T) {
	waitingForReadinessErr := func(t assert.TestingT, err error, i ...interface{}) bool {
		return assert.ErrorIs(t, err, ErrWaitingForReadiness)
	}

	tests := []struct {
		name    string
		node    *Node
		wantErr assert.ErrorAssertionFunc
	}{
		{
			name: "collection with nil desired is not ready",
			node: newTestNode("buckets", graph.NodeTypeCollection).
				withReadyWhen("each.status.ready == true").build(),
			wantErr: waitingForReadinessErr,
		},
		{
			name: "collection with resolved empty desired is ready",
			node: func() *Node {
				n := newTestNode("buckets", graph.NodeTypeCollection).
					withReadyWhen("each.status.ready == true").build()
				n.desired = []*unstructured.Unstructured{}
				return n
			}(),
			wantErr: assert.NoError,
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
			wantErr: waitingForReadinessErr,
		},
		{
			name:    "collection without readyWhen, nil desired - not ready",
			node:    newTestNode("items", graph.NodeTypeCollection).build(),
			wantErr: waitingForReadinessErr,
		},
		{
			name: "collection without readyWhen, empty desired - ready",
			node: func() *Node {
				n := newTestNode("items", graph.NodeTypeCollection).build()
				n.desired = []*unstructured.Unstructured{}
				return n
			}(),
			wantErr: assert.NoError,
		},
		{
			name: "collection without readyWhen, observed < desired - not ready",
			node: newTestNode("items", graph.NodeTypeCollection).
				withDesired(
					newUnstructured("v1", "ConfigMap", "ns", "cm-1"),
					newUnstructured("v1", "ConfigMap", "ns", "cm-2"),
				).
				withObservedUnstructured(newUnstructured("v1", "ConfigMap", "ns", "cm-1")).
				build(),
			wantErr: waitingForReadinessErr,
		},
		{
			name: "collection without readyWhen, observed >= desired - ready",
			node: newTestNode("items", graph.NodeTypeCollection).
				withDesired(
					newUnstructured("v1", "ConfigMap", "ns", "cm-1"),
					newUnstructured("v1", "ConfigMap", "ns", "cm-2"),
				).
				withObservedUnstructured(
					newUnstructured("v1", "ConfigMap", "ns", "cm-1"),
					newUnstructured("v1", "ConfigMap", "ns", "cm-2"),
				).
				build(),
			wantErr: assert.NoError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.wantErr(t, tt.node.CheckReadiness())
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
			name: "resource-backed includeWhen evaluates to true",
			node: func() *Node {
				vpc := newTestNode("vpc", graph.NodeTypeResource).
					withObserved(map[string]any{"status": map[string]any{"state": "available"}}).build()
				return newTestNode("subnet", graph.NodeTypeResource).
					withDep(vpc).
					withIncludeWhenRefs("vpc.status.state == 'available'", "vpc").build()
			},
			wantIgnored: false,
		},
		{
			name: "resource-backed includeWhen evaluates to false",
			node: func() *Node {
				vpc := newTestNode("vpc", graph.NodeTypeResource).
					withObserved(map[string]any{"status": map[string]any{"state": "pending"}}).build()
				return newTestNode("subnet", graph.NodeTypeResource).
					withDep(vpc).
					withIncludeWhenRefs("vpc.status.state == 'available'", "vpc").build()
			},
			wantIgnored: true,
		},
		{
			name: "resource-backed includeWhen waits for dependency readiness",
			node: func() *Node {
				vpc := newTestNode("vpc", graph.NodeTypeResource).
					withObserved(map[string]any{"status": map[string]any{}}).
					withReadyWhen("vpc.status.state == 'available'").build()
				return newTestNode("subnet", graph.NodeTypeResource).
					withDep(vpc).
					withIncludeWhenRefs("vpc.status.state == 'available'", "vpc").build()
			},
			wantErr: true,
			errIs:   ErrDataPending,
		},
		{
			name: "resource-backed includeWhen missing field is pending",
			node: func() *Node {
				vpc := newTestNode("vpc", graph.NodeTypeResource).
					withObserved(map[string]any{"metadata": map[string]any{"name": "vpc"}}).build()
				return newTestNode("subnet", graph.NodeTypeResource).
					withDep(vpc).
					withIncludeWhenRefs("vpc.status.state == 'available'", "vpc").build()
			},
			wantErr: true,
			errIs:   ErrDataPending,
		},
		{
			name: "resource-backed includeWhen false is contagious to downstream resources",
			node: func() *Node {
				source := newTestNode("source", graph.NodeTypeResource).
					withObserved(map[string]any{"data": map[string]any{"enabled": "false"}}).build()
				middle := newTestNode("middle", graph.NodeTypeResource).
					withDep(source).
					withIncludeWhenRefs("source.data.enabled == 'true'", "source").build()
				return newTestNode("child", graph.NodeTypeResource).
					withDep(middle).
					withTemplateExpr("middle.data.value", variable.ResourceVariableKindDynamic).
					build()
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
	waitingForReadinessErr := func(t assert.TestingT, err error, i ...interface{}) bool {
		return assert.ErrorIs(t, err, ErrWaitingForReadiness)
	}
	tests := []struct {
		name    string
		node    func() *Node
		wantErr assert.ErrorAssertionFunc
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
			wantErr: assert.NoError,
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
			wantErr: waitingForReadinessErr,
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
			wantErr: waitingForReadinessErr,
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
			wantErr: func(t assert.TestingT, err error, i ...interface{}) bool {
				return assert.ErrorContains(t, err, "division by zero")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.wantErr(t, tt.node().CheckReadiness())
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
			wantErr: false,
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
			wantErr: false,
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
			wantErr: true,
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
			wantErr: true,
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
			wantErr: true,
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
			wantErr: true,
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
			err := tt.node().CheckReadiness()
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errContain != "" {
					assert.Contains(t, err.Error(), tt.errContain)
				}
				return
			}
			assert.NoError(t, err)
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
		{
			name: "collection exceeds max size",
			node: func() *Node {
				var items []any
				for i := 0; i < testMaxCollectionSize+1; i++ {
					items = append(items, fmt.Sprintf("item-%d", i))
				}

				schema := newTestNode("schema", graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"items": items,
						},
					}).build()
				n := newTestNode("resources", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.items").build()
				n.Spec.ForEach = []graph.ForEachDimension{
					{Name: "item", Expression: krocel.NewUncompiled("schema.spec.items")},
				}
				return n
			}(),
			wantErr: true,
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
		"source", "middle",
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
	namespaced       bool
	deps             map[string]*Node
	observed         []*unstructured.Unstructured
	desired          []*unstructured.Unstructured
	includeWhenExprs []*expressionEvaluationState
	readyWhenExprs   []*expressionEvaluationState
	forEachExprs     []*expressionEvaluationState
	templateExprs    []*expressionEvaluationState
	templateVars     []*variable.ResourceField
	template         *unstructured.Unstructured
	resourceSchema   *spec.Schema
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

// withIncludeWhenRefs adds includeWhen expressions with explicit identifier references.
func (b *testNodeBuilder) withIncludeWhenRefs(expr string, refs ...string) *testNodeBuilder {
	compiled := mustCompileTestExpr(expr)
	compiled.References = append(compiled.References, refs...)
	b.includeWhenExprs = append(b.includeWhenExprs, &expressionEvaluationState{
		Expression: compiled,
		Kind:       variable.ResourceVariableKindIncludeWhen,
	})
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

// withTemplateVar adds a template variable.
func (b *testNodeBuilder) withTemplateVar(path string, expr string) *testNodeBuilder {
	b.templateVars = append(b.templateVars, &variable.ResourceField{
		FieldDescriptor: variable.FieldDescriptor{
			Path:       path,
			Expression: krocel.NewUncompiled(expr),
		},
	})
	return b
}

func (b *testNodeBuilder) withTemplateVarRefs(path string, expr string, refs ...string) *testNodeBuilder {
	compiled := mustCompileTestExpr(expr)
	compiled.References = refs
	b.templateExprs = append(b.templateExprs, &expressionEvaluationState{
		Expression: compiled,
		Kind:       variable.ResourceVariableKindStatic,
	})
	b.templateVars = append(b.templateVars, &variable.ResourceField{
		FieldDescriptor: variable.FieldDescriptor{
			Path:       path,
			Expression: compiled,
		},
	})
	return b
}

// withTemplate sets the template.
func (b *testNodeBuilder) withTemplate(obj map[string]any) *testNodeBuilder {
	b.template = &unstructured.Unstructured{Object: obj}
	return b
}

// withNamespaced marks the node as namespace-scoped.
func (b *testNodeBuilder) withNamespaced() *testNodeBuilder {
	b.namespaced = true
	return b
}

// withResourceSchema sets the OpenAPI schema for this node's resource.
func (b *testNodeBuilder) withResourceSchema(schema *spec.Schema) *testNodeBuilder {
	b.resourceSchema = schema
	return b
}

// build constructs the Node.
func (b *testNodeBuilder) build() *Node {
	node := &Node{
		Spec: &graph.Node{
			Meta: graph.NodeMeta{
				ID:         b.id,
				Type:       b.nodeType,
				Namespaced: b.namespaced,
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
		rgdConfig: graph.RGDConfig{
			MaxCollectionSize: testMaxCollectionSize,
		},
		resourceSchema: b.resourceSchema,
	}
	return node
}

// secretSchema returns the OpenAPI schema for v1/Secret with format:byte data values.
func secretSchema() *spec.Schema {
	return &spec.Schema{
		SchemaProps: spec.SchemaProps{
			Type: []string{"object"},
			Properties: map[string]spec.Schema{
				"apiVersion": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"kind":       {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
				"metadata": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					Properties: map[string]spec.Schema{
						"name":      {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
						"namespace": {SchemaProps: spec.SchemaProps{Type: []string{"string"}}},
					},
				}},
				"data": {SchemaProps: spec.SchemaProps{
					Type: []string{"object"},
					AdditionalProperties: &spec.SchemaOrBool{
						Schema: &spec.Schema{
							SchemaProps: spec.SchemaProps{
								Type:   []string{"string"},
								Format: "byte",
							},
						},
					},
				}},
			},
		},
	}
}

func TestErrorHelpers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(t *testing.T)
	}{
		{
			name: "IsDataPending matches wrapped sentinel",
			fn: func(t *testing.T) {
				err := errors.New("other")
				assert.False(t, IsDataPending(err))
				assert.True(t, IsDataPending(errors.Join(err, ErrDataPending)))
			},
		},
		{
			name: "isCELDataPending matches known retryable CEL errors",
			fn: func(t *testing.T) {
				assert.False(t, isCELDataPending(nil))

				for _, msg := range []string{
					"eval failed: no such key: status",
					"eval failed: no such field: ready",
					"eval failed: no such attribute: schema",
					"eval failed: index out of bounds: 1",
				} {
					assert.True(t, isCELDataPending(errors.New(msg)), msg)
				}

				assert.False(t, isCELDataPending(errors.New("division by zero")))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, tt.fn)
	}
}

func TestEvalExprAny(t *testing.T) {
	tests := []struct {
		name           string
		expr           *expressionEvaluationState
		ctx            map[string]any
		want           any
		wantErrContain string
		wantResolved   bool
	}{
		{
			name: "returns cached value without evaluation",
			expr: &expressionEvaluationState{
				Expression:    krocel.NewUncompiled("schema.spec.name"),
				Kind:          variable.ResourceVariableKindStatic,
				Resolved:      true,
				ResolvedValue: "cached",
			},
			ctx:          map[string]any{"schema": map[string]any{"spec": map[string]any{"name": "fresh"}}},
			want:         "cached",
			wantResolved: true,
		},
		{
			name: "evaluates and caches uncached expression",
			expr: func() *expressionEvaluationState {
				expr := &expressionEvaluationState{
					Expression: mustCompileTestExpr("schema.spec.name"),
					Kind:       variable.ResourceVariableKindStatic,
				}
				expr.Expression.References = []string{"schema"}
				return expr
			}(),
			ctx: map[string]any{
				"schema": map[string]any{"spec": map[string]any{"name": "demo"}},
				"other":  "ignored",
			},
			want:         "demo",
			wantResolved: true,
		},
		{
			name: "propagates evaluation errors",
			expr: func() *expressionEvaluationState {
				expr := &expressionEvaluationState{
					Expression: mustCompileTestExpr("schema.spec.total / schema.spec.divisor"),
					Kind:       variable.ResourceVariableKindStatic,
				}
				expr.Expression.References = []string{"schema"}
				return expr
			}(),
			ctx: map[string]any{
				"schema": map[string]any{"spec": map[string]any{"total": int64(10), "divisor": int64(0)}},
			},
			wantErrContain: "division by zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := evalExprAny(tt.expr, tt.ctx)
			if tt.wantErrContain != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContain)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, val)
			assert.Equal(t, tt.wantResolved, tt.expr.Resolved)
			assert.Equal(t, tt.want, tt.expr.ResolvedValue)
		})
	}
}

func TestEvalBoolExpr(t *testing.T) {
	tests := []struct {
		name           string
		expr           *expressionEvaluationState
		ctx            map[string]any
		want           bool
		wantErrContain string
		wantResolved   bool
	}{
		{
			name: "returns cached bool",
			expr: &expressionEvaluationState{
				Expression:    krocel.NewUncompiled("schema.spec.enabled"),
				Kind:          variable.ResourceVariableKindIncludeWhen,
				Resolved:      true,
				ResolvedValue: true,
			},
			ctx:          map[string]any{},
			want:         true,
			wantResolved: true,
		},
		{
			name: "returns false for null values and caches the result",
			expr: &expressionEvaluationState{
				Expression: mustCompileTestExpr("null"),
				Kind:       variable.ResourceVariableKindIncludeWhen,
			},
			ctx:          map[string]any{},
			want:         false,
			wantResolved: true,
		},
		{
			name: "errors when expression is not bool",
			expr: &expressionEvaluationState{
				Expression: mustCompileTestExpr("1"),
				Kind:       variable.ResourceVariableKindIncludeWhen,
			},
			ctx:            map[string]any{},
			wantErrContain: "did not return bool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := evalBoolExpr(tt.expr, tt.ctx)
			if tt.wantErrContain != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContain)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, val)
			assert.Equal(t, tt.wantResolved, tt.expr.Resolved)
		})
	}
}

func TestEvalListExpr(t *testing.T) {
	tests := []struct {
		name           string
		expr           *expressionEvaluationState
		ctx            map[string]any
		want           []any
		wantErrContain string
		wantResolved   bool
	}{
		{
			name: "returns cached list",
			expr: &expressionEvaluationState{
				Expression:    krocel.NewUncompiled("schema.spec.items"),
				Kind:          variable.ResourceVariableKindIteration,
				Resolved:      true,
				ResolvedValue: []any{"cached"},
			},
			ctx:          map[string]any{},
			want:         []any{"cached"},
			wantResolved: true,
		},
		{
			name: "returns list and caches value",
			expr: func() *expressionEvaluationState {
				expr := &expressionEvaluationState{
					Expression: mustCompileTestExpr("schema.spec.items"),
					Kind:       variable.ResourceVariableKindIteration,
				}
				expr.Expression.References = []string{"schema"}
				return expr
			}(),
			ctx: map[string]any{
				"schema": map[string]any{"spec": map[string]any{"items": []any{"a", "b"}}},
			},
			want:         []any{"a", "b"},
			wantResolved: true,
		},
		{
			name: "errors on null result",
			expr: &expressionEvaluationState{
				Expression: mustCompileTestExpr("null"),
				Kind:       variable.ResourceVariableKindIteration,
			},
			ctx:            map[string]any{},
			wantErrContain: "expected list",
		},
		{
			name: "errors on non-list result",
			expr: &expressionEvaluationState{
				Expression: mustCompileTestExpr("1"),
				Kind:       variable.ResourceVariableKindIteration,
			},
			ctx:            map[string]any{},
			wantErrContain: "did not return a list",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			val, err := evalListExpr(tt.expr, tt.ctx)
			if tt.wantErrContain != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrContain)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, val)
			assert.Equal(t, tt.wantResolved, tt.expr.Resolved)
		})
	}
}

func TestFilterContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  map[string]any
		refs []string
		want map[string]any
		same bool
	}{
		{
			name: "returns original context when refs are empty",
			ctx:  map[string]any{"a": 1, "b": 2},
			want: map[string]any{"a": 1, "b": 2},
			same: true,
		},
		{
			name: "filters to referenced keys",
			ctx:  map[string]any{"a": 1, "b": 2},
			refs: []string{"b", "missing"},
			want: map[string]any{"b": 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterContext(tt.ctx, tt.refs)
			assert.Equal(t, tt.want, got)
			if tt.same {
				got["c"] = 3
				assert.Equal(t, 3, tt.ctx["c"])
			}
		})
	}
}

func TestNormalizeNamespaces(t *testing.T) {
	makeNode := func(instanceNS string) *Node {
		inst := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
			withObservedUnstructured(newUnstructured("v1", "Instance", instanceNS, "my-inst")).
			build()
		return newTestNode("child", graph.NodeTypeResource).
			withDep(inst).
			build()
	}

	tests := []struct {
		name           string
		nodeNamespaced bool
		instanceNS     string
		objs           []*unstructured.Unstructured
		wantNamespaces []string
		wantErr        string
	}{
		{
			name:           "inherits instance namespace",
			nodeNamespaced: true,
			instanceNS:     "tenant-a",
			objs: []*unstructured.Unstructured{
				newUnstructured("v1", "ConfigMap", "", "generated"),
				newUnstructured("v1", "ConfigMap", "explicit", "existing"),
			},
			wantNamespaces: []string{"tenant-a", "explicit"},
		},
		{
			name:           "cluster-scoped instance rejects empty namespace on namespaced child",
			nodeNamespaced: true,
			instanceNS:     "",
			objs: []*unstructured.Unstructured{
				newUnstructured("v1", "ConfigMap", "", "no-ns"),
				newUnstructured("v1", "ConfigMap", "explicit", "has-ns"),
			},
			wantNamespaces: []string{"", "explicit"},
			wantErr:        "must resolve metadata.namespace",
		},
		{
			name:           "cluster-scoped instance allows explicit namespace on namespaced child",
			nodeNamespaced: true,
			instanceNS:     "",
			objs: []*unstructured.Unstructured{
				newUnstructured("v1", "ConfigMap", "target-ns", "explicit"),
			},
			wantNamespaces: []string{"target-ns"},
		},
		{
			name:           "cluster-scoped child is a no-op",
			nodeNamespaced: false,
			instanceNS:     "tenant-a",
			objs: []*unstructured.Unstructured{
				newUnstructured("v1", "ClusterRole", "", "cr"),
			},
			wantNamespaces: []string{""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := makeNode(tt.instanceNS)
			node.Spec.Meta.Namespaced = tt.nodeNamespaced
			err := node.normalizeNamespaces(tt.objs)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			for i, obj := range tt.objs {
				assert.Equal(t, tt.wantNamespaces[i], obj.GetNamespace())
			}
		})
	}
}

func TestNode_TemplateVarsForPaths(t *testing.T) {
	node := newTestNode("test", graph.NodeTypeResource).
		withTemplateVar("metadata.name", "schema.spec.name").
		withTemplateVar("metadata.namespace", "schema.metadata.namespace").
		withTemplateVar("data.value", "vpc.status.id").
		build()

	tests := []struct {
		name      string
		paths     []string
		wantPaths []string
	}{
		{
			name:      "nil paths returns all template vars",
			paths:     nil,
			wantPaths: []string{"metadata.name", "metadata.namespace", "data.value"},
		},
		{
			name:      "filters to identity paths",
			paths:     identityPaths[graph.NodeTypeResource],
			wantPaths: []string{"metadata.name", "metadata.namespace"},
		},
		{
			name:      "returns empty slice for unknown paths",
			paths:     []string{"status.ready"},
			wantPaths: nil,
		},
		{
			name:      "prefix match includes nested paths",
			paths:     []string{"metadata.namespace", "metadata.selector"},
			wantPaths: []string{"metadata.namespace"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := node.templateVarsForPaths(tt.paths)
			require.Len(t, result, len(tt.wantPaths))
			for i, path := range tt.wantPaths {
				assert.Equal(t, path, result[i].Path)
			}
		})
	}
}

func TestNode_NeededDeps(t *testing.T) {
	schemaExpr := krocel.NewUncompiled("schema.spec.name")
	schemaExpr.References = []string{"schema"}
	vpcExpr := krocel.NewUncompiled("vpc.status.id")
	vpcExpr.References = []string{"vpc"}
	iterExpr := krocel.NewUncompiled("region")
	iterExpr.References = []string{"region"}

	node := newTestNode("test", graph.NodeTypeResource).build()
	node.templateExprs = []*expressionEvaluationState{
		{Expression: schemaExpr, Kind: variable.ResourceVariableKindStatic},
		{Expression: vpcExpr, Kind: variable.ResourceVariableKindDynamic},
		{Expression: iterExpr, Kind: variable.ResourceVariableKindIteration},
	}

	tests := []struct {
		name     string
		exprs    map[string]struct{}
		wantDeps []string
	}{
		{
			name:     "nil filter includes all non-iteration deps",
			exprs:    nil,
			wantDeps: []string{"schema", "vpc"},
		},
		{
			name: "explicit filter narrows dependencies",
			exprs: map[string]struct{}{
				"vpc.status.id": {},
			},
			wantDeps: []string{"vpc"},
		},
		{
			name:     "empty filter returns no dependencies",
			exprs:    map[string]struct{}{},
			wantDeps: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.ElementsMatch(t, tt.wantDeps, node.neededDeps(tt.exprs))
		})
	}
}

func TestNode_GetDesiredIdentity(t *testing.T) {
	tests := []struct {
		name        string
		node        func() *Node
		expectPanic bool
		validate    func(t *testing.T, result []*unstructured.Unstructured, err error)
	}{
		{
			name: "resource resolves only identity fields and normalizes namespace",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"metadata": map[string]any{"namespace": "tenant-a"},
						"spec":     map[string]any{"name": "demo"},
					}).build()
				subnet := newTestNode("subnet", graph.NodeTypeResource).build()

				node := newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withDep(subnet).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${schema.spec.name}"},
						"data":       map[string]any{"subnet": "${subnet.status.id}"},
					}).
					withTemplateVar("metadata.name", "schema.spec.name").
					withTemplateVar("data.subnet", "subnet.status.id").
					withTemplateExpr("schema.spec.name", variable.ResourceVariableKindStatic).
					withTemplateExpr("subnet.status.id", variable.ResourceVariableKindDynamic).
					build()
				node.Spec.Meta.Namespaced = true
				return node
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Equal(t, "demo", result[0].GetName())
				assert.Equal(t, "tenant-a", result[0].GetNamespace())
			},
		},
		{
			name: "collection resolves identities without collection index labels",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"metadata": map[string]any{"namespace": "tenant-a"},
						"spec":     map[string]any{"regions": []any{"east", "west"}},
					}).build()

				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).
					withTemplateVar("metadata.name", "region").
					withTemplateExpr("region", variable.ResourceVariableKindIteration).
					build()
				node.Spec.Meta.Namespaced = true
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				return node
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 2)
				assert.Equal(t, []string{"east", "west"}, []string{result[0].GetName(), result[1].GetName()})
				assert.Equal(t, []string{"tenant-a", "tenant-a"}, []string{result[0].GetNamespace(), result[1].GetNamespace()})
				assert.Empty(t, result[0].GetLabels()[metadata.CollectionIndexLabel])
				assert.Empty(t, result[1].GetLabels()[metadata.CollectionIndexLabel])
			},
		},
		{
			name: "external collection resolves static selector as identity",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "Pod",
						"metadata": map[string]any{
							"namespace": "default",
							"selector":  map[string]any{"app": "foo"},
						},
					}).
					build()
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Equal(t, "default", result[0].GetNamespace())
				sel, _, _ := unstructured.NestedStringMap(result[0].Object, "metadata", "selector")
				assert.Equal(t, map[string]string{"app": "foo"}, sel)
			},
		},
		{
			name: "external collection resolves CEL selector as identity",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"app": "bar"}}).
					build()
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "Pod",
						"metadata": map[string]any{
							"namespace": "default",
							"selector":  map[string]any{"app": "${schema.spec.app}"},
						},
					}).
					withTemplateVarRefs("metadata.selector.app", "schema.spec.app", "schema").
					withDep(schema).
					build()
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				sel, _, _ := unstructured.NestedStringMap(result[0].Object, "metadata", "selector")
				assert.Equal(t, map[string]string{"app": "bar"}, sel)
			},
		},
		{
			name: "external collection returns DataPending when selector dep is unobserved",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).build() // no observed
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "Pod",
						"metadata": map[string]any{
							"namespace": "default",
							"selector":  map[string]any{"app": "${schema.spec.app}"},
						},
					}).
					withTemplateVarRefs("metadata.selector.app", "schema.spec.app", "schema").
					withDep(schema).
					build()
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.Error(t, err)
				assert.True(t, IsDataPending(err))
				assert.Nil(t, result)
			},
		},
		{
			name: "external resource resolves identity like a single resource",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternal).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "Secret",
						"metadata":   map[string]any{"name": "external-name"},
					}).
					build()
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Equal(t, "external-name", result[0].GetName())
			},
		},
		{
			name: "resource identity errors are propagated",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"total": int64(10), "divisor": int64(0)},
					}).build()

				node := newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${schema.spec.total / schema.spec.divisor}"},
					}).
					withTemplateVar("metadata.name", "schema.spec.total / schema.spec.divisor").
					withTemplateExpr("schema.spec.total / schema.spec.divisor", variable.ResourceVariableKindStatic).
					build()
				node.templateExprs[0].Expression.References = []string{"schema"}
				return node
			},
			validate: func(t *testing.T, _ []*unstructured.Unstructured, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "division by zero")
			},
		},
		{
			name: "collection identity errors are propagated",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{}}).
					build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).
					withTemplateVar("metadata.name", "region").
					withTemplateExpr("region", variable.ResourceVariableKindIteration).
					build()
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				node.forEachExprs[0].Expression.References = []string{"schema"}
				return node
			},
			validate: func(t *testing.T, _ []*unstructured.Unstructured, err error) {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrDataPending)
			},
		},
		{
			name: "instance nodes panic when asked for desired identity",
			node: func() *Node {
				return newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).build()
			},
			expectPanic: true,
		},
		{
			name: "unknown node types panic",
			node: func() *Node {
				return newTestNode("mystery", graph.NodeType(99)).build()
			},
			expectPanic: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := tt.node()
			if tt.expectPanic {
				assert.Panics(t, func() {
					_, _ = node.GetDesiredIdentity()
				})
				return
			}

			result, err := node.GetDesiredIdentity()
			tt.validate(t, result, err)
		})
	}
}

func TestNode_DeleteTargets(t *testing.T) {
	tests := []struct {
		name        string
		node        func() *Node
		expectPanic bool
		validate    func(t *testing.T, node *Node, result []*unstructured.Unstructured, err error)
	}{
		{
			name: "resource deletes currently observed object set",
			node: func() *Node {
				return newTestNode("configmap", graph.NodeTypeResource).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "desired"},
					}).
					withObservedUnstructured(newUnstructured("v1", "ConfigMap", "ns", "observed")).
					build()
			},
			validate: func(t *testing.T, node *Node, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Same(t, node.observed[0], result[0])
			},
		},
		{
			name: "collection deletes only observed objects that still match desired identities in desired order",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"metadata": map[string]any{"namespace": "tenant-a"},
						"spec":     map[string]any{"regions": []any{"east", "west"}},
					}).build()

				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).
					withTemplateVar("metadata.name", "region").
					withTemplateExpr("region", variable.ResourceVariableKindIteration).
					withObservedUnstructured(
						newUnstructured("v1", "ConfigMap", "tenant-a", "west"),
						newUnstructured("v1", "ConfigMap", "tenant-a", "orphan"),
						newUnstructured("v1", "ConfigMap", "tenant-a", "east"),
					).
					build()
				node.Spec.Meta.Namespaced = true
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				return node
			},
			validate: func(t *testing.T, _ *Node, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 2)
				assert.Equal(t, []string{"east", "west"}, []string{result[0].GetName(), result[1].GetName()})
			},
		},
		{
			name: "external nodes panic because they are never delete targets",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternal).build()
			},
			expectPanic: true,
		},
		{
			name: "external collections panic because they are never delete targets",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternalCollection).build()
			},
			expectPanic: true,
		},
		{
			name: "instance nodes panic because they are never delete targets",
			node: func() *Node {
				return newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).build()
			},
			expectPanic: true,
		},
		{
			name: "identity resolution errors are surfaced",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{}}).
					build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).
					withTemplateVar("metadata.name", "region").
					withTemplateExpr("region", variable.ResourceVariableKindIteration).
					build()
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				node.forEachExprs[0].Expression.References = []string{"schema"}
				return node
			},
			validate: func(t *testing.T, _ *Node, _ []*unstructured.Unstructured, err error) {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrDataPending)
			},
		},
		{
			name: "unknown node types panic",
			node: func() *Node {
				return newTestNode("mystery", graph.NodeType(99)).build()
			},
			expectPanic: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := tt.node()
			if tt.expectPanic {
				assert.Panics(t, func() {
					_, _ = node.DeleteTargets()
				})
				return
			}

			result, err := node.DeleteTargets()
			tt.validate(t, node, result, err)
		})
	}
}

func TestNode_GetDesired(t *testing.T) {
	tests := []struct {
		name        string
		node        func() *Node
		expectPanic bool
		validate    func(t *testing.T, result []*unstructured.Unstructured, err error)
	}{
		{
			name: "resource desired is namespace-normalized",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"metadata": map[string]any{"namespace": "tenant-a"},
						"spec":     map[string]any{"name": "demo"},
					}).build()

				node := newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${schema.spec.name}"},
					}).
					withTemplateVar("metadata.name", "schema.spec.name").
					withTemplateExpr("schema.spec.name", variable.ResourceVariableKindStatic).
					build()
				node.Spec.Meta.Namespaced = true
				return node
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Equal(t, "demo", result[0].GetName())
				assert.Equal(t, "tenant-a", result[0].GetNamespace())
			},
		},
		{
			name: "collection desired gets index labels and namespaces",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"metadata": map[string]any{"namespace": "tenant-a"},
						"spec":     map[string]any{"regions": []any{"east", "west"}},
					}).build()

				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).
					withTemplateVar("metadata.name", "region").
					withTemplateExpr("region", variable.ResourceVariableKindIteration).
					build()
				node.Spec.Meta.Namespaced = true
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				return node
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 2)
				assert.Equal(t, "0", result[0].GetLabels()[metadata.CollectionIndexLabel])
				assert.Equal(t, "1", result[1].GetLabels()[metadata.CollectionIndexLabel])
				assert.Equal(t, []string{"tenant-a", "tenant-a"}, []string{result[0].GetNamespace(), result[1].GetNamespace()})
			},
		},
		{
			name: "instance desired uses soft resolution",
			node: func() *Node {
				return newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withDep(newTestNode("vpc", graph.NodeTypeResource).
						withObserved(map[string]any{"status": map[string]any{"vpcId": "vpc-123"}}).build()).
					withTemplate(map[string]any{"status": map[string]any{"vpcId": "${vpc.status.vpcId}"}}).
					withTemplateVar("status.vpcId", "vpc.status.vpcId").
					withTemplateExpr("vpc.status.vpcId", variable.ResourceVariableKindDynamic).
					build()
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				vpcID, found, getErr := unstructured.NestedString(result[0].Object, "status", "vpcId")
				require.NoError(t, getErr)
				assert.True(t, found)
				assert.Equal(t, "vpc-123", vpcID)
			},
		},
		{
			name: "external collection resolves template like a single desired object",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "Service",
						"metadata": map[string]any{
							"name": "selector-template",
							"labels": map[string]any{
								"app": "demo",
							},
						},
					}).
					build()
			},
			validate: func(t *testing.T, result []*unstructured.Unstructured, err error) {
				require.NoError(t, err)
				require.Len(t, result, 1)
				assert.Equal(t, "selector-template", result[0].GetName())
			},
		},
		{
			name: "dependency readiness failures are wrapped",
			node: func() *Node {
				dep := newTestNode("vpc", graph.NodeTypeResource).
					withObserved(map[string]any{
						"status": map[string]any{"replicas": int64(3), "divisor": int64(0)},
					}).
					withReadyWhen("vpc.status.replicas / vpc.status.divisor > 0").
					build()

				return newTestNode("configmap", graph.NodeTypeResource).
					withDep(dep).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "child"},
					}).
					build()
			},
			validate: func(t *testing.T, _ []*unstructured.Unstructured, err error) {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "failed to check readiness of dependent node")
			},
		},
		{
			name: "unknown node types panic",
			node: func() *Node {
				return newTestNode("mystery", graph.NodeType(99)).build()
			},
			expectPanic: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := tt.node()
			if tt.expectPanic {
				assert.Panics(t, func() {
					_, _ = node.GetDesired()
				})
				return
			}

			result, err := node.GetDesired()
			tt.validate(t, result, err)
		})
	}
}

func TestNode_SetObserved_ExternalCollection(t *testing.T) {
	tests := []struct {
		name      string
		node      *Node
		observed  []*unstructured.Unstructured
		wantNames []string
	}{
		{
			name: "external collection keeps observed order untouched",
			node: newTestNode("external", graph.NodeTypeExternalCollection).build(),
			observed: []*unstructured.Unstructured{
				newUnstructured("v1", "Service", "ns", "c"),
				newUnstructured("v1", "Service", "ns", "a"),
				newUnstructured("v1", "Service", "ns", "b"),
			},
			wantNames: []string{"c", "a", "b"},
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

func TestNode_CheckReadiness_ExternalCollection(t *testing.T) {
	tests := []struct {
		name           string
		node           func() *Node
		wantErrIs      error
		wantErrContain string
	}{
		{
			name: "missing observed state does not block external collection readiness",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withReadyWhen("each.status.ready == true").
					build()
			},
		},
		{
			name: "observed external items can satisfy readyWhen",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withObserved(map[string]any{"status": map[string]any{"ready": true}}).
					withReadyWhen("each.status.ready == true").
					build()
			},
		},
		{
			name: "pending readyWhen surfaces waiting sentinel",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withObserved(map[string]any{}).
					withReadyWhen("each.status.ready == true").
					build()
			},
			wantErrIs: ErrWaitingForReadiness,
		},
		{
			name: "non-bool readyWhen returns a type error",
			node: func() *Node {
				return newTestNode("external", graph.NodeTypeExternalCollection).
					withObserved(map[string]any{"status": map[string]any{"ready": "yes"}}).
					withReadyWhen("each.status.ready").
					build()
			},
			wantErrContain: "did not return bool",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.node().CheckReadiness()
			if tt.wantErrIs == nil && tt.wantErrContain == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			if tt.wantErrIs != nil {
				assert.ErrorIs(t, err, tt.wantErrIs)
			}
			if tt.wantErrContain != "" {
				assert.Contains(t, err.Error(), tt.wantErrContain)
			}
		})
	}
}

func TestNode_CheckReadiness_IsIgnoredErrors(t *testing.T) {
	tests := []struct {
		name           string
		node           func() *Node
		wantErrContain string
	}{
		{
			name: "ignore evaluation failures are surfaced before readiness checks",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"total": int64(10), "divisor": int64(0)},
					}).build()
				parent := newTestNode("parent", graph.NodeTypeResource).
					withDep(schema).
					withIncludeWhen("schema.spec.total / schema.spec.divisor > 0").
					build()
				return newTestNode("child", graph.NodeTypeResource).
					withDep(parent).
					build()
			},
			wantErrContain: "is ignore check failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.node().CheckReadiness()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrContain)
		})
	}
}

func TestNode_EvaluateForEach_Errors(t *testing.T) {
	tests := []struct {
		name           string
		node           func() *Node
		wantErrIs      error
		wantErrContain string
	}{
		{
			name: "missing list input returns data pending",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{}}).
					build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					build()
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				return node
			},
			wantErrIs: ErrDataPending,
		},
		{
			name: "non-list forEach input returns a type error",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{"spec": map[string]any{"regions": int64(1)}}).
					build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					build()
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				return node
			},
			wantErrContain: "did not return a list",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := tt.node().evaluateForEach()
			assert.Nil(t, result)
			require.Error(t, err)
			if tt.wantErrIs != nil {
				assert.ErrorIs(t, err, tt.wantErrIs)
			}
			if tt.wantErrContain != "" {
				assert.Contains(t, err.Error(), tt.wantErrContain)
			}
		})
	}
}

func TestNode_HardResolveSingleResource_Errors(t *testing.T) {
	tests := []struct {
		name           string
		node           func() *Node
		wantErrContain string
	}{
		{
			name: "resolver summary errors are returned",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"name": "demo"},
					}).build()
				node := newTestNode("configmap", graph.NodeTypeResource).
					withDep(schema).
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "demo"},
					}).
					withTemplateVar("metadata[", "schema.spec.name").
					withTemplateExpr("schema.spec.name", variable.ResourceVariableKindStatic).
					build()
				node.templateExprs[0].Expression.References = []string{"schema"}
				return node
			},
			wantErrContain: "resolve errors",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := tt.node()
			result, err := node.hardResolveSingleResource(node.templateVars)
			assert.Nil(t, result)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErrContain)
		})
	}
}

func TestNode_HardResolveCollection_Errors(t *testing.T) {
	tests := []struct {
		name           string
		node           func() *Node
		wantErrIs      error
		wantErrContain string
	}{
		{
			name: "base expression failures are wrapped",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"divisor": int64(0),
							"regions": []any{"east"},
							"total":   int64(10),
						},
					}).build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${schema.spec.total / schema.spec.divisor}"},
					}).
					withTemplateVar("metadata.name", "schema.spec.total / schema.spec.divisor").
					withTemplateExpr("schema.spec.total / schema.spec.divisor", variable.ResourceVariableKindStatic).
					build()
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				node.templateExprs[0].Expression.References = []string{"schema"}
				node.forEachExprs[0].Expression.References = []string{"schema"}
				return node
			},
			wantErrContain: `node "configs" base eval`,
		},
		{
			name: "iteration pending errors return ErrDataPending",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{
							"items":  []any{map[string]any{}},
							"prefix": "cfg",
						},
					}).build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.items").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${schema.spec.prefix + '-' + item.missing}"},
					}).
					withTemplateVar("metadata.name", "schema.spec.prefix + '-' + item.missing").
					withTemplateExpr("schema.spec.prefix + '-' + item.missing", variable.ResourceVariableKindIteration).
					build()
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "item", Expression: krocel.NewUncompiled("schema.spec.items")},
				}
				node.templateExprs[0].Expression.References = []string{"schema"}
				node.forEachExprs[0].Expression.References = []string{"schema"}
				return node
			},
			wantErrIs: ErrDataPending,
		},
		{
			name: "resolver summary errors are returned",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"spec": map[string]any{"regions": []any{"east"}},
					}).build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).
					withTemplateVar("metadata[", "region").
					withTemplateExpr("region", variable.ResourceVariableKindIteration).
					build()
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				node.forEachExprs[0].Expression.References = []string{"schema"}
				return node
			},
			wantErrContain: "collection resolve: resolve errors",
		},
		{
			name: "identity collisions are rejected",
			node: func() *Node {
				schema := newTestNode(graph.InstanceNodeID, graph.NodeTypeInstance).
					withObserved(map[string]any{
						"metadata": map[string]any{"namespace": "tenant-a"},
						"spec":     map[string]any{"regions": []any{"dup", "dup"}},
					}).build()
				node := newTestNode("configs", graph.NodeTypeCollection).
					withDep(schema).
					withForEach("schema.spec.regions").
					withTemplate(map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${region}"},
					}).
					withTemplateVar("metadata.name", "region").
					withTemplateExpr("region", variable.ResourceVariableKindIteration).
					build()
				node.Spec.Meta.Namespaced = true
				node.Spec.ForEach = []graph.ForEachDimension{
					{Name: "region", Expression: krocel.NewUncompiled("schema.spec.regions")},
				}
				node.forEachExprs[0].Expression.References = []string{"schema"}
				return node
			},
			wantErrContain: "identity collision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := tt.node()
			result, err := node.hardResolveCollection(node.templateVars, true)
			assert.Nil(t, result)
			require.Error(t, err)
			if tt.wantErrIs != nil {
				assert.ErrorIs(t, err, tt.wantErrIs)
			}
			if tt.wantErrContain != "" {
				assert.Contains(t, err.Error(), tt.wantErrContain)
			}
		})
	}
}
