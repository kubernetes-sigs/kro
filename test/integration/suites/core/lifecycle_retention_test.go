// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

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
	"github.com/kubernetes-sigs/kro/pkg/metadata"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Lifecycle Retention Policy", func() {
	var ns *corev1.Namespace
	const testGroupLifecycle = "lifecycle.test.kro.run"

	BeforeEach(func(ctx SpecContext) {
		ns = &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("test-%s", rand.String(5)),
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})

	Context("with map literal syntax", func() {
		It("should retain resources on instance deletion and allow adoption by new instance", func(ctx SpecContext) {
			rgd := createLifecycleRGD("lifecycle-map-rgd", testGroupLifecycle, "LifecycleMapTest", "v1alpha1", true)
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			waitForRGDActive(ctx, rgd.Name)

			By("Creating first instance")
			instance1 := createLifecycleInstance(testGroupLifecycle, "LifecycleMapTest", "v1alpha1", "test-app-1", ns.Name, "my-app")
			Expect(env.Client.Create(ctx, instance1)).To(Succeed())
			waitForInstanceActive(ctx, ns.Name, "test-app-1", instance1)

			By("Verifying resources exist with KRO labels")
			cm := &corev1.ConfigMap{}
			verifyResourceExists(ctx, ns.Name, "test-app-1-config", cm)
			Expect(cm.Labels).To(HaveKey(metadata.InstanceIDLabel))
			Expect(cm.Labels).To(HaveKey(metadata.NodeIDLabel))

			secret := &corev1.Secret{}
			verifyResourceExists(ctx, ns.Name, "test-app-1-secret", secret)
			Expect(secret.Labels).To(HaveKey(metadata.InstanceIDLabel))
			Expect(secret.Labels).To(HaveKey(metadata.NodeIDLabel))

			By("Deleting first instance")
			Expect(env.Client.Delete(ctx, instance1)).To(Succeed())
			waitForResourceDeleted(ctx, ns.Name, "test-app-1", instance1)

			By("Verifying resources retained without KRO labels")
			cm = &corev1.ConfigMap{}
			verifyResourceExists(ctx, ns.Name, "test-app-1-config", cm)
			Expect(cm.Labels).ToNot(HaveKey(metadata.InstanceIDLabel))
			Expect(cm.Labels).ToNot(HaveKey(metadata.NodeIDLabel))

			secret = &corev1.Secret{}
			verifyResourceExists(ctx, ns.Name, "test-app-1-secret", secret)
			Expect(secret.Labels).ToNot(HaveKey(metadata.InstanceIDLabel))
			Expect(secret.Labels).ToNot(HaveKey(metadata.NodeIDLabel))

			By("Creating second instance that adopts the retained resources")
			instance2 := createLifecycleInstance(testGroupLifecycle, "LifecycleMapTest", "v1alpha1", "test-app-1", ns.Name, "my-app-updated")
			Expect(env.Client.Create(ctx, instance2)).To(Succeed())
			waitForInstanceActive(ctx, ns.Name, "test-app-1", instance2)

			By("Verifying resources re-adopted with new KRO labels")
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "test-app-1-config", Namespace: ns.Name}, cm)).To(Succeed())
				g.Expect(cm.Labels).To(HaveKey(metadata.InstanceIDLabel))
				g.Expect(cm.Labels[metadata.InstanceIDLabel]).To(Equal(string(instance2.GetUID())))
				g.Expect(cm.Data["appName"]).To(Equal("my-app-updated"))
			}, 30*time.Second, time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				secret := &corev1.Secret{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "test-app-1-secret", Namespace: ns.Name}, secret)).To(Succeed())
				g.Expect(secret.Labels).To(HaveKey(metadata.InstanceIDLabel))
				g.Expect(secret.Labels[metadata.InstanceIDLabel]).To(Equal(string(instance2.GetUID())))
			}, 30*time.Second, time.Second).Should(Succeed())

			Expect(env.Client.Delete(ctx, instance2)).To(Succeed())
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})

	Context("with CEL expression syntax", func() {
		It("should retain resources on instance deletion and allow adoption by new instance", func(ctx SpecContext) {
			rgd := createLifecycleRGD("lifecycle-cel-rgd", testGroupLifecycle, "LifecycleCELTest", "v1alpha1", false)
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			waitForRGDActive(ctx, rgd.Name)

			By("Creating first instance")
			instance1 := createLifecycleInstance(testGroupLifecycle, "LifecycleCELTest", "v1alpha1", "test-app-2", ns.Name, "my-app")
			Expect(env.Client.Create(ctx, instance1)).To(Succeed())
			waitForInstanceActive(ctx, ns.Name, "test-app-2", instance1)

			By("Verifying resources exist with KRO labels")
			cm := &corev1.ConfigMap{}
			verifyResourceExists(ctx, ns.Name, "test-app-2-config", cm)
			Expect(cm.Labels).To(HaveKey(metadata.InstanceIDLabel))
			Expect(cm.Labels).To(HaveKey(metadata.NodeIDLabel))

			secret := &corev1.Secret{}
			verifyResourceExists(ctx, ns.Name, "test-app-2-secret", secret)
			Expect(secret.Labels).To(HaveKey(metadata.InstanceIDLabel))
			Expect(secret.Labels).To(HaveKey(metadata.NodeIDLabel))

			By("Deleting first instance")
			Expect(env.Client.Delete(ctx, instance1)).To(Succeed())
			waitForResourceDeleted(ctx, ns.Name, "test-app-2", instance1)

			By("Verifying resources retained without KRO labels")
			cm = &corev1.ConfigMap{}
			verifyResourceExists(ctx, ns.Name, "test-app-2-config", cm)
			Expect(cm.Labels).ToNot(HaveKey(metadata.InstanceIDLabel))
			Expect(cm.Labels).ToNot(HaveKey(metadata.NodeIDLabel))

			secret = &corev1.Secret{}
			verifyResourceExists(ctx, ns.Name, "test-app-2-secret", secret)
			Expect(secret.Labels).ToNot(HaveKey(metadata.InstanceIDLabel))
			Expect(secret.Labels).ToNot(HaveKey(metadata.NodeIDLabel))

			By("Creating second instance that adopts the retained resources")
			instance2 := createLifecycleInstance(testGroupLifecycle, "LifecycleCELTest", "v1alpha1", "test-app-2", ns.Name, "my-app-updated")
			Expect(env.Client.Create(ctx, instance2)).To(Succeed())
			waitForInstanceActive(ctx, ns.Name, "test-app-2", instance2)

			By("Verifying resources re-adopted with new KRO labels")
			Eventually(func(g Gomega) {
				cm := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "test-app-2-config", Namespace: ns.Name}, cm)).To(Succeed())
				g.Expect(cm.Labels).To(HaveKey(metadata.InstanceIDLabel))
				g.Expect(cm.Labels[metadata.InstanceIDLabel]).To(Equal(string(instance2.GetUID())))
				g.Expect(cm.Data["appName"]).To(Equal("my-app-updated"))
			}, 30*time.Second, time.Second).Should(Succeed())

			Eventually(func(g Gomega) {
				secret := &corev1.Secret{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: "test-app-2-secret", Namespace: ns.Name}, secret)).To(Succeed())
				g.Expect(secret.Labels).To(HaveKey(metadata.InstanceIDLabel))
				g.Expect(secret.Labels[metadata.InstanceIDLabel]).To(Equal(string(instance2.GetUID())))
			}, 30*time.Second, time.Second).Should(Succeed())

			Expect(env.Client.Delete(ctx, instance2)).To(Succeed())
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})
})

