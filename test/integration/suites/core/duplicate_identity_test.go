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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("RGD duplicate identities before adoption", func() {
	It("preserves a foreign object through rejection and instance deletion", func(ctx SpecContext) {
		namespace := env.CreateNamespace(GinkgoT())
		const sharedName = "dup-shared"
		key := types.NamespacedName{Name: sharedName, Namespace: namespace}
		userCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: sharedName, Namespace: namespace,
				Labels:      map[string]string{"user-label": "keep"},
				Annotations: map[string]string{"user-annotation": "keep"},
			},
			Data: map[string]string{"from": "previous"},
		}
		Expect(env.Client.Create(ctx, userCM)).To(Succeed())
		pristine := &corev1.ConfigMap{}
		Expect(env.Client.Get(ctx, key, pristine)).To(Succeed())

		sharedConfigMap := func(from string) map[string]any {
			return map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": "${schema.spec.name}"},
				"data":     map[string]any{"from": from},
			}
		}
		rgd := generator.NewResourceGraphDefinition("test-dup-identity",
			generator.WithSchema("TestDupIdentity", "v1alpha1",
				map[string]any{"name": "string"}, map[string]any{"rendered": "${schema.spec.name}"}),
			generator.WithResource("cma", sharedConfigMap("cma"), nil, nil),
			generator.WithResource("cmb", sharedConfigMap("cmb"), nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(env.Client.Delete(ctx, rgd)).To(Succeed()) })
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1", "kind": "TestDupIdentity",
			"metadata": map[string]any{"name": "dup-instance", "namespace": namespace},
			"spec":     map[string]any{"name": sharedName},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		instanceKey := types.NamespacedName{Name: instance.GetName(), Namespace: namespace}
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			state, _, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state).To(Equal("ERROR"))
			conditions, _, err := unstructured.NestedSlice(instance.Object, "status", "conditions")
			g.Expect(err).NotTo(HaveOccurred())
			var ready map[string]any
			for _, entry := range conditions {
				condition := entry.(map[string]any)
				if condition["type"] == "ResourcesReady" {
					ready = condition
				}
			}
			g.Expect(ready).NotTo(BeNil())
			g.Expect(ready["status"]).To(Equal("False"))
			g.Expect(ready["observedGeneration"]).To(Equal(instance.GetGeneration()))
			g.Expect(ready["message"]).To(And(ContainSubstring("cma"), ContainSubstring("cmb")))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())

		assertUntouched := func(g Gomega) {
			live := &corev1.ConfigMap{}
			g.Expect(env.Client.Get(ctx, key, live)).To(Succeed())
			g.Expect(live.Data).To(Equal(pristine.Data))
			g.Expect(live.ObjectMeta).To(Equal(pristine.ObjectMeta), "including UID, resourceVersion, labels, annotations, ownerReferences and managedFields")
		}
		Consistently(assertUntouched, time.Second, 100*time.Millisecond).Should(Succeed())
		_, found, err := unstructured.NestedString(instance.Object, "status", "rendered")
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse(), "author-status execution is withheld on the rejected cycle")

		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func() bool {
			return apierrors.IsNotFound(env.Client.Get(ctx, instanceKey, instance))
		}, 30*time.Second, 100*time.Millisecond).Should(BeTrue())
		Consistently(assertUntouched, time.Second, 100*time.Millisecond).Should(Succeed())
	})

	It("does not mistake a resource-backed placeholder name for a duplicate", func(ctx SpecContext) {
		namespace := env.CreateNamespace(GinkgoT())
		configMap := func(name, value string) map[string]any {
			return map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{"name": name}, "data": map[string]any{"k": value},
			}
		}
		rgd := generator.NewResourceGraphDefinition("test-dup-softseed",
			generator.WithSchema("TestDupSoftseed", "v1alpha1", nil, map[string]any{"value": "${a.data.k}"}),
			generator.WithResource("a", configMap("upstream", "real"), nil, nil),
			generator.WithResource("b", configMap("${a.?data.k.orValue('fallback')}", "b"), nil, nil),
			generator.WithResource("c", configMap("fallback", "c"), nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(env.Client.Delete(ctx, rgd)).To(Succeed()) })
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)).To(Succeed())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1", "kind": "TestDupSoftseed",
			"metadata": map[string]any{"name": "softseed", "namespace": namespace},
		}}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) { Expect(env.Client.Delete(ctx, instance)).To(Succeed()) })
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: instance.GetName(), Namespace: namespace}, instance)).To(Succeed())
			state, _, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state).To(Equal("ACTIVE"))
			for name, value := range map[string]string{"upstream": "real", "real": "b", "fallback": "c"} {
				cm := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, cm)).To(Succeed())
				g.Expect(cm.Data["k"]).To(Equal(value), fmt.Sprintf("ConfigMap %s", name))
			}
		}, 30*time.Second, 100*time.Millisecond).Should(Succeed())
	})
})
