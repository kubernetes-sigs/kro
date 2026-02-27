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

var _ = Describe("ExternalRef Watch", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})
	})

	It("external ref watch triggers re-reconciliation on change", func(ctx SpecContext) {
		By("creating RGD with external ref ConfigMap and managed ConfigMap that copies its data")
		rgd := generator.NewResourceGraphDefinition("test-extref-watch",
			generator.WithSchema(
				"TestExtRefWatch", "v1alpha1",
				map[string]interface{}{},
				nil,
			),
			generator.WithExternalRef("extcm", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name: "ext-config",
				},
			}, nil, nil),
			generator.WithResource("managed", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "managed-config",
				},
				"data": map[string]interface{}{
					"replicas": "${extcm.data.replicas}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for RGD to become active")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating the external ConfigMap with replicas=1")
		extCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "ext-config",
				Namespace: namespace,
			},
			Data: map[string]string{
				"replicas": "1",
			},
		}
		Expect(env.Client.Create(ctx, extCM)).To(Succeed())

		By("creating the instance")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtRefWatch",
				"metadata": map[string]interface{}{
					"name":      "test-instance",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE and managed ConfigMap to have replicas=1")
		managedCM := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			err = env.Client.Get(ctx, types.NamespacedName{
				Name:      "managed-config",
				Namespace: namespace,
			}, managedCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(managedCM.Data).To(HaveKeyWithValue("replicas", "1"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("sleeping to let the watch settle")
		time.Sleep(5 * time.Second)

		By("updating external ConfigMap to replicas=3")
		Expect(env.Client.Get(ctx, types.NamespacedName{
			Name:      "ext-config",
			Namespace: namespace,
		}, extCM)).To(Succeed())
		extCM.Data["replicas"] = "3"
		Expect(env.Client.Update(ctx, extCM)).To(Succeed())

		By("asserting managed ConfigMap updates to replicas=3 within 3s (watch-driven)")
		// DefaultRequeueDuration is 5s. If the managed ConfigMap updates within 3s,
		// it proves the watch (not the requeue timer) triggered the reconciliation.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "managed-config",
				Namespace: namespace,
			}, managedCM)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(managedCM.Data).To(HaveKeyWithValue("replicas", "3"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed(),
			"managed ConfigMap should update reactively via watch, not the 5s requeue timer",
		)
	})

	It("external collection watch reacts to new matching resources", func(ctx SpecContext) {
		By("creating RGD with external collection ref using label selector")
		rgd := generator.NewResourceGraphDefinition("test-extcoll-watch",
			generator.WithSchema(
				"TestExtCollWatch", "v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{
					"configCount": "${string(size(extconfigs))}",
				},
			),
			generator.WithExternalRef("extconfigs", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"team": "alpha",
						},
					},
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for RGD to become active")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating first ConfigMap with label team=alpha")
		cm1 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "alpha-config-1",
				Namespace: namespace,
				Labels: map[string]string{
					"team": "alpha",
				},
			},
			Data: map[string]string{"key": "value1"},
		}
		Expect(env.Client.Create(ctx, cm1)).To(Succeed())

		By("creating the instance")
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExtCollWatch",
				"metadata": map[string]interface{}{
					"name":      "test-instance",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		By("waiting for instance to become ACTIVE with configCount=1")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(instance.Object).To(HaveKey("status"))
			g.Expect(instance.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("1"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("sleeping to let the watch settle")
		time.Sleep(5 * time.Second)

		By("creating a second ConfigMap with label team=alpha")
		cm2 := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "alpha-config-2",
				Namespace: namespace,
				Labels: map[string]string{
					"team": "alpha",
				},
			},
			Data: map[string]string{"key": "value2"},
		}
		Expect(env.Client.Create(ctx, cm2)).To(Succeed())

		By("asserting configCount updates to 2 within 3s (watch-driven)")
		// DefaultRequeueDuration is 5s. If the status updates within 3s,
		// it proves the watch (not the requeue timer) triggered the reconciliation.
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			configCount, found, err := unstructured.NestedString(instance.Object, "status", "configCount")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(configCount).To(Equal("2"))
		}, 3*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed(),
			"instance status.configCount should update reactively via watch, not the 5s requeue timer",
		)
	})
})
