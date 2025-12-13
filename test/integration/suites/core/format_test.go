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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Format function in ResourceGraphDefinition templates", func() {
	It("should resolve formatted strings using format()", func(ctx SpecContext) {
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{}
		ns.Name = namespace
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, ns)
		})

		By("creating ResourceGraphDefinition")

		rgd := generator.NewResourceGraphDefinition(
			"test-format",
			generator.WithSchema(
				"TestFormat",
				"v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{},
			),
			generator.WithResource("serviceAccount", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}",
				},
			}, nil, nil),
			generator.WithResource("configMap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}",
				},
				"data": map[string]interface{}{
					"key": `${"%s:%s".format([schema.metadata.namespace, serviceAccount.metadata.name])}`,
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for RGD to become active")

		Eventually(func(g Gomega, ctx SpecContext) {
			obj := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, obj)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(obj.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(obj.Status.TopologicalOrder).To(ContainElements("serviceAccount", "configMap"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating instance")

		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestFormat",
				"metadata": map[string]interface{}{
					"name":      "test-format",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		By("waiting for instance to become ACTIVE")

		Eventually(func(g Gomega, ctx SpecContext) {
			obj := instance.DeepCopy()
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: instance.GetNamespace(),
			}, obj)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(obj.Object["status"]).To(HaveKeyWithValue("state", "ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying generated ServiceAccount")

		sa := &corev1.ServiceAccount{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-format",
				Namespace: namespace,
			}, sa)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying generated ConfigMap with formatted key")

		cm := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-format",
				Namespace: namespace,
			}, cm)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm.Data).To(HaveKey("key"))
			g.Expect(cm.Data["key"]).To(Equal(fmt.Sprintf("%s:%s", namespace, "test-format")))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
