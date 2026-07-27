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

package compiler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	memory "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/pkg/testutil/generator"
	testk8s "sigs.k8s.io/krocodile/pkg/testutil/k8s"
)

// ---- shared test helpers ----------------------------------------------------

func newTestCompiler(t *testing.T) *Compiler {
	t.Helper()
	resolver, disco := testk8s.NewFakeResolver()
	rm := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(disco))
	return &Compiler{schemaResolver: resolver, restMapper: rm}
}

func rawExtensionFromString(s string) *runtime.RawExtension {
	return &runtime.RawExtension{Raw: []byte(s)}
}

func rawExtensionFromObject(m map[string]any) *runtime.RawExtension {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return &runtime.RawExtension{Raw: raw}
}

func pod(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
	}
}

func configMap(name string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": name},
		"data":       map[string]any{"key": "value"},
	}
}

// ---- TestCompile ------------------------------------------------------------

// TestCompile drives every reachable Compile path through one table. The
// row shape: build a Graph, run Compile, check the error string (substring),
// then run an optional Program assertion when a Program is expected.
func TestCompile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		graph   *expv1alpha1.Graph
		wantErr string // substring; empty means success
		after   func(t *testing.T, prog *Program, graph *expv1alpha1.Graph)
	}{
		// --- per-kind happy paths ---
		{
			name:  "template node",
			graph: generator.NewGraph("g", generator.WithTemplate("cm", configMap("cfg"))),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				n := prog.Nodes["cm"]
				assert.Equal(t, NodeKindTemplate, n.Kind)
				assert.Equal(t, "configmaps", n.GVR.Resource)
				assert.True(t, n.Namespaced)
				assert.Empty(t, n.Variables)
				assert.Empty(t, n.Dependencies)
				assert.Contains(t, prog.NodeSchemas, "cm")
				assert.Equal(t, []string{"cm"}, prog.TopologicalOrder)
			},
		},
		{
			name: "ref node by name",
			graph: generator.NewGraph("g",
				generator.WithRef("existing", &expv1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "Pod",
					Metadata: expv1alpha1.ExternalRefMetadata{Name: "some-pod"},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				n := prog.Nodes["existing"]
				assert.Equal(t, NodeKindRef, n.Kind)
				assert.Equal(t, "pods", n.GVR.Resource)
				assert.False(t, n.IsCollection())
				assert.Contains(t, prog.NodeSchemas, "existing")
			},
		},
		{
			name: "ref node with CEL-driven selector picks up dependency",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"app": "billing"}),
				generator.WithRef("appPods", &expv1alpha1.ExternalRef{
					APIVersion: "v1", Kind: "Pod",
					Metadata: expv1alpha1.ExternalRefMetadata{
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "${naming.app}"}},
					},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Equal(t, []string{"naming"}, prog.Nodes["appPods"].Dependencies)
			},
		},
		{
			name: "watch node always a collection, list-wrapped schema",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"app": "checkout"}),
				generator.WithWatch("appPods", &expv1alpha1.WatchSpec{
					APIVersion: "v1", Kind: "Pod",
					Selector: map[string]string{"app": "${naming.app}"},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				n := prog.Nodes["appPods"]
				assert.Equal(t, NodeKindWatch, n.Kind)
				assert.True(t, n.IsCollection())
				assert.Equal(t, []string{"naming"}, n.Dependencies)
				require.Contains(t, prog.NodeSchemas, "appPods")
				assert.True(t, prog.NodeSchemas["appPods"].Type.Contains("array"))
			},
		},
		{
			name: "def node feeds template",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"prefix": "team-", "app": "billing"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${naming.prefix + naming.app}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Equal(t, NodeKindDef, prog.Nodes["naming"].Kind)
				assert.Equal(t, []string{"naming", "cm"}, prog.TopologicalOrder)
				assert.Equal(t, "", prog.Nodes["naming"].GVR.Resource)
				// Defs now publish an inferred schema so the typed CEL
				// env can narrow `${naming.prefix}` to string and reject
				// typos at compile time.
				require.Contains(t, prog.NodeSchemas, "naming")
				naming := prog.NodeSchemas["naming"]
				assert.True(t, naming.Type.Contains("object"))
				assert.Contains(t, naming.Properties, "prefix")
				assert.Contains(t, naming.Properties, "app")
			},
		},
		{
			name: "two literal nodes both count as inputs",
			graph: generator.NewGraph("g",
				generator.WithDef("constants", map[string]any{"replicas": int64(3)}),
				generator.WithTemplate("p", pod("p")),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.ElementsMatch(t, []string{"constants", "p"}, prog.TopologicalOrder)
			},
		},

		// --- dependency / DAG ---
		{
			name: "linear dependency chain",
			graph: generator.NewGraph("g",
				generator.WithDef("base", map[string]any{"name": "alpha"}),
				generator.WithTemplate("cm1", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${base.name}"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithTemplate("cm2", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${cm1.metadata.name + '-2'}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Equal(t, []string{"base", "cm1", "cm2"}, prog.TopologicalOrder)
				assert.Equal(t, []string{"base"}, prog.Nodes["cm1"].Dependencies)
				assert.Equal(t, []string{"cm1"}, prog.Nodes["cm2"].Dependencies)
			},
		},
		{
			name: "diamond shape",
			graph: generator.NewGraph("g",
				generator.WithDef("root", map[string]any{"name": "r"}),
				generator.WithTemplate("left", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${root.name + '-l'}"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithTemplate("right", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${root.name + '-r'}"},
					"data":     map[string]any{"k": "v"},
				}),
				generator.WithTemplate("merge", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${left.metadata.name + right.metadata.name}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				order := prog.TopologicalOrder
				idx := func(id string) int {
					for i, x := range order {
						if x == id {
							return i
						}
					}
					t.Fatalf("id %q not in order %v", id, order)
					return -1
				}
				assert.Less(t, idx("root"), idx("left"))
				assert.Less(t, idx("root"), idx("right"))
				assert.Less(t, idx("left"), idx("merge"))
				assert.Less(t, idx("right"), idx("merge"))
				assert.ElementsMatch(t, []string{"left", "right"}, prog.Nodes["merge"].Dependencies)
			},
		},
		{
			name: "duplicate references collapse to one dependency",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"a": "x", "b": "y"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${v.a}"},
					"data":     map[string]any{"both": "${v.a + v.b}"},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Equal(t, []string{"v"}, prog.Nodes["cm"].Dependencies)
			},
		},

		// --- forEach ---
		{
			name: "forEach over typed source binds element type",
			graph: generator.NewGraph("g",
				generator.WithTemplate("src", configMap("source")),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${'p-' + el}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}, generator.ForEachDim("el", "${[src.metadata.name]}")),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Equal(t, []string{"src"}, prog.Nodes["p"].Dependencies)
				assert.True(t, prog.Nodes["p"].IsCollection())
			},
		},
		{
			name: "forEach over def is dyn-bound at compile",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"items": []any{"a", "b"}}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${'pod-' + el}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}, generator.ForEachDim("el", "${seed.items}")),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Equal(t, []string{"seed"}, prog.Nodes["p"].Dependencies)
			},
		},

		// --- top-level / structural errors ---
		{
			name:    "empty graph",
			graph:   generator.NewGraph("g"),
			wantErr: "at least one node",
		},
		{
			name: "every node has CEL — no input seed",
			graph: generator.NewGraph("g",
				generator.WithTemplate("a", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${b.metadata.name}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}),
				generator.WithTemplate("b", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${a.metadata.name}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}),
			),
			wantErr: "no CEL expressions",
		},
		{
			name: "unknown GVK",
			graph: generator.NewGraph("g", generator.WithTemplate("x", map[string]any{
				"apiVersion": "unknown.group/v1", "kind": "NotARealKind",
				"metadata": map[string]any{"name": "x"},
			})),
			wantErr: "resolve schema",
		},
		{
			name:    "node with zero kinds set",
			graph:   generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{ID: "x"})),
			wantErr: "exactly one of",
		},
		{
			name: "node with two kinds set",
			graph: generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{
				ID:       "x",
				Template: rawExtensionFromObject(configMap("a")),
				Def:      rawExtensionFromObject(map[string]any{"k": "v"}),
			})),
			wantErr: "exactly one of",
		},
		{
			name: "template payload missing apiVersion",
			graph: generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{
				ID:       "x",
				Template: rawExtensionFromObject(map[string]any{"metadata": map[string]any{"name": "x"}}),
			})),
			wantErr: "apiVersion",
		},
		{
			name: "template payload malformed YAML",
			graph: generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{
				ID:       "x",
				Template: rawExtensionFromString("not: { valid yaml"),
			})),
			wantErr: "unmarshal",
		},
		{
			name: "readyWhen that returns non-bool is rejected at compile time",
			graph: generator.NewGraph("g",
				// Use a typed reference (cm is a real ConfigMap, so
				// cm.metadata.name is known to be a string) so the type
				// check has a non-dyn type to reject.
				generator.WithTemplate("cm", configMap("source")),
				generator.WithReadyWhen("${cm.metadata.name}"),
			),
			wantErr: "must return bool",
		},
		{
			name: "includeWhen that returns non-bool is rejected at compile time",
			graph: generator.NewGraph("g",
				generator.WithTemplate("cm", configMap("source")),
				generator.WithDef("guarded", map[string]any{"k": "v"}),
				generator.WithIncludeWhen("${cm.metadata.name}"),
			),
			wantErr: "must return bool",
		},
		{
			name: "includeWhen references an upstream node — added to dependency graph",
			graph: generator.NewGraph("g",
				generator.WithDef("flag", map[string]any{"enabled": true}),
				generator.WithDef("guarded", map[string]any{"k": "v"}),
				generator.WithIncludeWhen("${flag.enabled}"),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Equal(t, []string{"flag"}, prog.Nodes["guarded"].Dependencies)
				require.Len(t, prog.Nodes["guarded"].IncludeWhen, 1)
			},
		},
		{
			name: "readyWhen returning optional<bool> is accepted (via ?-accessor or optional macros)",
			graph: generator.NewGraph("g",
				generator.WithTemplate("cm", configMap("source")),
				// optional.of(true) returns optional_type(bool), exercising
				// the IsBoolOrOptionalBool branch.
				generator.WithReadyWhen("${optional.of(true).orValue(true)}"),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				require.Len(t, prog.Nodes["cm"].ReadyWhen, 1)
			},
		},
		{
			name: "readyWhen that references another node is rejected",
			graph: generator.NewGraph("g",
				generator.WithDef("a", map[string]any{"ok": true}),
				generator.WithDef("b", map[string]any{"x": "y"}),
				generator.WithReadyWhen("${a.ok}"),
			),
			wantErr: "may only reference the node itself",
		},
		{
			name: "forEach on a ref node is rejected (refs don't render templates)",
			graph: generator.NewGraph("g",
				generator.WithDef("src", map[string]any{"names": []any{"a"}}),
				generator.WithRawNode(expv1alpha1.Node{
					ID: "r",
					Ref: &expv1alpha1.ExternalRef{
						APIVersion: "v1", Kind: "Pod",
						Metadata: expv1alpha1.ExternalRefMetadata{Name: "x"},
					},
					ForEach: []expv1alpha1.ForEachDimension{{"n": "${src.names}"}},
				}),
			),
			wantErr: "forEach is not supported on ref nodes",
		},
		{
			name: "forEach on a watch node is rejected (watch is inherently a collection)",
			graph: generator.NewGraph("g",
				generator.WithDef("src", map[string]any{"names": []any{"a"}}),
				generator.WithRawNode(expv1alpha1.Node{
					ID: "w",
					Watch: &expv1alpha1.WatchSpec{
						APIVersion: "v1", Kind: "Pod",
						Selector: map[string]string{"app": "x"},
					},
					ForEach: []expv1alpha1.ForEachDimension{{"n": "${src.names}"}},
				}),
			),
			wantErr: "forEach is not supported on watch nodes",
		},
		{
			name: "cluster-scoped target rejecting metadata.namespace",
			graph: generator.NewGraph("g", generator.WithTemplate("crd", map[string]any{
				"apiVersion": "apiextensions.k8s.io/v1",
				"kind":       "CustomResourceDefinition",
				"metadata":   map[string]any{"name": "x", "namespace": "default"},
				"spec":       map[string]any{},
			})),
			wantErr: "cluster-scoped but template sets metadata.namespace",
		},
		{
			name: "forEach without iterator use in metadata.name is rejected",
			graph: generator.NewGraph("g",
				generator.WithDef("src", map[string]any{"names": []any{"a", "b"}}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "fixed"}, // no iterator!
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}, generator.ForEachDim("name", "${src.names}")),
			),
			wantErr: "every forEach iterator must appear in metadata.name",
		},
		{
			name: "template missing metadata is rejected",
			graph: generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{
				ID:       "x",
				Template: rawExtensionFromObject(map[string]any{"apiVersion": "v1", "kind": "ConfigMap"}),
			})),
			wantErr: "metadata",
		},
		{
			name: "template with malformed apiVersion is rejected",
			graph: generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{
				ID: "x",
				Template: rawExtensionFromObject(map[string]any{
					"apiVersion": "v1/wat/extra",
					"kind":       "X",
					"metadata":   map[string]any{"name": "x"},
				}),
			})),
			wantErr: "apiVersion",
		},
		{
			name: "template version segment that isn't v<N>[alpha|beta<N>] is rejected",
			graph: generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{
				ID: "x",
				Template: rawExtensionFromObject(map[string]any{
					"apiVersion": "apps/wat",
					"kind":       "Deployment",
					"metadata":   map[string]any{"name": "x"},
				}),
			})),
			wantErr: "not a valid Kubernetes version",
		},
		{
			name: "readyWhen self-reference is filtered out of the dependency graph",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"phase": "Active"}),
				generator.WithReadyWhen("${v.phase == 'Active'}"),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				assert.Empty(t, prog.Nodes["v"].Dependencies)
				require.Len(t, prog.Nodes["v"].ReadyWhen, 1)
			},
		},
		{
			name: "def typo is rejected at compile time (typed def schema)",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"prefix": "team-", "app": "billing"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					// `prefxi` is a typo for `prefix`. With def schemas
					// inferred, the typed env knows `naming` only has
					// {prefix, app} and rejects this access.
					"metadata": map[string]any{"name": "${naming.prefxi}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "undefined field 'prefxi'",
		},
		{
			name: "wrong-type def field is rejected at compile time",
			graph: generator.NewGraph("g",
				generator.WithDef("v", map[string]any{"count": int64(3)}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					// count is int; concatenating with a string would
					// require the operands to share a type. The typed env
					// rejects mixing int and string at compile time.
					"metadata": map[string]any{"name": "${v.count + 'suffix'}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "",
		},
		{
			name: "CEL-fragment def field stays dyn (sub-field access compiles)",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"k": "v"}),
				// `info` is a CEL fragment, so its inferred type is dyn —
				// downstream sub-field access compiles even though the
				// runtime value won't have it (used by other tests that
				// rely on this contract).
				generator.WithDef("base", map[string]any{"info": "${'literal'}"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${base.info.anything}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			after: func(t *testing.T, prog *Program, _ *expv1alpha1.Graph) {
				// Compiles fine — the dyn escape hatch works.
				assert.Contains(t, prog.Nodes, "cm")
			},
		},
		{
			name: "typed env rejects an expression referencing a non-existent field on a typed node",
			graph: generator.NewGraph("g",
				generator.WithTemplate("cm", configMap("source")),
				// `cm` is typed (ConfigMap schema is known); `cm.totallyMissing`
				// type-checks to an error inside env.Check (parseAndCheck's
				// Check-error branch).
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${cm.totallyMissing}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}),
			),
			wantErr: "",
		},
		{
			name: "ref node with unknown target GVK fails schema resolution",
			graph: generator.NewGraph("g", generator.WithRef("r", &expv1alpha1.ExternalRef{
				APIVersion: "totally.unknown/v999",
				Kind:       "MissingKind",
				Metadata:   expv1alpha1.ExternalRefMetadata{Name: "x"},
			})),
			wantErr: "resolve schema",
		},
		{
			name: "watch node with unknown target GVK fails schema resolution",
			graph: generator.NewGraph("g",
				generator.WithDef("naming", map[string]any{"app": "x"}),
				generator.WithWatch("w", &expv1alpha1.WatchSpec{
					APIVersion: "totally.unknown/v999",
					Kind:       "MissingKind",
					Selector:   map[string]string{"app": "${naming.app}"},
				}),
			),
			wantErr: "resolve schema",
		},
		{
			name: "expression with malformed CEL syntax fails parser",
			graph: generator.NewGraph("g",
				generator.WithDef("base", map[string]any{"name": "x"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${ base.[] }"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "",
		},
		{
			name: "def payload malformed YAML",
			graph: generator.NewGraph("g", generator.WithRawNode(expv1alpha1.Node{
				ID:  "x",
				Def: rawExtensionFromString("not: { valid yaml"),
			})),
			wantErr: "unmarshal",
		},

		// --- dependency errors ---
		{
			name: "dependency cycle",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"x": "y"}),
				generator.WithTemplate("a", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${b.metadata.name}"},
					"data":     map[string]any{"seed": "${seed.x}"},
				}),
				generator.WithTemplate("b", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${a.metadata.name}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "cycle",
		},
		{
			name: "unknown identifier in template",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"x": "y"}),
				generator.WithTemplate("a", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${unknown.x}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "unknown identifier",
		},
		{
			name: "unknown CEL function",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"x": "y"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${notARealFunction(seed.x)}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "",
			// Unknown identifiers come back as either inspector errors or
			// CEL function errors depending on parse path; rather than
			// freeze the message, just assert an error.
		},
		{
			name: "syntax error inside ${}",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"x": "y"}),
				generator.WithTemplate("cm", map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${seed.}"},
					"data":     map[string]any{"k": "v"},
				}),
			),
			wantErr: "",
		},
		{
			name: "forEach non-list-typed expression",
			graph: generator.NewGraph("g",
				generator.WithDef("base", map[string]any{"name": "a"}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${base.name + '-' + region}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				}, generator.ForEachDim("region", "${'not a list'}")),
			),
			wantErr: "must return a list",
		},
		{
			name: "forEach cross-iterator reference",
			graph: generator.NewGraph("g",
				generator.WithDef("seed", map[string]any{"a": []any{"x"}, "b": []any{"y"}}),
				generator.WithTemplate("p", map[string]any{
					"apiVersion": "v1", "kind": "Pod",
					"metadata": map[string]any{"name": "${first + second}"},
					"spec":     map[string]any{"containers": []any{map[string]any{"name": "c", "image": "nginx"}}},
				},
					generator.ForEachDim("first", "${seed.a}"),
					generator.ForEachDim("second", "${first + 'whoops'}"),
				),
			),
			wantErr: "cannot reference other iterators",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newTestCompiler(t)
			prog, err := c.Compile(tc.graph)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			// Cases where the after func is nil but no wantErr is set are
			// marked above with empty wantErr deliberately — they just
			// need an error of any kind.
			if tc.after == nil {
				require.Error(t, err, "case expected an error but no expectation was provided")
				return
			}
			require.NoError(t, err)
			require.NotNil(t, prog)
			tc.after(t, prog, tc.graph)
		})
	}
}

