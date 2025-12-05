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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("ResourceGraphDefinition Time Expressions", func() {
	It("should populate status.creationTimestamp from secret.metadata.creationTimestamp", func(ctx SpecContext) {
		ns := fmt.Sprintf("test-%s", rand.String(5))

		By("creating namespace")
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: ns},
		}
		Expect(env.Client.Create(ctx, namespace)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, namespace)).To(Succeed())
		})

		By("creating ResourceGraphDefinition")

		rgd := generator.NewResourceGraphDefinition(
			"test-timestamp-rgd",

			// schema
			generator.WithSchema(
				"TestTimestamp",
				"v1alpha1",
				map[string]any{
					"name": `string | default="test"`,
				},
				map[string]any{
					// Now a success case
					"creationTimestamp": `${secret.metadata.creationTimestamp}`,
				},
			),

			// secret resource
			generator.WithResource(
				"secret",
				map[string]any{
					"apiVersion": "v1",
					"kind":       "Secret",
					"metadata": map[string]any{
						"name": "${schema.spec.name}",
					},
					"stringData": map[string]any{
						"hello": "world",
					},
				},
				nil,
				nil,
			),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for RGD to become Active")
		created := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, created)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(created.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("creating instance")
		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestTimestamp",
				"metadata": map[string]any{
					"name":      "test",
					"namespace": ns,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		By("waiting for Secret to be created")
		var secretCreationTS string

		Eventually(func(g Gomega, ctx SpecContext) {
			secret := &corev1.Secret{}
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test",
				Namespace: ns,
			}, secret)
			g.Expect(err).ToNot(HaveOccurred())

			// Format RFC3339 exactly as returned by Kubernetes
			secretCreationTS = secret.ObjectMeta.CreationTimestamp.Time.UTC().Format(time.RFC3339)
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		By("verifying instance becomes ACTIVE and timestamp matches secret")

		Eventually(func(g Gomega, ctx SpecContext) {
			obj := &unstructured.Unstructured{}
			obj.SetAPIVersion("kro.run/v1alpha1")
			obj.SetKind("TestTimestamp")

			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test",
				Namespace: ns,
			}, obj)
			g.Expect(err).ToNot(HaveOccurred())

			status := obj.Object["status"].(map[string]any)
			g.Expect(status).To(HaveKeyWithValue("state", Equal("ACTIVE")))
			g.Expect(status).To(HaveKeyWithValue("creationTimestamp", Equal(secretCreationTS)))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
