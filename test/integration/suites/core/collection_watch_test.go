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

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
)

var _ = Describe("Collection Watch", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should reactively reconcile when a non-last collection item is modified", func(ctx SpecContext) {
		// Create an RGD with a forEach collection that produces 3 ConfigMaps.
		rgd := generator.NewResourceGraphDefinition("test-collection-watch",
			generator.WithSchema(
				"CollWatchTest", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to become active.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create an instance with 3 values -> 3 ConfigMaps.
		name := "test-coll-watch"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CollWatchTest",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"alpha", "beta", "gamma"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Wait for instance to become ACTIVE and all ConfigMaps to exist.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify all 3 ConfigMaps exist.
		for _, value := range []string{"alpha", "beta", "gamma"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["key"]).To(Equal(value))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Wait for the requeue timer to fire and watches to stabilize.
		// The default requeue duration is 5s, so sleeping 5s ensures we are
		// past the last timer-driven reconcile.
		time.Sleep(5 * time.Second)

		// Mutate the FIRST ConfigMap (alpha). If the bug exists, only the last
		// collection item (gamma) has a watch — alpha's modification won't
		// trigger reactive reconciliation.
		firstCM := &corev1.ConfigMap{}
		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      fmt.Sprintf("%s-alpha", name),
			Namespace: namespace,
		}, firstCM)).To(Succeed())

		firstCM.Data["key"] = "tampered"
		Expect(env.Client.Update(ctx, firstCM)).To(Succeed())

		// Assert that the instance controller reactively corrects the drift
		// within 3 seconds. The requeue timer is 5s, so if this passes within
		// 3s it proves the correction was watch-driven.
		Eventually(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-alpha", name),
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data["key"]).To(Equal("alpha"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed(),
			"first collection item should be corrected reactively via watch, not the 5s requeue timer",
		)

		// Cleanup.
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should not react to externally created resources after collection shrinks", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-collection-shrink-watch",
			generator.WithSchema(
				"CollShrinkTest", "v1alpha1",
				map[string]interface{}{
					"name":   "string",
					"values": "[]string",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-${value}",
				},
				"data": map[string]interface{}{
					"key": "${value}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"value": "${schema.spec.values}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance with 3 items.
		name := "test-coll-shrink"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CollShrinkTest",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":   name,
					"values": []interface{}{"alpha", "beta", "gamma"},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Wait for ACTIVE with all 3 ConfigMaps.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(val).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		for _, value := range []string{"alpha", "beta", "gamma"} {
			cm := &corev1.ConfigMap{}
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name:      fmt.Sprintf("%s-%s", name, value),
					Namespace: namespace,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
		}

		// Shrink collection from 3 to 2 by removing "gamma".
		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      name,
			Namespace: namespace,
		}, instance)).To(Succeed())
		instance.Object["spec"] = map[string]interface{}{
			"name":   name,
			"values": []interface{}{"alpha", "beta"},
		}
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		// Wait for gamma ConfigMap to be pruned.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-gamma", name),
				Namespace: namespace,
			}, &corev1.ConfigMap{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "gamma should be pruned"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Wait for instance to re-stabilize as ACTIVE.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			val, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(val).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Let watches stabilize past the requeue timer.
		time.Sleep(5 * time.Second)

		// Externally create a ConfigMap with gamma's old name but WITHOUT kro
		// labels. The collection watch selector (instance-id + node-id) should
		// NOT match this resource.
		externalCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-gamma", name),
				Namespace: namespace,
			},
			Data: map[string]string{
				"key": "external-data",
			},
		}
		Expect(env.Client.Create(ctx, externalCM)).To(Succeed())

		// The external ConfigMap should remain untouched. If a stale watch
		// fired, the controller would reconcile and potentially interfere.
		// Use Consistently for 4s (under the 5s requeue) to prove no
		// watch-driven reaction.
		Consistently(func(g Gomega, ctx SpecContext) {
			cm := &corev1.ConfigMap{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-gamma", name),
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data["key"]).To(Equal("external-data"))
		}, 4*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed(),
			"external resource with old collection item name should not trigger watch-driven reconciliation",
		)

		// Cleanup.
		Expect(env.Client.Delete(ctx, externalCM)).To(Succeed())
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
