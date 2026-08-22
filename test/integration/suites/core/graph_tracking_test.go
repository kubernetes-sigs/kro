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

var _ = Describe("Graph Tracking", func() {
	It("records applied resources in status.ManagedResources", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "track-basic", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{
					{
						ID: "cm",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "tracked-cm"},
							"data":       map[string]any{"k": "v"},
						}),
					},
				},
			},
		}
		env.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "track-basic"}
		env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
			got := env.GetGraph(t, graphKey)
			if len(got.Status.ManagedResources) != 1 {
				return fmt.Errorf("status.managedResources len=%d want 1", len(got.Status.ManagedResources))
			}
			entry := got.Status.ManagedResources[0]
			if entry.NodeID != "cm" {
				return fmt.Errorf("nodeID=%q want cm", entry.NodeID)
			}
			if entry.APIVersion != "v1" || entry.Kind != "ConfigMap" {
				return fmt.Errorf("gvk=%s/%s want v1/ConfigMap", entry.APIVersion, entry.Kind)
			}
			if entry.Namespace != ns || entry.Name != "tracked-cm" {
				return fmt.Errorf("identity=%s/%s want %s/tracked-cm", entry.Namespace, entry.Name, ns)
			}
			if entry.UID == "" {
				return fmt.Errorf("UID empty; SSA response should have populated it")
			}
			return nil
		})
	})

	It("prunes old resource on spec change (rename)", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		mk := func(cmName string) *expv1alpha1.Graph {
			return &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: "prune-rename", Namespace: ns},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{{
						ID: "cm",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": cmName},
							"data":       map[string]any{"k": "v"},
						}),
					}},
				},
			}
		}

		env.CreateGraph(t, mk("first-name"))

		graphKey := types.NamespacedName{Namespace: ns, Name: "prune-rename"}
		oldKey := types.NamespacedName{Namespace: ns, Name: "first-name"}
		newKey := types.NamespacedName{Namespace: ns, Name: "second-name"}

		env.AwaitObject(t, configMapGVK, oldKey, nil, 15*time.Second)
		env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// Rename the template.
		env.UpdateGraphSpec(t, graphKey, func(g *expv1alpha1.Graph) {
			g.Spec.Nodes[0].Template = environment.RawExt(t, map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": "second-name"},
				"data":       map[string]any{"k": "v"},
			})
		})

		env.AwaitObject(t, configMapGVK, newKey, nil, 15*time.Second)
		env.AwaitDeleted(t, configMapGVK, oldKey, 15*time.Second)

		environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
			got := env.GetGraph(t, graphKey)
			if len(got.Status.ManagedResources) != 1 {
				return fmt.Errorf("status.managedResources len=%d want 1 (the new entry)", len(got.Status.ManagedResources))
			}
			if got.Status.ManagedResources[0].Name != "second-name" {
				return fmt.Errorf("tracking name=%q want second-name", got.Status.ManagedResources[0].Name)
			}
			return nil
		})
	})

	It("prunes resource when includeWhen flips to false", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "prune-flip", Namespace: ns},
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
		env.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "prune-flip"}
		cmKey := types.NamespacedName{Namespace: ns, Name: "flip-cm"}
		env.AwaitObject(t, configMapGVK, cmKey, nil, 15*time.Second)

		// Flip the flag — node becomes ignored, its previous tracking
		// entry becomes a prune candidate, the cm should be deleted.
		env.UpdateGraphSpec(t, graphKey, func(g *expv1alpha1.Graph) {
			g.Spec.Nodes[0].Def = environment.RawExt(t, map[string]any{"enabled": false})
		})

		env.AwaitDeleted(t, configMapGVK, cmKey, 15*time.Second)

		environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
			got := env.GetGraph(t, graphKey)
			if len(got.Status.ManagedResources) != 0 {
				return fmt.Errorf("status.managedResources len=%d want 0 after includeWhen flip", len(got.Status.ManagedResources))
			}
			return nil
		})
	})

	It("prunes dropped items on forEach shrink", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		mk := func(items []string) *expv1alpha1.Graph {
			return &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: "prune-shrink", Namespace: ns},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{
						{
							ID:  "src",
							Def: environment.RawExt(t, map[string]any{"items": items}),
						},
						{
							ID:      "cms",
							ForEach: []expv1alpha1.ForEachDimension{{"label": "${src.items}"}},
							Template: environment.RawExt(t, map[string]any{
								"apiVersion": "v1",
								"kind":       "ConfigMap",
								"metadata":   map[string]any{"name": "${label}"},
							}),
						},
					},
				},
			}
		}

		env.CreateGraph(t, mk([]string{"alpha", "beta", "gamma"}))

		graphKey := types.NamespacedName{Namespace: ns, Name: "prune-shrink"}
		for _, name := range []string{"alpha", "beta", "gamma"} {
			env.AwaitObject(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: name}, nil, 15*time.Second)
		}

		// Shrink: drop gamma.
		env.UpdateGraphSpec(t, graphKey, func(g *expv1alpha1.Graph) {
			g.Spec.Nodes[0].Def = environment.RawExt(t, map[string]any{"items": []string{"alpha", "beta"}})
		})

		env.AwaitDeleted(t, configMapGVK, types.NamespacedName{Namespace: ns, Name: "gamma"}, 15*time.Second)

		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		// alpha and beta should remain.
		environment.Consistently(t, 1500*time.Millisecond, 100*time.Millisecond, func() error {
			for _, name := range []string{"alpha", "beta"} {
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(configMapGVK)
				if err := env.Client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, obj); err != nil {
					return fmt.Errorf("%s should remain: %w", name, err)
				}
			}
			return nil
		})

		environment.Eventually(t, 10*time.Second, 100*time.Millisecond, func() error {
			got := env.GetGraph(t, graphKey)
			if len(got.Status.ManagedResources) != 2 {
				return fmt.Errorf("status.managedResources len=%d want 2 after shrink", len(got.Status.ManagedResources))
			}
			return nil
		})
	})

	It("uses tracking record instead of resolve on delete", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "delete-rename", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "original"},
					}),
				}},
			},
		}
		env.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "delete-rename"}
		origKey := types.NamespacedName{Namespace: ns, Name: "original"}
		env.AwaitObject(t, configMapGVK, origKey, nil, 15*time.Second)
		env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// Now delete the Graph. The original CM must be removed.
		got := env.GetGraph(t, graphKey)
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Delete(ctx, got); err != nil {
			t.Fatalf("delete graph: %v", err)
		}
		env.AwaitDeleted(t, configMapGVK, origKey, 15*time.Second)
		env.AwaitGraphGone(t, graphKey, 15*time.Second)
	})

	It("preserves tracked resources across clean reconciles", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "preserve", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "preserved-cm"},
						"data":       map[string]any{"k": "v"},
					}),
				}},
			},
		}
		env.CreateGraph(t, g)

		graphKey := types.NamespacedName{Namespace: ns, Name: "preserve"}
		cmKey := types.NamespacedName{Namespace: ns, Name: "preserved-cm"}
		env.AwaitObject(t, configMapGVK, cmKey, nil, 15*time.Second)
		env.AwaitCondition(t, graphKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
			got := env.GetGraph(t, graphKey)
			if len(got.Status.ManagedResources) != 1 {
				return fmt.Errorf("status churn: managedResources len=%d", len(got.Status.ManagedResources))
			}
			obj := &unstructured.Unstructured{}
			obj.SetGroupVersionKind(configMapGVK)
			err := env.Client.Get(ctx, cmKey, obj)
			if err != nil {
				if apierrors.IsNotFound(err) {
					return fmt.Errorf("preserved CM disappeared")
				}
				return err
			}
			return nil
		})
	})
})
