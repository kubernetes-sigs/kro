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

package core_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "sigs.k8s.io/krocodile/api/v1alpha1"
	"sigs.k8s.io/krocodile/test/integration/environment"
)

// TestNestedGraphCaptureAndApply: a subgraph captures a parent def, applies a
// child ConfigMap named from it, the Graph goes Ready, and tracking records
// the child with a frame-qualified NodeID.
func TestNestedGraphCaptureAndApply(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "nested", Namespace: ns},
		Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
			{
				ID:  "cfg",
				Def: environment.RawExt(t, map[string]any{"name": "nested-child-cm"}),
			},
			{
				ID: "sub",
				Graph: environment.RawExt(t, map[string]any{
					"nodes": []any{
						map[string]any{
							"id": "cm",
							"template": map[string]any{
								"apiVersion": "v1", "kind": "ConfigMap",
								"metadata": map[string]any{"name": "${cfg.name}"}, // captures parent
								"data":     map[string]any{"hello": "from-nested"},
							},
						},
					},
				}),
			},
		}},
	}
	env.CreateGraph(t, g)

	key := types.NamespacedName{Namespace: ns, Name: "nested"}
	env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 15*time.Second)
	env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

	env.AwaitObject(t, configMapGVK,
		types.NamespacedName{Namespace: ns, Name: "nested-child-cm"},
		func(u *unstructured.Unstructured) error {
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if data["hello"] != "from-nested" {
				return errMismatch("data.hello", "from-nested", data["hello"])
			}
			return nil
		},
		15*time.Second,
	)

	// The child resource is tracked under a frame-qualified NodeID.
	got := env.GetGraph(t, key)
	var found bool
	for _, mr := range got.Status.ManagedResources {
		if mr.NodeID == "sub/cm" && mr.Name == "nested-child-cm" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected managed resource NodeID=sub/cm name=nested-child-cm, got %+v", got.Status.ManagedResources)
	}
}

// TestNestedGraphParentReadsChildOutput: a parent template names its
// ConfigMap from a child subgraph's output, addressed through the subgraph
// node ID (${sub.out.name}).
func TestNestedGraphParentReadsChildOutput(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "reads-child", Namespace: ns},
		Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
			{
				ID: "sub",
				Graph: environment.RawExt(t, map[string]any{
					"nodes": []any{
						map[string]any{
							"id":  "out",
							"def": map[string]any{"name": "parent-named-me"},
						},
					},
				}),
			},
			{
				ID: "cm",
				Template: environment.RawExt(t, map[string]any{
					"apiVersion": "v1", "kind": "ConfigMap",
					"metadata": map[string]any{"name": "${sub.out.name}"}, // reads child output
					"data":     map[string]any{"k": "v"},
				}),
			},
		}},
	}
	env.CreateGraph(t, g)

	key := types.NamespacedName{Namespace: ns, Name: "reads-child"}
	env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)
	env.AwaitObject(t, configMapGVK,
		types.NamespacedName{Namespace: ns, Name: "parent-named-me"},
		func(*unstructured.Unstructured) error { return nil },
		15*time.Second,
	)
}

// TestNestedGraphDeletionRemovesChild: deleting the Graph tears down the
// nested child resource — tracking (with qualified NodeIDs) drives delete.
func TestNestedGraphDeletionRemovesChild(t *testing.T) {
	env := environment.Shared(t)
	ns := env.CreateNamespace(t)

	g := &expv1alpha1.Graph{
		ObjectMeta: metav1.ObjectMeta{Name: "nested-del", Namespace: ns},
		Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
			{
				ID: "sub",
				Graph: environment.RawExt(t, map[string]any{
					"nodes": []any{
						map[string]any{
							"id": "cm",
							"template": map[string]any{
								"apiVersion": "v1", "kind": "ConfigMap",
								"metadata": map[string]any{"name": "doomed-child"},
								"data":     map[string]any{"k": "v"},
							},
						},
					},
				}),
			},
		}},
	}
	env.CreateGraph(t, g)

	key := types.NamespacedName{Namespace: ns, Name: "nested-del"}
	cmKey := types.NamespacedName{Namespace: ns, Name: "doomed-child"}
	env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)
	env.AwaitObject(t, configMapGVK, cmKey, func(*unstructured.Unstructured) error { return nil }, 15*time.Second)

	got := env.GetGraph(t, key)
	if err := env.Client.Delete(env.Ctx, got); err != nil {
		t.Fatalf("delete graph: %v", err)
	}
	env.AwaitDeleted(t, configMapGVK, cmKey, 15*time.Second)
	env.AwaitGraphGone(t, key, 15*time.Second)
}
