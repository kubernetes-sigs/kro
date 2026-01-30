// Copyright 2025 The Kube Resource Orchestrator Authors
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
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
)

var _ = Describe("Instance Isolation", func() {
	var (
		namespace, namespace2 string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())

		namespace2 = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace2,
			},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())

		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace2,
			},
		})).To(Succeed())
	})

	It("should prevent cross-RGD reconciliation using instance GVK labels", func(ctx SpecContext) {
		rgd1 := generator.NewResourceGraphDefinition("test-conflict-app1",
			generator.WithSchema(
				"TestConflictApp1", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"data": map[string]interface{}{
					"source": "app1",
				},
			}, nil, nil),
		)

		rgd2 := generator.NewResourceGraphDefinition("test-conflict-app2",
			generator.WithSchema(
				"TestConflictApp2", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				nil,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"data": map[string]interface{}{
					"source": "app2",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd1)).To(Succeed())
		Expect(env.Client.Create(ctx, rgd2)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd1)).To(Succeed())
			Expect(env.Client.Delete(ctx, rgd2)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd1.Name}, rgd1)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd1.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd2.Name}, rgd2)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd2.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Here we are purposefully creating 2 instances with the same name with different RGDs
		// Both instances have the same name but are in 2 different namespaces
		instance1 := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestConflictApp1",
				"metadata": map[string]interface{}{
					"name":      "my-app",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": "shared-resource",
				},
			},
		}

		instance2 := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestConflictApp2",
				"metadata": map[string]interface{}{
					"name":      "my-app",
					"namespace": namespace2,
				},
				"spec": map[string]interface{}{
					"name": "shared-resource",
				},
			},
		}

		Expect(env.Client.Create(ctx, instance1)).To(Succeed())
		Expect(env.Client.Create(ctx, instance2)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance1)).To(Succeed())
			Expect(env.Client.Delete(ctx, instance2)).To(Succeed())
		})

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance1.GetName(),
				Namespace: namespace,
			}, instance1)
			g.Expect(err).ToNot(HaveOccurred())

			instanceState, found, err := unstructured.NestedString(instance1.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(instanceState).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance2.GetName(),
				Namespace: namespace2,
			}, instance2)
			g.Expect(err).ToNot(HaveOccurred())

			instanceState, found, err := unstructured.NestedString(instance2.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(instanceState).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		cfgMap1 := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      "shared-resource",
				Namespace: namespace,
			}, cfgMap1)).ToNot(HaveOccurred())
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		cfgMap2 := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      "shared-resource",
				Namespace: namespace2,
			}, cfgMap2)).ToNot(HaveOccurred())
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(cfgMap1.GetLabels()).To(SatisfyAll(
			HaveKeyWithValue(metadata.InstanceLabel, "my-app"),
			HaveKeyWithValue(metadata.InstanceGroupLabel, krov1alpha1.KRODomainName),
			HaveKeyWithValue(metadata.InstanceVersionLabel, "v1alpha1"),
			HaveKeyWithValue(metadata.InstanceKindLabel, "TestConflictApp1"),
		), "ConfigMap should have instance GVK labels for TestConflictApp1")

		Expect(cfgMap2.GetLabels()).To(SatisfyAll(
			HaveKeyWithValue(metadata.InstanceLabel, "my-app"),
			HaveKeyWithValue(metadata.InstanceGroupLabel, krov1alpha1.KRODomainName),
			HaveKeyWithValue(metadata.InstanceVersionLabel, "v1alpha1"),
			HaveKeyWithValue(metadata.InstanceKindLabel, "TestConflictApp2"),
		), "ConfigMap should have instance GVK labels for TestConflictApp2")

		Expect(cfgMap1.Data).To(HaveKeyWithValue("source", "app1"))
		Expect(cfgMap2.Data).To(HaveKeyWithValue("source", "app2"))

		// Intentional reconcile to validate behavior here
		// We want to ensure that instance with change reconciles
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance1.GetName(),
				Namespace: namespace,
			}, instance1)
			g.Expect(err).ToNot(HaveOccurred())

			instance1.Object["spec"] = map[string]interface{}{
				"name": "shared-resource",
			}
			err = env.Client.Update(ctx, instance1)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		time.Sleep(5 * time.Second)

		Consistently(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      "shared-resource",
				Namespace: namespace2,
			}, cfgMap2)).ToNot(HaveOccurred())

			g.Expect(cfgMap2.GetLabels()).To(SatisfyAll(
				HaveKeyWithValue(metadata.InstanceKindLabel, "TestConflictApp2"),
				HaveKeyWithValue(metadata.InstanceLabel, "my-app"),
			))
			g.Expect(cfgMap2.Data).To(HaveKeyWithValue("source", "app2"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed(),
			"Instance2 ConfigMap should not be affected by instance1 updates due to GVK label filtering")
	})
})
