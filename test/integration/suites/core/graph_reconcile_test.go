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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var configMapGVK = schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

var _ = Describe("Graph Reconcile", func() {
	It("applies, updates, and deletes resources cleanly with finalizer management", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "rec", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "rec-cm"},
						"data":       map[string]any{"v": "initial"},
					}),
				}},
			},
		}
		env.CreateGraph(t, g)
		key := types.NamespacedName{Namespace: ns, Name: "rec"}

		// Wait for the child to materialize.
		env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "rec-cm"}, func(u *unstructured.Unstructured) error {
			v, _, _ := unstructured.NestedString(u.Object, "data", "v")
			if v != "initial" {
				return fmt.Errorf("data.v=%q want initial", v)
			}
			return nil
		}, 15*time.Second)

		// Finalizer must be on the Graph by now (managed).
		got := env.GetGraph(t, key)
		if !metadata.HasGraphFinalizer(got) {
			t.Fatalf("expected finalizer %q on managed Graph; got %v", metadata.GraphFinalizer, got.Finalizers)
		}

		// Update the spec — same template name, new value — and wait for
		// the ConfigMap data to reflect it. UpdateGraphSpec retries on
		// 409s the controller's concurrent status writes can produce.
		env.UpdateGraphSpec(t, key, func(g *expv1alpha1.Graph) {
			g.Spec.Nodes[0].Template = environment.RawExt(t, map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "rec-cm"},
				"data":       map[string]any{"v": "updated"},
			})
		})
		env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "rec-cm"}, func(u *unstructured.Unstructured) error {
			v, _, _ := unstructured.NestedString(u.Object, "data", "v")
			if v != "updated" {
				return fmt.Errorf("data.v=%q want updated", v)
			}
			return nil
		}, 15*time.Second)

		// Delete the graph. ConfigMap should be torn down; Graph object
		// should be gone (no finalizer hold). Re-fetch to get the latest
		// RV — the spec update above made `got` stale.
		got = env.GetGraph(t, key)
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Delete(ctx, got); err != nil {
			t.Fatalf("delete graph: %v", err)
		}
		env.AwaitDeleted(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "rec-cm"}, 15*time.Second)
		env.AwaitGraphGone(t, key, 15*time.Second)
	})

	It("honors topological dependency order during apply", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "dep", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID:  "base",
						Def: environment.RawExt(t, map[string]any{"suffix": "child"}),
					},
					{
						ID: "parent",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "parent"},
							"data":       map[string]any{"who": "parent"},
						}),
					},
					{
						ID: "child",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "${base.suffix}"},
							"data":       map[string]any{"parentName": "${parent.metadata.name}"},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "child"}, func(u *unstructured.Unstructured) error {
			v, _, _ := unstructured.NestedString(u.Object, "data", "parentName")
			if v != "parent" {
				return fmt.Errorf("data.parentName=%q want parent", v)
			}
			return nil
		}, 15*time.Second)
	})

	It("skips nodes when includeWhen evaluates to false", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "incl", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID:  "flag",
						Def: environment.RawExt(t, map[string]any{"enabled": false}),
					},
					{
						ID:          "skipped",
						IncludeWhen: []string{"${flag.enabled}"},
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "skipped-cm"},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		// Wait for Accepted to confirm reconcile ran.
		env.AwaitCondition(t,
			types.NamespacedName{Namespace: ns, Name: "incl"},
			expv1alpha1.GraphConditionTypeAccepted,
			metav1.ConditionTrue, 15*time.Second)

		// Consistently assert the ConfigMap is not created.
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		environment.Consistently(t, 1500*time.Millisecond, 100*time.Millisecond, func() error {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(configMapGVK)
			err := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "skipped-cm"}, obj)
			if err == nil {
				return fmt.Errorf("ConfigMap should not exist: %v", obj.GetName())
			}
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("unexpected error: %w", err)
			}
			return nil
		})
	})

	It("satisfies readyWhen to make Graph Ready", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "ready", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "ready-cm"},
						"data":       map[string]any{"k": "v"},
					}),
					// data.k is "v" — readyWhen reads it back from observed.
					ReadyWhen: []string{`${cm.data.k == "v"}`},
				}},
			},
		}
		env.CreateGraph(t, g)

		env.AwaitCondition(t,
			types.NamespacedName{Namespace: ns, Name: "ready"},
			expv1alpha1.GraphConditionTypeReady,
			metav1.ConditionTrue, 15*time.Second)
	})
})