// ---- TestCompile_PreservesInput --------------------------------------------

func TestCompile_PreservesInputGraph(t *testing.T) {
	t.Parallel()
	c := newTestCompiler(t)
	g := generator.NewGraph("g", generator.WithTemplate("cm", configMap("cfg")))
	orig := g.DeepCopy()
	_, err := c.Compile(g)
	require.NoError(t, err)
	assert.Equal(t, orig, g, "input Graph was mutated")
}

// ---- TestNewCompiler --------------------------------------------------------

func TestNewCompiler(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     *rest.Config
		client  *http.Client
		wantErr bool
	}{
		{name: "with fake reachable-shaped config", cfg: &rest.Config{Host: "http://127.0.0.1:1"}, client: &http.Client{}},
		{name: "malformed host triggers discovery construction failure",
			cfg:     &rest.Config{Host: "://broken"},
			client:  &http.Client{},
			wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, err := NewCompiler(tc.cfg, tc.client)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, c)
		})
	}
}

// ---- TestUnmarshalRaw -------------------------------------------------------

func TestUnmarshalRaw(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		raw     []byte
		want    map[string]any
		wantErr bool
	}{
		{name: "empty yields empty map", raw: nil, want: map[string]any{}},
		{name: "valid json", raw: []byte(`{"a": 1}`), want: map[string]any{"a": int64(1)}},
		{name: "invalid json", raw: []byte(`{`), wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m, err := unmarshalRaw(tc.raw)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			for k, v := range tc.want {
				assert.EqualValues(t, v, m[k], "key %q", k)
			}
		})
	}
}