func createLifecycleRGD(name, group, kind, version string, useMapLiteral bool) *krov1alpha1.ResourceGraphDefinition {
	// Lifecycle is a CEL expression string wrapped in ${}
	lifecycle := "${policy().withRetain()}"

	rgd := generator.NewResourceGraphDefinition(name,
		generator.WithSchema(kind, version,
			map[string]any{"appName": "string"},
			map[string]any{},
		),
		generator.WithResourceWithLifecycle("configMap", map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-config",
				"namespace": "${schema.metadata.namespace}",
			},
			"data": map[string]any{
				"appName": "${schema.spec.appName}",
			},
		}, nil, nil, lifecycle),
		generator.WithResourceWithLifecycle("secret", map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      "${schema.metadata.name}-secret",
				"namespace": "${schema.metadata.namespace}",
			},
			"type": "Opaque",
			"stringData": map[string]any{
				"key": "value",
			},
		}, nil, nil, lifecycle),
	)
	rgd.Spec.Schema.Group = group
	return rgd
}

func createLifecycleInstance(group, kind, version, name, namespace, appName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": group + "/" + version,
			"kind":       kind,
			"metadata": map[string]any{
				"name":      name,
				"namespace": namespace,
			},
			"spec": map[string]any{
				"appName": appName,
			},
		},
	}
}
