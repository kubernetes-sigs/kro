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
	"github.com/kubernetes-sigs/kro/pkg/testutil/environment"
)

var _ = Describe("External Fields", func() {
	It("sets the initial value on create, then releases ownership so external edits survive while unlisted fields still revert", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "extfields", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "extfields-cm"},
						"data": map[string]any{
							"owned":    "initial-owned",
							"released": "initial-released",
						},
					}),
					ExternalFields: []string{"data.released"},
				}},
			},
		}
		env.CreateGraph(t, g)

		cmKey := types.NamespacedName{Namespace: ns, Name: "extfields-cm"}
		env.AwaitCondition(t,
			types.NamespacedName{Namespace: ns, Name: "extfields"},
			expv1alpha1.GraphConditionTypeReady,
			metav1.ConditionTrue, 15*time.Second)

		cm := env.AwaitObject(t, configMapGVK, cmKey, func(u *unstructured.Unstructured) error {
			owned, _, _ := unstructured.NestedString(u.Object, "data", "owned")
			released, _, _ := unstructured.NestedString(u.Object, "data", "released")
			if owned != "initial-owned" || released != "initial-released" {
				return fmt.Errorf("data=%v, want initial values on both keys", u.Object["data"])
			}
			return nil
		}, 5*time.Second)

		// Hand-mutate both keys under a separate field manager. "owned" is
		// still fully kro-managed and must be reverted on the next
		// reconcile (regression guard: externalFields must not become a
		// global opt-out). "released" is listed in externalFields, so kro
		// must have stopped claiming it on the update apply — the mutation
		// should survive.
		cm = cm.DeepCopy()
		if err := unstructured.SetNestedField(cm.Object, "drifted-owned", "data", "owned"); err != nil {
			t.Fatalf("set owned field: %v", err)
		}
		if err := unstructured.SetNestedField(cm.Object, "drifted-released", "data", "released"); err != nil {
			t.Fatalf("set released field: %v", err)
		}
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		if err := env.Client.Update(ctx, cm); err != nil {
			t.Fatalf("apply drift: %v", err)
		}

		env.AwaitObject(t, configMapGVK, cmKey, func(u *unstructured.Unstructured) error {
			owned, _, _ := unstructured.NestedString(u.Object, "data", "owned")
			released, _, _ := unstructured.NestedString(u.Object, "data", "released")
			if owned != "initial-owned" {
				return fmt.Errorf("data.owned=%q, want reverted to initial-owned", owned)
			}
			if released != "drifted-released" {
				return fmt.Errorf("data.released=%q, want external edit preserved (drifted-released)", released)
			}
			return nil
		}, 15*time.Second)

		// Deleting the Graph must still remove the whole ConfigMap, even
		// though one of its fields was released from kro's ownership.
		if err := env.Client.Delete(ctx, g); err != nil {
			t.Fatalf("delete graph: %v", err)
		}
		env.AwaitDeleted(t, configMapGVK, cmKey, 15*time.Second)
	})
})
