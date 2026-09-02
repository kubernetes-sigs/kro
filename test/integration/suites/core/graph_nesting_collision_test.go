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
	"k8s.io/apimachinery/pkg/util/validation"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/test/integration/environment"
)

// This suite reproduces the sibling-subgraph node-id collision: two subgraph
// nodes (subA, subB) each declare a node with the SAME local id "res". Their
// managed resources are distinct (collide-a, collide-b) with node paths
// subA/res and subB/res.
//
// The identity metadata must stay consistent and unambiguous across the three
// sinks that a change to any one of them would otherwise desynchronize:
//   - ManagedResource.NodeID in Graph status: the readable '/'-form path.
//   - the kro.run/node-id LABEL: a bounded, label-safe token ('.'-form here).
//   - the internal.kro.run/node-path ANNOTATION: the full readable '/'-form.
//
// and, behaviorally, that each subgraph's drift watch is routed to ITS OWN
// items and does not cross-match the sibling's (the watch selector is keyed on
// the same token as the label).
var _ = Describe("Graph Nesting NodeID Collision", func() {
	subgraphNode := func(t environment.TestingT, id, cmName string) expv1alpha1.Node {
		return expv1alpha1.Node{
			ID: id,
			Graph: environment.RawExt(t, map[string]any{
				"nodes": []any{
					map[string]any{
						"id": "res",
						"template": map[string]any{
							"apiVersion": "v1", "kind": "ConfigMap",
							"metadata": map[string]any{"name": cmName},
							"data":     map[string]any{"owner": id},
						},
					},
				},
			}),
		}
	}

	It("qualifies node-id label/annotation and isolates drift per subgraph", func(ctx SpecContext) {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "collide", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				subgraphNode(t, "subA", "collide-a"),
				subgraphNode(t, "subB", "collide-b"),
			}},
		}
		env.CreateGraph(t, g)

		key := types.NamespacedName{Namespace: ns, Name: "collide"}
		env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 20*time.Second)

		aKey := types.NamespacedName{Namespace: ns, Name: "collide-a"}
		bKey := types.NamespacedName{Namespace: ns, Name: "collide-b"}
		env.AwaitObject(t, configMapGVK, aKey, nil, 15*time.Second)
		env.AwaitObject(t, configMapGVK, bKey, nil, 15*time.Second)

		// --- Store: each child is tracked under its frame-qualified '/'-form. ---
		got := env.GetGraph(t, key)
		wantPaths := map[string]string{"collide-a": "subA/res", "collide-b": "subB/res"}
		seen := map[string]string{}
		for _, mr := range got.Status.ManagedResources {
			if p, ok := wantPaths[mr.Name]; ok {
				seen[mr.Name] = mr.NodeID
				if mr.NodeID != p {
					t.Fatalf("managed resource %q: NodeID = %q, want %q", mr.Name, mr.NodeID, p)
				}
			}
		}
		for name, path := range wantPaths {
			if seen[name] == "" {
				t.Fatalf("managed resource %q (path %q) missing from status: %+v", name, path, got.Status.ManagedResources)
			}
		}

		// --- Label + annotation: the two children are distinguishable. ---
		assertNodeMeta := func(cm *unstructured.Unstructured, wantLabel, wantPath string) error {
			gotLabel := cm.GetLabels()[metadata.NodeIDLabel]
			if gotLabel != wantLabel {
				return errMismatch(metadata.NodeIDLabel, wantLabel, gotLabel)
			}
			if errs := validation.IsValidLabelValue(gotLabel); len(errs) > 0 {
				return fmt.Errorf("node-id label %q is not a valid label value: %v", gotLabel, errs)
			}
			gotPath := cm.GetAnnotations()[metadata.NodePathAnnotation]
			if gotPath != wantPath {
				return errMismatch(metadata.NodePathAnnotation, wantPath, gotPath)
			}
			return nil
		}
		env.AwaitObject(t, configMapGVK, aKey, func(u *unstructured.Unstructured) error {
			return assertNodeMeta(u, "subA.res", "subA/res")
		}, 15*time.Second)
		env.AwaitObject(t, configMapGVK, bKey, func(u *unstructured.Unstructured) error {
			return assertNodeMeta(u, "subB.res", "subB/res")
		}, 15*time.Second)

		// --- Behavioral: drift on subB's item is restored by subB's OWN watch. ---
		// The Graph spec is unchanged, so only the dynamic drift watch can drive
		// the next reconcile. If subB's watch were shadowed by subA's colliding
		// selector (the pre-fix bug), the deletion could be misrouted and the
		// object might not come back correctly.
		cmB := env.AwaitObject(t, configMapGVK, bKey, nil, 5*time.Second)
		originalUID := cmB.GetUID()

		reconcileCtx := env.Context()
		if reconcileCtx == nil {
			reconcileCtx = context.Background()
		}
		if err := env.Client.Delete(reconcileCtx, cmB); err != nil {
			t.Fatalf("delete collide-b: %v", err)
		}

		// subB's item comes back (new UID) and still carries subB's identity —
		// proving the restore came from subB's watch, not a cross-matched subA.
		env.AwaitObject(t, configMapGVK, bKey, func(u *unstructured.Unstructured) error {
			if u.GetUID() == originalUID {
				return fmt.Errorf("UID unchanged — collide-b was not rebuilt")
			}
			return assertNodeMeta(u, "subB.res", "subB/res")
		}, 20*time.Second)

		// subA's item was never touched: its identity is intact and it was not
		// disturbed by subB's drift event.
		env.AwaitObject(t, configMapGVK, aKey, func(u *unstructured.Unstructured) error {
			return assertNodeMeta(u, "subA.res", "subA/res")
		}, 5*time.Second)
	})
})
