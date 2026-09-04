// Copyright 2025 The Kubernetes Authors.
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
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// configuredMaxCollectionSize mirrors the RGDConfig the integration environment
// installs (see test/integration/environment/setup.go). The collection cap is
// an operator-facing setting, so the value the controller enforces has to be
// the value it was given rather than a built-in default.
const configuredMaxCollectionSize = 1000

// Collection behavior that the existing forEach coverage does not reach.
//
//   - The configured collection cap is what gets enforced. Operators raise or
//     lower it deliberately; silently enforcing a different number makes a
//     tuned deployment behave unlike its configuration, and the effective limit
//     can end up lower than what was asked for.
//   - Drift correction has to cover every namespace a collection spans. A
//     collection can template items into different namespaces, and an item is
//     no less managed for living in the second one. If only part of a
//     collection is watched, the rest drifts unnoticed while the instance keeps
//     reporting that everything is ready.
var _ = Describe("CollectionBehavior", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("enforces the configured collection size limit", func(ctx SpecContext) {
		// 34 x 34 = 1156 items, above the configured cap. Nothing should be
		// created, and the instance should say which limit it hit.
		rgd := generator.NewResourceGraphDefinition("test-collection-cap",
			generator.WithSchema(
				"TestCollectionCap", "v1alpha1",
				map[string]any{
					"name": "string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name": "${schema.spec.name}-${string(i)}-${string(j)}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"i": "${lists.range(34)}"},
					{"j": "${lists.range(34)}"},
				},
				nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "collection-cap"
		instance := newInstance("TestCollectionCap", name, namespace, map[string]any{
			"name": name,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)).To(Succeed())

			conds := instanceConditions(instance)
			g.Expect(conds).To(ContainSubstring(fmt.Sprintf("%d", configuredMaxCollectionSize)),
				"the instance should report the configured collection limit of %d; conditions: %s",
				configuredMaxCollectionSize, conds)
		}, 60*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())

		state, _, err := unstructured.NestedString(instance.Object, "status", "state")
		Expect(err).ToNot(HaveOccurred())
		Expect(state).ToNot(Equal("ACTIVE"),
			"an instance whose collection exceeds the cap must not report ACTIVE")
	})

	It("corrects drift on collection items in every namespace the collection spans", func(ctx SpecContext) {
		altNamespace := fmt.Sprintf("alt-%s", namespace)
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: altNamespace},
		})).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: altNamespace},
			})
		})

		// Item 0 lands in the instance's namespace, item 1 in the other one.
		rgd := generator.NewResourceGraphDefinition("test-collection-multi-ns-drift",
			generator.WithSchema(
				"TestCollectionMultiNsDrift", "v1alpha1",
				map[string]any{
					"name": "string",
					"ns1":  "string",
					"ns2":  "string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]any{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]any{
					"name":      "${schema.spec.name}-item-${string(i)}",
					"namespace": "${i == 0 ? schema.spec.ns1 : schema.spec.ns2}",
				},
				"data": map[string]any{
					"managed": "expected",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"i": "${lists.range(2)}"},
				},
				nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		name := "multi-ns-drift"
		instance := newInstance("TestCollectionMultiNsDrift", name, namespace, map[string]any{
			"name": name,
			"ns1":  namespace,
			"ns2":  altNamespace,
		})
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		waitForInstanceState(ctx, instance, name, namespace, "ACTIVE")

		items := map[string]string{
			namespace:    name + "-item-0",
			altNamespace: name + "-item-1",
		}
		for ns, cmName := range items {
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{
					Name:      cmName,
					Namespace: ns,
				}, cm)).To(Succeed())
				g.Expect(cm.Data).To(HaveKeyWithValue("managed", "expected"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Tamper with an item in each namespace in turn and expect both to be
		// restored. The namespace an item happens to live in must not decide
		// whether kro notices.
		for ns, cmName := range items {
			By(fmt.Sprintf("restoring drift on %s/%s", ns, cmName))

			cm := &corev1.ConfigMap{}
			Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      cmName,
				Namespace: ns,
			}, cm)).To(Succeed())
			cm.Data["managed"] = "tampered"
			Expect(env.Client.Update(ctx, cm)).To(Succeed())

			Eventually(func(g Gomega, ctx SpecContext) {
				current := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{
					Name:      cmName,
					Namespace: ns,
				}, current)).To(Succeed())
				g.Expect(current.Data).To(HaveKeyWithValue("managed", "expected"),
					"drift on collection item %s/%s was not corrected", ns, cmName)
			}, 60*time.Second, 2*time.Second).WithContext(ctx).Should(Succeed())
		}
	})
})