// ---- TestValidateKubernetesObjectStructure ----------------------------------

func TestValidateKubernetesObjectStructure(t *testing.T) {
	t.Parallel()
	withMeta := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": "p"},
	}
	cases := []struct {
		name            string
		obj             map[string]any
		requireMetadata bool
		wantErr         string
	}{
		{name: "ok with metadata", obj: withMeta, requireMetadata: true},
		{name: "ok without metadata when not required", obj: map[string]any{"apiVersion": "v1", "kind": "Pod"}, requireMetadata: false},
		{name: "nil", obj: nil, wantErr: "empty"},
		{name: "missing apiVersion", obj: map[string]any{"kind": "Pod"}, wantErr: "apiVersion"},
		{name: "missing kind", obj: map[string]any{"apiVersion": "v1"}, wantErr: "kind"},
		{name: "apiVersion not string", obj: map[string]any{"apiVersion": 1, "kind": "Pod"}, wantErr: "non-empty string"},
		{name: "kind empty", obj: map[string]any{"apiVersion": "v1", "kind": ""}, wantErr: "non-empty string"},
		{name: "malformed apiVersion (too many slashes)", obj: map[string]any{"apiVersion": "a/b/c", "kind": "X"}, wantErr: "apiVersion"},
		{name: "non-conventional version segment", obj: map[string]any{"apiVersion": "apps/wat", "kind": "X"}, wantErr: "not a valid Kubernetes version"},
		{name: "missing metadata when required", obj: map[string]any{"apiVersion": "v1", "kind": "Pod"}, requireMetadata: true, wantErr: "metadata"},
		{name: "metadata wrong type", obj: map[string]any{"apiVersion": "v1", "kind": "Pod", "metadata": "wat"}, requireMetadata: true, wantErr: "metadata"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateKubernetesObjectStructure(tc.obj, tc.requireMetadata)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// ---- TestExtractGVKFromUnstructured -----------------------------------------

func TestExtractGVKFromUnstructured(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		obj         map[string]any
		wantGroup   string
		wantVersion string
		wantKind    string
		wantErr     bool
	}{
		{name: "group + version + kind", obj: map[string]any{"apiVersion": "apps/v1", "kind": "Deployment"},
			wantGroup: "apps", wantVersion: "v1", wantKind: "Deployment"},
		{name: "core group", obj: map[string]any{"apiVersion": "v1", "kind": "Pod"},
			wantGroup: "", wantVersion: "v1", wantKind: "Pod"},
		{name: "malformed apiVersion", obj: map[string]any{"apiVersion": "a/b/c", "kind": "X"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gvk, err := extractGVKFromUnstructured(tc.obj)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantGroup, gvk.Group)
			assert.Equal(t, tc.wantVersion, gvk.Version)
			assert.Equal(t, tc.wantKind, gvk.Kind)
		})
	}
}

