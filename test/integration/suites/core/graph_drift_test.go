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
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

func getDriftRouterEnv(t environment.TestingT) *environment.Environment {
	if env != nil && env.Router != nil {
		return env
	}
	testEnv, err := environment.New(context.Background(), environment.ControllerConfig{
		AllowCRDDeletion: true,
		LogWriter:        GinkgoWriter,
	})
	if err != nil {
		t.Fatalf("failed to create isolated env: %v", err)
	}
	t.Cleanup(func() { _ = testEnv.Stop() })
	return testEnv
}

var _ = Describe("Graph Drift", func() {
	It("recreates deleted child solely via watch event", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "drift", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "drift-cm"},
						"data":       map[string]any{"v": "stable"},
					}),
				}},
			},
		}
		env.CreateGraph(t, g)

		cmKey := types.NamespacedName{Namespace: ns, Name: "drift-cm"}
		env.AwaitObject(t, configMapGVK, cmKey, nil, 15*time.Second)
		env.AwaitCondition(t,
			types.NamespacedName{Namespace: ns, Name: "drift"},
			expv1alpha1.GraphConditionTypeReady,
			metav1.ConditionTrue, 15*time.Second)

		// Snapshot the original UID — recreated ConfigMap will have a new
		// one so we can prove the object actually got rebuilt, not merely
		// re-fetched.
		cm := env.AwaitObject(t, configMapGVK, cmKey, nil, 5*time.Second)
		originalUID := cm.GetUID()

		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		// Delete the child. The Graph hasn't changed — only the dynamic
		// controller can drive the next reconcile.
		if err := env.Client.Delete(ctx, cm); err != nil {
			t.Fatalf("delete child ConfigMap: %v", err)
		}

		// Wait for the new ConfigMap to come back.
		env.AwaitObject(t, configMapGVK, cmKey, func(u *unstructured.Unstructured) error {
			if u.GetUID() == originalUID {
				return fmt.Errorf("UID unchanged — same object")
			}
			return nil
		}, 15*time.Second)
	})

	It("restores mutated field via SSA conflict detection", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "mutate", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "mutate-cm"},
						"data":       map[string]any{"v": "spec"},
					}),
				}},
			},
		}
		env.CreateGraph(t, g)

		cmKey := types.NamespacedName{Namespace: ns, Name: "mutate-cm"}
		env.AwaitCondition(t,
			types.NamespacedName{Namespace: ns, Name: "mutate"},
			expv1alpha1.GraphConditionTypeReady,
			metav1.ConditionTrue, 15*time.Second)

		cm := env.AwaitObject(t, configMapGVK, cmKey, nil, 5*time.Second)

		// Hand-mutate data.v to "drifted" using a separate field manager
		// so SSA recognizes the conflict and restores the kro value.
		cm = cm.DeepCopy()
		if err := unstructured.SetNestedField(cm.Object, "drifted", "data", "v"); err != nil {
			t.Fatalf("set drift field: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Update(ctx, cm); err != nil {
			t.Fatalf("apply drift: %v", err)
		}

		env.AwaitObject(t, configMapGVK, cmKey, func(u *unstructured.Unstructured) error {
			v, _, _ := unstructured.NestedString(u.Object, "data", "v")
			if v != "spec" {
				return fmt.Errorf("data.v=%q want spec (still drifted)", v)
			}
			return nil
		}, 15*time.Second)
	})

	It("matches watch coordinator tracking to Graph lifecycle", func() {
		t := GinkgoT()
		testEnv := getDriftRouterEnv(t)
		ns := testEnv.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "track", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID: "cm1",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "track-1"},
						}),
					},
					{
						ID: "cm2",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "track-2"},
						}),
					},
				},
			},
		}
		testEnv.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "track"}
		testEnv.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// Both ConfigMaps must show up in status.managedResources.
		got := testEnv.GetGraph(t, graphKey)
		if len(got.Status.ManagedResources) != 2 {
			t.Fatalf("expected 2 managed resources in status, got %d", len(got.Status.ManagedResources))
		}
		names := map[string]bool{}
		for _, mr := range got.Status.ManagedResources {
			names[mr.Name] = true
		}
		if !names["track-1"] || !names["track-2"] {
			t.Fatalf("expected track-1 and track-2 in managedResources, got %v", got.Status.ManagedResources)
		}

		// Both ConfigMaps must show up as scalar watches.
		environment.Eventually(t, 5*time.Second, 100*time.Millisecond, func() error {
			scalar, _ := testEnv.Router.Coordinator().WatchRequestCount()
			if scalar < 2 {
				return fmt.Errorf("scalar watch count=%d want >=2", scalar)
			}
			return nil
		})

		// Delete the Graph — the coordinator must release all of its
		// watches for this Graph.
		ctx := testEnv.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := testEnv.Client.Delete(ctx, got); err != nil {
			t.Fatalf("delete graph: %v", err)
		}
		testEnv.AwaitGraphGone(t, graphKey, 15*time.Second)

		environment.Eventually(t, 5*time.Second, 100*time.Millisecond, func() error {
			scalar, collection := testEnv.Router.Coordinator().WatchRequestCount()
			if scalar != 0 || collection != 0 {
				return fmt.Errorf("active watch count scalar=%d collection=%d want 0", scalar, collection)
			}
			if graphs := testEnv.Router.Coordinator().GraphCount(); graphs != 0 {
				return fmt.Errorf("coordinator graph count=%d want 0", graphs)
			}
			return nil
		})
	})

	It("registers watches across not-ready nodes", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "walkall", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID: "a",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "walkall-a"},
							"data":       map[string]any{"k": "v"},
						}),
						// Never satisfied — exercises the soft-error
						// continuation path in the executor.
						ReadyWhen: []string{`${a.data.k == "never"}`},
					},
					{
						ID: "b",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "walkall-b"},
							"data":       map[string]any{"upstream": "${a.metadata.name}"},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		// Both should materialize even though a is not-ready — apply runs
		// before readyWhen and the executor doesn't bail on soft errors.
		bKey := types.NamespacedName{Namespace: ns, Name: "walkall-b"}
		env.AwaitObject(t, configMapGVK, bKey, nil, 15*time.Second)

		// b should be in the coordinator's scalar index — drift it and
		// confirm restoration.
		cmB := env.AwaitObject(t, configMapGVK, bKey, nil, 5*time.Second).DeepCopy()
		if err := unstructured.SetNestedField(cmB.Object, "drifted", "data", "upstream"); err != nil {
			t.Fatalf("set drift field: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Update(ctx, cmB); err != nil {
			t.Fatalf("drift b: %v", err)
		}

		env.AwaitObject(t, configMapGVK, bKey, func(u *unstructured.Unstructured) error {
			v, _, _ := unstructured.NestedString(u.Object, "data", "upstream")
			if v != "walkall-a" {
				return fmt.Errorf("data.upstream=%q want walkall-a", v)
			}
			return nil
		}, 15*time.Second)
	})

	It("drops drift watch after includeWhen flips to false", func() {
		t := GinkgoT()
		testEnv := getDriftRouterEnv(t)
		ns := testEnv.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "flip", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID:  "flag",
						Def: environment.RawExt(t, map[string]any{"enabled": true}),
					},
					{
						ID:          "cm",
						IncludeWhen: []string{"${flag.enabled}"},
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "flip-cm"},
						}),
					},
				},
			},
		}
		testEnv.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "flip"}
		cmKey := types.NamespacedName{Namespace: ns, Name: "flip-cm"}
		testEnv.AwaitObject(t, configMapGVK, cmKey, nil, 15*time.Second)

		// Watch should be present.
		environment.Eventually(t, 5*time.Second, 100*time.Millisecond, func() error {
			scalar, _ := testEnv.Router.Coordinator().WatchRequestCount()
			if scalar < 1 {
				return fmt.Errorf("expected at least one scalar watch")
			}
			return nil
		})

		// Flip includeWhen to false by setting flag.enabled=false.
		testEnv.UpdateGraphSpec(t, graphKey, func(g *expv1alpha1.Graph) {
			g.Spec.Nodes[0].Def = environment.RawExt(t, map[string]any{"enabled": false})
		})

		// The cm node is now ignored, so the executor never declares a
		// watch for it on the next reconcile cycle.
		environment.Eventually(t, 10*time.Second, 200*time.Millisecond, func() error {
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(configMapGVK)
			ctx := testEnv.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			err := testEnv.Client.Get(ctx, cmKey, obj)
			if apierrors.IsNotFound(err) {
				// Acceptable — graph cleanup pruned it.
				return nil
			}
			return nil
		})
	})
})
