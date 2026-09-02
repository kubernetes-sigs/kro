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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var _ = Describe("Graph Patch", func() {
	It("contributes fields to pre-existing resources and releases them on node removal", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		cmKey := types.NamespacedName{Namespace: ns, Name: "patch-target"}

		// A ConfigMap owned by nobody in this Graph — the patch contributes to it.
		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(cmGVK)
		target.SetNamespace(ns)
		target.SetName("patch-target")
		if err := unstructured.SetNestedStringMap(target.Object, map[string]string{"orig": "kept"}, "data"); err != nil {
			t.Fatalf("set target data: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Create(ctx, target); err != nil {
			t.Fatalf("create target ConfigMap: %v", err)
		}

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "patcher", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID:  "keep",
						Def: environment.RawExt(t, map[string]any{"x": 1}),
					},
					{
						ID: "p",
						Patch: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "patch-target"},
							"data":       map[string]any{"added": "contributed"},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		gKey := types.NamespacedName{Namespace: ns, Name: "patcher"}
		env.AwaitCondition(t, gKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// The contributed field is present; the pre-existing field survives.
		env.AwaitObject(t, cmGVK, cmKey, func(u *unstructured.Unstructured) error {
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if data["added"] != "contributed" {
				return fmt.Errorf("data.added: want=contributed got=%q", data["added"])
			}
			if data["orig"] != "kept" {
				return fmt.Errorf("data.orig: want=kept got=%q", data["orig"])
			}
			return nil
		}, 15*time.Second)

		// The contribution inventory is persisted on the Graph.
		environment.Eventually(t, 10*time.Second, 200*time.Millisecond, func() error {
			cur := env.GetGraph(t, gKey)
			if len(cur.Status.Contributions) == 0 {
				return fmt.Errorf("patch-contributions not persisted to status")
			}
			return nil
		})

		// Remove the patch node from the spec. The controller releases the
		// contributed field on the next reconcile; the target object survives.
		env.UpdateGraphSpec(t, gKey, func(cur *expv1alpha1.Graph) {
			cur.Spec.Nodes = []expv1alpha1.Node{{
				ID:  "keep",
				Def: environment.RawExt(t, map[string]any{"x": 1}),
			}}
		})

		env.AwaitObject(t, cmGVK, cmKey, func(u *unstructured.Unstructured) error {
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if _, ok := data["added"]; ok {
				return fmt.Errorf("data.added still present after release")
			}
			if data["orig"] != "kept" {
				return fmt.Errorf("data.orig lost during release: got=%q", data["orig"])
			}
			return nil
		}, 15*time.Second)
	})

	It("contributes metadata labels and annotations to a pre-existing resource", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		cmKey := types.NamespacedName{Namespace: ns, Name: "hello"}

		// A ConfigMap the Graph does not own; the patch contributes metadata to it.
		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(cmGVK)
		target.SetNamespace(ns)
		target.SetName("hello")
		if err := unstructured.SetNestedStringMap(target.Object, map[string]string{"orig": "kept"}, "data"); err != nil {
			t.Fatalf("set target data: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Create(ctx, target); err != nil {
			t.Fatalf("create target ConfigMap: %v", err)
		}

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "hello-patcher", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID: "cmpatcher",
						Patch: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata": map[string]any{
								"name":        "hello",
								"namespace":   ns,
								"labels":      map[string]any{"touched-by": "kro"},
								"annotations": map[string]any{"kro.run/note": "patched"},
							},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		gKey := types.NamespacedName{Namespace: ns, Name: "hello-patcher"}
		env.AwaitCondition(t, gKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// The contributed metadata is present; the pre-existing data survives.
		env.AwaitObject(t, cmGVK, cmKey, func(u *unstructured.Unstructured) error {
			if got := u.GetLabels()["touched-by"]; got != "kro" {
				return fmt.Errorf("labels[touched-by]: want=kro got=%q", got)
			}
			if got := u.GetAnnotations()["kro.run/note"]; got != "patched" {
				return fmt.Errorf("annotations[kro.run/note]: want=patched got=%q", got)
			}
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if data["orig"] != "kept" {
				return fmt.Errorf("data.orig: want=kept got=%q", data["orig"])
			}
			return nil
		}, 15*time.Second)

		// Remove the patch node; the contributed labels/annotations are released,
		// the target object survives.
		env.UpdateGraphSpec(t, gKey, func(cur *expv1alpha1.Graph) {
			cur.Spec.Nodes = []expv1alpha1.Node{{
				ID:  "keep",
				Def: environment.RawExt(t, map[string]any{"x": 1}),
			}}
		})

		env.AwaitObject(t, cmGVK, cmKey, func(u *unstructured.Unstructured) error {
			if _, ok := u.GetLabels()["touched-by"]; ok {
				return fmt.Errorf("labels[touched-by] still present after release")
			}
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if data["orig"] != "kept" {
				return fmt.Errorf("data.orig lost during release: got=%q", data["orig"])
			}
			return nil
		}, 15*time.Second)
	})

	It("contributes and releases patches inside nested subgraphs", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		cmKey := types.NamespacedName{Namespace: ns, Name: "nested-patch-target"}

		// Target ConfigMap owned by nobody in this Graph
		target := &unstructured.Unstructured{}
		target.SetGroupVersionKind(cmGVK)
		target.SetNamespace(ns)
		target.SetName("nested-patch-target")
		if err := unstructured.SetNestedStringMap(target.Object, map[string]string{"orig": "kept"}, "data"); err != nil {
			t.Fatalf("set target data: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Create(ctx, target); err != nil {
			t.Fatalf("create target ConfigMap: %v", err)
		}

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "nested-patcher", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID:  "keep",
						Def: environment.RawExt(t, map[string]any{"x": 1}),
					},
					{
						ID: "sub",
						Graph: environment.RawExt(t, map[string]any{
							"nodes": []any{
								map[string]any{
									"id": "childp",
									"patch": map[string]any{
										"apiVersion": "v1",
										"kind":       "ConfigMap",
										"metadata":   map[string]any{"name": "nested-patch-target"},
										"data":       map[string]any{"nested-added": "nested-contributed"},
									},
								},
							},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		gKey := types.NamespacedName{Namespace: ns, Name: "nested-patcher"}
		env.AwaitCondition(t, gKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// The contributed field is present; the pre-existing field survives.
		env.AwaitObject(t, cmGVK, cmKey, func(u *unstructured.Unstructured) error {
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if data["nested-added"] != "nested-contributed" {
				return fmt.Errorf("data.nested-added: want=nested-contributed got=%q", data["nested-added"])
			}
			if data["orig"] != "kept" {
				return fmt.Errorf("data.orig: want=kept got=%q", data["orig"])
			}
			return nil
		}, 15*time.Second)

		// The contribution inventory is persisted on the parent Graph.
		environment.Eventually(t, 10*time.Second, 200*time.Millisecond, func() error {
			cur := env.GetGraph(t, gKey)
			if len(cur.Status.Contributions) == 0 {
				return fmt.Errorf("patch-contributions not persisted to status")
			}
			return nil
		})

		// Remove the subgraph node from the spec. The controller releases the
		// contributed field on the next reconcile; the target object survives.
		env.UpdateGraphSpec(t, gKey, func(cur *expv1alpha1.Graph) {
			cur.Spec.Nodes = []expv1alpha1.Node{{
				ID:  "keep",
				Def: environment.RawExt(t, map[string]any{"x": 1}),
			}}
		})

		env.AwaitObject(t, cmGVK, cmKey, func(u *unstructured.Unstructured) error {
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if _, ok := data["nested-added"]; ok {
				return fmt.Errorf("data.nested-added still present after release")
			}
			if data["orig"] != "kept" {
				return fmt.Errorf("data.orig lost during release: got=%q", data["orig"])
			}
			return nil
		}, 15*time.Second)
	})
})
