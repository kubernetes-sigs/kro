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
	"github.com/kubernetes-sigs/kro/pkg/testutil/environment"
)

var deploymentGVK = schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}

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

	It("releases a single environment variable on one named container via a [field=value] selector, leaving the same-named variable on a sibling container and every other field kro-managed", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "extfields-selector", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "deploy",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "apps/v1",
						"kind":       "Deployment",
						"metadata":   map[string]any{"name": "extfields-deploy"},
						"spec": map[string]any{
							"replicas": int64(1),
							"selector": map[string]any{"matchLabels": map[string]any{"app": "extfields-deploy"}},
							"template": map[string]any{
								"metadata": map[string]any{"labels": map[string]any{"app": "extfields-deploy"}},
								"spec": map[string]any{
									"containers": []any{
										map[string]any{
											"name":  "app",
											"image": "nginx:alpine",
											"env": []any{
												map[string]any{"name": "FEATURE_FLAG", "value": "app-initial"},
											},
										},
										map[string]any{
											"name":  "sidecar",
											"image": "busybox",
											"env": []any{
												map[string]any{"name": "FEATURE_FLAG", "value": "sidecar-initial"},
											},
										},
									},
								},
							},
						},
					}),
					ExternalFields: []string{"spec.template.spec.containers[name=sidecar].env[name=FEATURE_FLAG].value"},
				}},
			},
		}
		env.CreateGraph(t, g)

		deployKey := types.NamespacedName{Namespace: ns, Name: "extfields-deploy"}
		env.AwaitCondition(t,
			types.NamespacedName{Namespace: ns, Name: "extfields-selector"},
			expv1alpha1.GraphConditionTypeReady,
			metav1.ConditionTrue, 15*time.Second)

		envValue := func(u *unstructured.Unstructured, container, name string) (string, bool) {
			containers, _, _ := unstructured.NestedSlice(u.Object, "spec", "template", "spec", "containers")
			for _, c := range containers {
				cm, ok := c.(map[string]any)
				if !ok || cm["name"] != container {
					continue
				}
				envs, _, _ := unstructured.NestedSlice(cm, "env")
				for _, e := range envs {
					em, ok := e.(map[string]any)
					if !ok || em["name"] != name {
						continue
					}
					v, _ := em["value"].(string)
					return v, true
				}
			}
			return "", false
		}

		dep := env.AwaitObject(t, deploymentGVK, deployKey, func(u *unstructured.Unstructured) error {
			app, _ := envValue(u, "app", "FEATURE_FLAG")
			sidecar, _ := envValue(u, "sidecar", "FEATURE_FLAG")
			if app != "app-initial" || sidecar != "sidecar-initial" {
				return fmt.Errorf("app=%q sidecar=%q, want both initial values", app, sidecar)
			}
			return nil
		}, 5*time.Second)

		// Hand-mutate both containers' FEATURE_FLAG value under a separate
		// field manager. The "app" container's variable is not listed in
		// externalFields and must be reverted (regression guard: the
		// selector must target only the exact list item named, not every
		// item sharing the same field name). The "sidecar" container's
		// variable is listed and must survive.
		ctx := env.Context()
		if ctx == nil {
			ctx = context.Background()
		}
		dep = dep.DeepCopy()
		containers, _, _ := unstructured.NestedSlice(dep.Object, "spec", "template", "spec", "containers")
		for _, c := range containers {
			cm := c.(map[string]any)
			envs, _, _ := unstructured.NestedSlice(cm, "env")
			for _, e := range envs {
				em := e.(map[string]any)
				switch cm["name"] {
				case "app":
					em["value"] = "app-drifted"
				case "sidecar":
					em["value"] = "sidecar-drifted"
				}
			}
			cm["env"] = envs
		}
		if err := unstructured.SetNestedSlice(dep.Object, containers, "spec", "template", "spec", "containers"); err != nil {
			t.Fatalf("set containers: %v", err)
		}
		if err := env.Client.Update(ctx, dep); err != nil {
			t.Fatalf("apply drift: %v", err)
		}

		env.AwaitObject(t, deploymentGVK, deployKey, func(u *unstructured.Unstructured) error {
			app, _ := envValue(u, "app", "FEATURE_FLAG")
			sidecar, _ := envValue(u, "sidecar", "FEATURE_FLAG")
			if app != "app-initial" {
				return fmt.Errorf("app=%q, want reverted to app-initial (not listed in externalFields)", app)
			}
			if sidecar != "sidecar-drifted" {
				return fmt.Errorf("sidecar=%q, want external edit preserved (sidecar-drifted)", sidecar)
			}
			return nil
		}, 15*time.Second)
	})
})
