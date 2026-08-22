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
	"k8s.io/apimachinery/pkg/types"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

var _ = Describe("Graph Multi-Graph Isolation", func() {
	It("isolates independent graphs watching different resources of the same GVR", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		mkGraph := func(name, childName string) *expv1alpha1.Graph {
			return &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{{
						ID: "cm",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": childName},
							"data":       map[string]any{"v": "spec"},
						}),
					}},
				},
			}
		}

		gA := env.CreateGraph(t, mkGraph("ga", "child-a"))
		gB := env.CreateGraph(t, mkGraph("gb", "child-b"))

		keyA := types.NamespacedName{Namespace: ns, Name: gA.Name}
		keyB := types.NamespacedName{Namespace: ns, Name: gB.Name}
		env.AwaitCondition(t, keyA, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)
		env.AwaitCondition(t, keyB, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// Snapshot child-a's ResourceVersion. If mutating child-b (a
		// different resource) causes graphA to reconcile spuriously, the
		// SSA apply will bump child-a's resourceVersion.
		cmAKey := types.NamespacedName{Namespace: ns, Name: "child-a"}
		preA := env.AwaitObject(t, configMapGVK, cmAKey, nil, 5*time.Second)
		originalRV := preA.GetResourceVersion()

		// Drift child-b. graphB's watcher routes the event back to graphB.
		cmBKey := types.NamespacedName{Namespace: ns, Name: "child-b"}
		cmB := env.AwaitObject(t, configMapGVK, cmBKey, nil, 5*time.Second)
		cmB = cmB.DeepCopy()
		if err := unstructured.SetNestedField(cmB.Object, "drifted", "data", "v"); err != nil {
			t.Fatalf("set drift field: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Update(ctx, cmB); err != nil {
			t.Fatalf("update child-b: %v", err)
		}

		// graphB converges its child back to spec.
		env.AwaitObject(t, configMapGVK, cmBKey, func(u *unstructured.Unstructured) error {
			v, _, _ := unstructured.NestedString(u.Object, "data", "v")
			if v != "spec" {
				return fmt.Errorf("data.v=%q want spec", v)
			}
			return nil
		}, 15*time.Second)

		// child-a's ResourceVersion must NOT have changed — graphA was
		// never woken up. Poll for a window to give any spurious wake-up
		// time to manifest. (resourceVersion bumps even on no-op SSA, so
		// this is a tight signal.)
		environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
			curA := &unstructured.Unstructured{}
			curA.SetGroupVersionKind(configMapGVK)
			if err := env.Client.Get(ctx, cmAKey, curA); err != nil {
				return err
			}
			if curA.GetResourceVersion() != originalRV {
				return fmt.Errorf("child-a resourceVersion changed (%s → %s) — graphA was reconciled spuriously",
					originalRV, curA.GetResourceVersion())
			}
			return nil
		})
	})

	It("handles contention when two graphs declare templates targeting the same resource", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		mkGraph := func(name, value string) *expv1alpha1.Graph {
			return &expv1alpha1.Graph{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
				Spec: expv1alpha1.GraphSpec{
					Nodes: []expv1alpha1.Node{{
						ID: "cm",
						Template: environment.RawExt(t, map[string]any{
							"apiVersion": "v1",
							"kind":       "ConfigMap",
							"metadata":   map[string]any{"name": "shared"},
							"data":       map[string]any{name: value},
						}),
					}},
				},
			}
		}

		env.CreateGraph(t, mkGraph("ga", "value-a"))
		env.CreateGraph(t, mkGraph("gb", "value-b"))

		// Both Graphs should reach Ready=True (each writes its own field).
		for _, name := range []string{"ga", "gb"} {
			env.AwaitCondition(t,
				types.NamespacedName{Namespace: ns, Name: name},
				expv1alpha1.GraphConditionTypeReady,
				metav1.ConditionTrue, 20*time.Second)
		}

		// The ConfigMap must carry both fields.
		env.AwaitObject(t,
			configMapGVK,
			types.NamespacedName{Namespace: ns, Name: "shared"},
			func(u *unstructured.Unstructured) error {
				data, _, _ := unstructured.NestedStringMap(u.Object, "data")
				if data["ga"] == "" && data["gb"] == "" {
					return fmt.Errorf("expected at least one of ga/gb keys, got %v", data)
				}
				return nil
			},
			15*time.Second,
		)
	})
})
