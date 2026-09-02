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

var _ = Describe("Graph Smoke", func() {
	It("boots and accepts a trivial Graph", func() {
		t := GinkgoT()
		ns := env.CreateNamespace(t)

		g := &expv1alpha1.Graph{
			ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: ns},
			Spec: expv1alpha1.GraphSpec{
				Nodes: []expv1alpha1.Node{{
					ID: "cm",
					Template: environment.RawExt(t, map[string]any{
						"apiVersion": "v1",
						"kind":       "ConfigMap",
						"metadata":   map[string]any{"name": "smoke-cm"},
						"data":       map[string]any{"hello": "world"},
					}),
				}},
			},
		}
		env.CreateGraph(t, g)

		key := types.NamespacedName{Namespace: ns, Name: "smoke"}
		env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeAccepted, metav1.ConditionTrue, 15*time.Second)
		env.AwaitCondition(t, key, expv1alpha1.GraphConditionTypeReady, metav1.ConditionTrue, 15*time.Second)

		// The ConfigMap should have been applied.
		env.AwaitObject(t,
			schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"},
			types.NamespacedName{Namespace: ns, Name: "smoke-cm"},
			func(u *unstructured.Unstructured) error {
				data, _, _ := unstructured.NestedStringMap(u.Object, "data")
				if data["hello"] != "world" {
					return errMismatch("data.hello", "world", data["hello"])
				}
				return nil
			},
			15*time.Second,
		)
	})
})

// errMismatch is a tiny helper to keep AwaitObject match closures
// readable without pulling in a full assertion framework.
func errMismatch(field, want, got string) error {
	return fmt.Errorf("%s: want=%s got=%s", field, want, got)
}
