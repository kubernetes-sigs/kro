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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/ptr"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("ExternalRef", func() {
	It("should handle ResourceGraphDefinition with ExternalRef", func() {
		ctx := context.Background()
		namespace := fmt.Sprintf("test-%s", rand.String(5))

		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())

		// Create a Deployment that will be referenced
		deployment1 := &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-deployment",
				Namespace: namespace,
			},
			Spec: appsv1.DeploymentSpec{
				Replicas: ptr.To[int32](2),
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"app": "test-deployment",
					},
				},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{
						Labels: map[string]string{
							"app": "test-deployment",
						},
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "test-container", Image: "nginx"},
						},
					},
				},
			},
		}
		Expect(env.Client.Create(ctx, deployment1)).To(Succeed())

		// Create ResourceGraphDefinition with ExternalRef
		rgd := generator.NewResourceGraphDefinition("test-externalref",
			generator.WithSchema(
				"TestExternalRef", "v1alpha1",
				map[string]interface{}{},
				map[string]interface{}{},
			),
			generator.WithExternalRef("deployment1", &krov1alpha1.ExternalRef{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name:      "test-deployment",
					Namespace: namespace,
				},
			}, nil, nil),
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}",
				},
				"spec": map[string]interface{}{
					"replicas": "${deployment1.spec.replicas}",
					"selector": map[string]interface{}{
						"matchLabels": map[string]interface{}{
							"app": "deployment",
						},
					},
					"template": map[string]interface{}{
						"metadata": map[string]interface{}{
							"labels": map[string]interface{}{
								"app": "deployment",
							},
						},
						"spec": map[string]interface{}{
							"containers": []interface{}{
								map[string]interface{}{
									"name":  "web",
									"image": "nginx",
								},
							},
						},
					},
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("TestExternalRef"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(2))
			g.Expect(createdRGD.Spec.Resources[0].ExternalRef).ToNot(BeNil())
			g.Expect(createdRGD.Spec.Resources[0].ExternalRef.Kind).To(Equal("Deployment"))
			g.Expect(createdRGD.Spec.Resources[0].ExternalRef.Metadata.Name).To(Equal("test-deployment"))
			g.Expect(createdRGD.Spec.Resources[0].ExternalRef.Metadata.Namespace).To(Equal(namespace))

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(2))
			g.Expect(createdRGD.Status.TopologicalOrder).To(ContainElements("deployment1", "deployment"))
		}, 10*time.Second, time.Second).Should(Succeed())

		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "kro.run/v1alpha1",
				"kind":       "TestExternalRef",
				"metadata": map[string]interface{}{
					"name":      "foo-instance",
					"namespace": namespace,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify Deployment is created with correct environment variables
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "foo-instance",
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment has the ConfigMap reference in envFrom
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(2)))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Expect(env.Client.Delete(ctx, deployment1)).To(Succeed())
		Expect(env.Client.Delete(ctx, ns)).To(Succeed())
	})
})
