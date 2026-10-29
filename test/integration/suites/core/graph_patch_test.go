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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	expv1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/graphengine/executor"
	"github.com/kubernetes-sigs/kro/pkg/testutil/environment"
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

	It("keeps forEach write-ahead stable through missing-target recovery and retirement", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)
		ctx := env.Context()
		names := []string{"claim-a", "claim-b", "claim-c"}
		g := env.CreateGraph(t, &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "write-ahead-patcher", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{Nodes: []expv1alpha1.Node{
				{ID: "src", Def: environment.RawExt(t, map[string]any{"names": names})},
				{
					ID:      "p",
					ForEach: []expv1alpha1.ForEachDimension{{"n": "${src.names}"}},
					Patch: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "${n}"},
						"data":       map[string]any{"added": "contributed"},
					}),
				},
			}},
		})
		key := client.ObjectKeyFromObject(g)
		fieldManager := executor.PatchFieldManager(g.UID, "p")
		want := make([]expv1alpha1.Contribution, 0, len(names))
		for _, name := range names {
			want = append(want, expv1alpha1.Contribution{
				APIVersion: "v1", Kind: "ConfigMap", Namespace: ns, Name: name,
				FieldManager: fieldManager,
			})
		}

		// Watch from creation so an intermediate ledger drop cannot hide between polls.
		graphGVR := schema.GroupVersionResource{Group: "kro.run", Version: "v1alpha1", Resource: "graphs"}
		updates, err := env.ClientSet.Dynamic().Resource(graphGVR).Namespace(ns).Watch(ctx, metav1.ListOptions{
			FieldSelector: "metadata.name=" + g.Name, ResourceVersion: g.ResourceVersion,
		})
		require.NoError(t, err)
		defer updates.Stop()
		sawIntent := false
		awaitPendingGeneration := func(generation int64) {
			t.Helper()
			timer := time.NewTimer(30 * time.Second)
			defer timer.Stop()
			for {
				select {
				case event, open := <-updates.ResultChan():
					require.True(t, open, "Graph watch closed before reconciliation")
					require.NotEqual(t, watch.Error, event.Type, "Graph watch error: %v", event.Object)
					obj, ok := event.Object.(*unstructured.Unstructured)
					require.True(t, ok, "unexpected Graph watch object: %T", event.Object)
					cur := &expv1alpha1.Graph{}
					require.NoError(t, runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, cur))
					if sawIntent || len(cur.Status.Contributions) != 0 {
						require.Equal(t, want, cur.Status.Contributions, "ledger changed at resourceVersion %s", cur.ResourceVersion)
						sawIntent = true
					}
					for _, condition := range cur.Status.Conditions {
						if string(condition.Type) != "ResourcesConverged" || condition.ObservedGeneration != generation {
							continue
						}
						require.Equal(t, metav1.ConditionFalse, condition.Status)
						require.NotNil(t, condition.Reason)
						require.Equal(t, "WaitingForReadiness", *condition.Reason)
						require.Equal(t, want, cur.Status.Contributions)
						require.Empty(t, cur.Status.ManagedResources)
						t.Logf("pending Graph %s generation=%d resourceVersion=%s ledger=%d", key, generation, cur.ResourceVersion, len(cur.Status.Contributions))
						return
					}
				case <-timer.C:
					t.Fatalf("Graph %s did not reconcile generation %d", key, generation)
				}
			}
		}
		reconcilePending := func(targets []string) {
			env.UpdateGraphSpec(t, key, func(cur *expv1alpha1.Graph) {
				// Change a Def value without changing target identities to force a new reconcile.
				cur.Spec.Nodes[0].Def = environment.RawExt(t, map[string]any{
					"names": targets, "revision": cur.Generation + 1,
				})
			})
			awaitPendingGeneration(env.GetGraph(t, key).Generation)
		}

		By("retaining all three missing targets through repeated reconciles")
		awaitPendingGeneration(g.Generation)
		reconcilePending(names)
		reconcilePending(names)

		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		uids := make(map[string]types.UID)
		createTarget := func(name string) {
			target := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": name, "namespace": ns},
				"data":     map[string]any{"orig": "kept"},
			}}
			require.NoError(t, env.Client.Create(ctx, target))
			uids[name] = target.GetUID()
			t.Cleanup(func() { _ = env.Client.Delete(context.Background(), target) })
		}
		awaitTarget := func(name string, contributed bool) {
			env.AwaitObject(t, cmGVK, types.NamespacedName{Namespace: ns, Name: name}, func(u *unstructured.Unstructured) error {
				data, _, _ := unstructured.NestedStringMap(u.Object, "data")
				if u.GetUID() != uids[name] || data["orig"] != "kept" {
					return fmt.Errorf("target %s identity or original data changed", name)
				}
				value, present := data["added"]
				if present != contributed || (contributed && value != "contributed") {
					return fmt.Errorf("target %s data.added=%q present=%t, want contributed=%t", name, value, present, contributed)
				}
				ownsFields := false
				for _, entry := range u.GetManagedFields() {
					ownsFields = ownsFields || entry.Manager == fieldManager
				}
				if ownsFields != contributed {
					return fmt.Errorf("target %s patch manager present=%t, want %t", name, ownsFields, contributed)
				}
				return nil
			}, 15*time.Second)
		}

		By("keeping the missing row when two targets are present")
		for _, name := range names[:2] {
			createTarget(name)
		}
		reconcilePending(names)
		reconcilePending(names)
		for _, name := range names[:2] {
			awaitTarget(name, true)
		}

		By("deferring retirement until the remaining targets can all be applied")
		reconcilePending(names[1:])
		reconcilePending(names[1:])
		awaitTarget(names[0], true)
		updates.Stop()

		By("recovering when the final target appears and releasing the retired target")
		createTarget(names[2])
		env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 60*time.Second)
		cur := env.GetGraph(t, key)
		require.Equal(t, want[1:], cur.Status.Contributions)
		t.Logf("recovered Graph %s generation=%d resourceVersion=%s ledger=%d", key, cur.Generation, cur.ResourceVersion, len(cur.Status.Contributions))
		awaitTarget(names[0], false)
		for _, name := range names[1:] {
			awaitTarget(name, true)
		}

		By("releasing the remaining fields on deletion while preserving every target")
		require.NoError(t, env.Client.Delete(ctx, cur))
		env.AwaitGraphGone(t, key, 30*time.Second)
		require.True(t, apierrors.IsNotFound(env.Client.Get(ctx, key, &expv1alpha1.Graph{})))
		for _, name := range names {
			awaitTarget(name, false)
		}
		t.Logf("deleted Graph %s; all three original target UIDs/data preserved and patch managers released", key)
	})

	// A main-resource patch is cooperative: a contributed field already owned by
	// a foreign field manager is refused and reported as
	// ResourcesConverged=False/FieldManagerConflict naming the target and the
	// manager; the field keeps the foreign value until that manager releases it.
	It("reports a patch field owned by a foreign manager as FieldManagerConflict and converges once released", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}

		cmGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}
		cmKey := types.NamespacedName{Namespace: ns, Name: "app-config"}
		const foreignManager = "kubectl-client-side-apply"

		// A kubectl-like manager owns data.logLevel via server-side apply.
		foreignApply := func(data map[string]any) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata":   map[string]any{"name": cmKey.Name, "namespace": ns},
				"data":       data,
			}}
			if err := env.Client.Patch(ctx, obj, client.Apply, client.FieldOwner(foreignManager)); err != nil {
				t.Fatalf("foreign manager apply: %v", err)
			}
		}
		foreignApply(map[string]any{"logLevel": "info", "other": "kept"})

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "loglevel-patcher", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "p",
					Patch: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": cmKey.Name},
						"data":       map[string]any{"logLevel": "debug"},
					}),
				}},
			},
		}
		env.CreateGraph(t, g)
		gKey := types.NamespacedName{Namespace: ns, Name: "loglevel-patcher"}

		// The Graph compiles (Accepted=True) but does not converge: the conflict
		// is reported under its own reason, and Ready rolls up to False.
		env.AwaitCondition(t, gKey, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 20*time.Second)
		environment.Eventually(t, 20*time.Second, 200*time.Millisecond, func() error {
			cur := env.GetGraph(t, gKey)
			var conv *expv1alpha1.Condition
			for i := range cur.Status.Conditions {
				if string(cur.Status.Conditions[i].Type) == "ResourcesConverged" {
					conv = &cur.Status.Conditions[i]
				}
			}
			if conv == nil || conv.Status != metav1.ConditionFalse {
				return fmt.Errorf("ResourcesConverged not False yet: %+v", conv)
			}
			if conv.Reason == nil || *conv.Reason != "FieldManagerConflict" {
				return fmt.Errorf("ResourcesConverged reason: want FieldManagerConflict, got %+v", conv)
			}
			msg := ""
			if conv.Message != nil {
				msg = *conv.Message
			}
			for _, want := range []string{cmKey.Name, foreignManager, "logLevel"} {
				if !strings.Contains(msg, want) {
					return fmt.Errorf("ResourcesConverged message %q does not name %q", msg, want)
				}
			}
			return nil
		})
		env.AwaitCondition(t, gKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionFalse, 10*time.Second)

		// The foreign owner's value is untouched: kro did not steal the field,
		// and no kro patch manager appears on the object.
		environment.Consistently(t, 2*time.Second, 200*time.Millisecond, func() error {
			cur := &unstructured.Unstructured{}
			cur.SetGroupVersionKind(cmGVK)
			if err := env.Client.Get(ctx, cmKey, cur); err != nil {
				return err
			}
			data, _, _ := unstructured.NestedStringMap(cur.Object, "data")
			if data["logLevel"] != "info" {
				return fmt.Errorf("data.logLevel=%q — the foreign owner's value was overwritten", data["logLevel"])
			}
			for _, mf := range cur.GetManagedFields() {
				if strings.HasPrefix(mf.Manager, "kro-graphengine.patch.") {
					return fmt.Errorf("a kro patch manager %q landed on the contested object", mf.Manager)
				}
			}
			return nil
		})

		// The foreign manager lets go of data.logLevel (re-applies without it).
		// SSA drops the field from that manager's set, and the Graph's next
		// requeue lands the contribution and converges.
		foreignApply(map[string]any{"other": "kept"})

		env.AwaitCondition(t, gKey, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 60*time.Second)
		env.AwaitObject(t, cmGVK, cmKey, func(u *unstructured.Unstructured) error {
			data, _, _ := unstructured.NestedStringMap(u.Object, "data")
			if data["logLevel"] != "debug" {
				return fmt.Errorf("data.logLevel: want=debug got=%q", data["logLevel"])
			}
			if data["other"] != "kept" {
				return fmt.Errorf("data.other: want=kept got=%q", data["other"])
			}
			return nil
		}, 15*time.Second)
	})
})