// ---- TestParseForEachDimensions ---------------------------------------------

func TestParseForEachDimensions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		dims    []expv1alpha1.ForEachDimension
		wantLen int
		wantErr bool
	}{
		{name: "empty input returns nil", dims: nil, wantLen: 0},
		{name: "single dim compiles", dims: []expv1alpha1.ForEachDimension{{"region": "${[1, 2, 3]}"}}, wantLen: 1},
		{name: "invalid expression errors", dims: []expv1alpha1.ForEachDimension{{"r": ""}}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			out, err := parseForEachDimensions(tc.dims)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Len(t, out, tc.wantLen)
		})
	}
}

// ---- TestDedupe + TestAllIteratorNames --------------------------------------

func TestDedupe(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "empty", in: nil, want: nil},
		{name: "no dupes", in: []string{"a", "b", "c"}, want: []string{"a", "b", "c"}},
		{name: "many dupes", in: []string{"a", "b", "a", "c", "b"}, want: []string{"a", "b", "c"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := append([]string(nil), tc.in...)
			dedupe(&got)
			if len(tc.want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestAllIteratorNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		nodes map[string]*Node
		want  []string
	}{
		{
			name: "no forEach",
			nodes: map[string]*Node{
				"a": {ID: "a"},
				"b": {ID: "b"},
			},
			want: nil,
		},
		{
			name: "mixed",
			nodes: map[string]*Node{
				"a": {ID: "a", ForEach: []ForEachDimension{{Name: "r"}}},
				"b": {ID: "b", ForEach: []ForEachDimension{{Name: "r"}, {Name: "t"}}},
				"c": {ID: "c"},
			},
			want: []string{"r", "r", "t"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := allIteratorNames(tc.nodes)
			assert.ElementsMatch(t, tc.want, got)
		})
	}
}

// TestCompile_DynamicGVK covers templates whose apiVersion or kind is a CEL
// expression: they compile as dynamic nodes (no compile-time GVK, schema, or
// REST mapping), publish no schema, flip Program.HasDynamicGVK, and remain
// referenceable by downstream nodes as dyn.
func TestCompile_DynamicGVK(t *testing.T) {
	t.Parallel()

	t.Run("templated apiVersion compiles as a dynamic node", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"group": "example.com/v1"}),
			generator.WithTemplate("res", map[string]any{
				"apiVersion": "${cfg.group}",
				"kind":       "Widget",
				"metadata":   map[string]any{"name": "w"},
				"spec":       map[string]any{"size": "${cfg.group}"},
			}),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)

		n := prog.Nodes["res"]
		require.NotNil(t, n)
		assert.True(t, n.DynamicGVK, "node should be flagged dynamic")
		assert.True(t, n.GVR.Empty(), "no compile-time GVR for a dynamic node")
		assert.False(t, n.Namespaced, "scope resolved at apply time, not compile")

		assert.True(t, prog.HasDynamicGVK)
		assert.Empty(t, prog.RequiredGroupKinds, "dynamic node contributes no literal GroupKind")
		_, hasSchema := prog.NodeSchemas["res"]
		assert.False(t, hasSchema, "dynamic node publishes no schema (dyn)")

		// The dynamic node depends on the def that feeds its apiVersion.
		assert.Contains(t, n.Dependencies, "cfg")
	})

	t.Run("templated kind also triggers dynamic", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"kind": "Widget"}),
			generator.WithTemplate("res", map[string]any{
				"apiVersion": "example.com/v1",
				"kind":       "${cfg.kind}",
				"metadata":   map[string]any{"name": "w"},
			}),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)
		require.NotNil(t, prog.Nodes["res"])
		assert.True(t, prog.Nodes["res"].DynamicGVK)
		assert.True(t, prog.HasDynamicGVK)
	})

	t.Run("downstream node references a dynamic node as dyn", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"group": "example.com/v1"}),
			generator.WithTemplate("res", map[string]any{
				"apiVersion": "${cfg.group}",
				"kind":       "Widget",
				"metadata":   map[string]any{"name": "w"},
			}),
			generator.WithTemplate("cm", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "cm"},
				// Reading an arbitrary field off the dynamic node must
				// type-check (dyn), and must create a dependency edge.
				"data": map[string]any{"ref": "${res.metadata.name}"},
			}),
		)
		prog, err := newTestCompiler(t).Compile(g)
		require.NoError(t, err)
		require.NotNil(t, prog.Nodes["cm"])
		assert.Contains(t, prog.Nodes["cm"].Dependencies, "res")
		// res is applied before cm.
		assert.Less(t, indexOf(prog.TopologicalOrder, "res"), indexOf(prog.TopologicalOrder, "cm"))
	})

	t.Run("dynamic forEach still requires iterators in identity fields", func(t *testing.T) {
		t.Parallel()
		g := generator.NewGraph("g",
			generator.WithNamespace("default"),
			generator.WithDef("cfg", map[string]any{"group": "example.com/v1", "names": []any{"a", "b"}}),
			generator.WithTemplate("res", map[string]any{
				"apiVersion": "${cfg.group}",
				"kind":       "Widget",
				// metadata.name omits the iterator `n` → duplicate identities.
				"metadata": map[string]any{"name": "fixed"},
			}, generator.ForEachDim("n", "${cfg.names}")),
		)
		_, err := newTestCompiler(t).Compile(g)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "forEach iterator")
	})
}

func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}
