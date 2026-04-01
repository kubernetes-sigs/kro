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
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("ExternalRef Deletion", func() {
	var ns *corev1.Namespace

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

	It("should delete managed resources before removing finalizer when external ref is topological root",
		func(ctx SpecContext) {
			// Dependency graph:
			//   extcm (External, root) ──→ managedcm (Resource, leaf)
			const (
				extCMName    = "source-configmap"
				instanceName = "extref-del-inst"
			)
			rgdName := fmt.Sprintf("test-extref-del-%s", rand.String(5))
			managedCMName := instanceName + "-managed"

			By("creating an RGD where the external ref is the topological root")
			rgd := generator.NewResourceGraphDefinition(rgdName,
				generator.WithSchema(
					"TestExternalRefDeletion", "v1alpha1",
					map[string]interface{}{},
					map[string]interface{}{},
				),
				// extcm listed first → becomes root in topological order (no deps)
				generator.WithExternalRef("extcm", &krov1alpha1.ExternalRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Metadata: krov1alpha1.ExternalRefMetadata{
						Name: extCMName,
					},
				}, nil, nil),
				// managedcm depends on extcm fields for both identity and data
				generator.WithResource("managedcm", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${extcm.data.managedName}",
					},
					"data": map[string]interface{}{
						"inherited": "${extcm.data.value}",
					},
				}, nil, nil),
			)
			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, rgd) })

			By("waiting for RGD to become active with extcm as topological root")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				g.Expect(rgd.Status.TopologicalOrder).To(HaveLen(2))
				g.Expect(rgd.Status.TopologicalOrder[0]).To(Equal("extcm"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("creating the external ConfigMap")
			extCM := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      extCMName,
					Namespace: ns.Name,
				},
				Data: map[string]string{
					"managedName": managedCMName,
					"value":       "from-external",
				},
			}
			Expect(env.Client.Create(ctx, extCM)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, extCM) })

			By("creating the instance")
			instance := &unstructured.Unstructured{
				Object: map[string]interface{}{
					"apiVersion": "kro.run/v1alpha1",
					"kind":       "TestExternalRefDeletion",
					"metadata": map[string]interface{}{
						"name":      instanceName,
						"namespace": ns.Name,
					},
				},
			}
			Expect(env.Client.Create(ctx, instance)).To(Succeed())
			DeferCleanup(func(ctx SpecContext) { _ = env.Client.Delete(ctx, instance) })

			By("waiting for the instance to become ACTIVE")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: instanceName, Namespace: ns.Name,
				}, instance)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(instance.Object).To(HaveKeyWithValue("status",
					HaveKeyWithValue("state", "ACTIVE")))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying the managed ConfigMap was created with data derived from the external ref")
			Eventually(func(g Gomega, ctx SpecContext) {
				cm := &corev1.ConfigMap{}
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: managedCMName, Namespace: ns.Name,
				}, cm)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(cm.Data["inherited"]).To(Equal("from-external"))
			}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("deleting the instance")
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())

			// Wait for the instance to be fully gone, then immediately check the
			// managed CM — no retry window. The controller must delete managedcm
			// before removing the finalizer, so when the instance is NotFound the
			// CM must already be NotFound too.
			By("waiting for instance to be fully deleted")
			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: instanceName, Namespace: ns.Name,
				}, instance)
				g.Expect(err).To(MatchError(errors.IsNotFound,
					"instance should be fully deleted once all managed resources are cleaned up"))
			}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			By("verifying managed ConfigMap was deleted before the instance finalizer was removed")
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: managedCMName, Namespace: ns.Name,
			}, &corev1.ConfigMap{})
			Expect(err).To(MatchError(errors.IsNotFound,
				"managed ConfigMap must already be gone when instance is deleted"))
		})
})
