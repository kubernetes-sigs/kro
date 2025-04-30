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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("Update", func() {
	It("should handle updates to instance resources correctly", func() {
		ctx := context.Background()
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())

		// Create ResourceGraphDefinition for a simple deployment service
		rgd := generator.NewResourceGraphDefinition("test-update",
			generator.WithSchema(
				"TestUpdate", "v1alpha1",
				map[string]interface{}{
					"replicas": "integer | default=1",
					"image":    "string | default=nginx:latest",
					"port":     "integer | default=80",
				},
				nil,
			),
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "deployment-${schema.metadata.name}",
				},
				"spec": map[string]interface{}{
					"replicas": "${schema.spec.replicas}",
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "test",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "test",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "app",
									"image": "${schema.spec.image}",
									"ports": []interface{}{
										map[string]interface{}{
											"containerPort": "${schema.spec.port}",
										},
									},
								},
							},
						},
					},
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).Should(Succeed())

		// Create initial instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestUpdate",
				"metadata": map[string]interface{}{
					"name":      "test-instance-for-updates",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"image":    "nginx:1.19",
					"port":     80,
					"replicas": 1,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify initial deployment
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "deployment-test-instance-for-updates",
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.19"))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(80)))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Mark deployment as ready
		deployment.Status.Replicas = 1
		deployment.Status.ReadyReplicas = 1
		deployment.Status.AvailableReplicas = 1
		Expect(env.Client.Status().Update(ctx, deployment)).To(Succeed())

		// Update instance with new values
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance-for-updates",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			instance.Object["spec"] = map[string]interface{}{
				"replicas": int64(3),
				"image":    "nginx:1.20",
				"port":     int64(443),
			}
			err = env.Client.Update(ctx, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).Should(Succeed())

		// Verify deployment is updated with new values
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "deployment-test-instance-for-updates",
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(3)))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Image).To(Equal("nginx:1.20"))
			g.Expect(deployment.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort).To(Equal(int32(443)))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "deployment-test-instance-for-updates",
				Namespace: namespace,
			}, deployment)
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})
})
