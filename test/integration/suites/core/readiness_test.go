// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package core_test

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("Readiness", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	It(`should wait for deployment to have deployment.spec.replicas 
		== deployment.status.availableReplicas before creating service`, func() {
		rg := generator.NewResourceGraphDefinition("test-readiness",
			generator.WithNamespace(namespace),
			generator.WithSchema(
				"TestReadiness", "v1alpha1",
				map[string]interface{}{
					"name":     "string",
					"replicas": "integer",
					"deployment": map[string]interface{}{
						"includeAnnotations": "boolean | default=false",
						"annotations": map[string]interface{}{
							"app": "string | default=nginx",
						},
					},
					"service": map[string]interface{}{
						"includeAnnotations": "boolean | default=true",
						"annotations": map[string]interface{}{
							"app": "string | default=service",
						},
					},
				},
				nil,
			),
			// Deployment - no dependencies
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
					"annotations": `${schema.spec.deployment.includeAnnotations == true
								? schema.spec.deployment.annotations : null}`,
				},
				"spec": map[string]interface{}{
					"replicas": "${schema.spec.replicas}",
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
									"name":  "${schema.spec.name}-deployment",
									"image": "nginx",
									"ports": []interface{}{
										map[string]interface{}{
											"containerPort": 8080,
										},
									},
								},
							},
						},
					},
				},
			}, []string{"${deployment.spec.replicas == deployment.status.availableReplicas}"}, nil),
			// ServiceB - depends on deploymentA and deploymentB
			generator.WithResource("service", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${deployment.metadata.name}",
					"annotations": `${schema.spec.service.includeAnnotations == true
								? schema.spec.service.annotations : null}`,
				},
				"spec": map[string]interface{}{
					"selector": map[string]interface{}{
						"app": "deployment",
					},
					"ports": []interface{}{
						map[string]interface{}{
							"port":       8080,
							"targetPort": 8080,
						},
					},
				},
			}, nil, nil),
		)

		// Create ResourceGraphDefinition
		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRG := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, createdRG)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRG.Spec.Schema.Kind).To(Equal("TestReadiness"))
			g.Expect(createdRG.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRG.Spec.Resources).To(HaveLen(2))

			g.Expect(createdRG.Status.TopologicalOrder).To(Equal([]string{
				"deployment",
				"service",
			}))

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRG.Status.TopologicalOrder).To(HaveLen(2))
			// Verify conditions
			g.Expect(createdRG.Status.Conditions).To(HaveLen(3))
			g.Expect(createdRG.Status.Conditions[0].Type).To(Equal(
				krov1alpha1.ResourceGraphDefinitionConditionTypeReconcilerReady,
			))
			g.Expect(createdRG.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRG.Status.Conditions[1].Type).To(Equal(
				krov1alpha1.ResourceGraphDefinitionConditionTypeGraphVerified,
			))
			g.Expect(createdRG.Status.Conditions[1].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRG.Status.Conditions[2].Type).To(
				Equal(krov1alpha1.ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced),
			)
			g.Expect(createdRG.Status.Conditions[2].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRG.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

		}, 10*time.Second, time.Second).Should(Succeed())

		name := "test-readiness"
		replicas := 5
		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KroDomainName, "v1alpha1"),
				"kind":       "TestReadiness",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":     name,
					"replicas": replicas,
					"deployment": map[string]interface{}{
						"includeAnnotations": false,
						"annotations":        map[string]interface{}{},
					},
					"service": map[string]interface{}{
						"includeAnnotations": true,
						"annotations": map[string]interface{}{
							"app": "service",
						},
					},
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if instance is created
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify DeploymentB is created
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(replicas)))
			g.Expect(deployment.Annotations).To(HaveLen(0))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify Service is not created yet
		Eventually(func(g Gomega) bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, &corev1.Service{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Patch the deployment to have available replicas in status
		deployment.Status.Replicas = int32(replicas)
		deployment.Status.ReadyReplicas = int32(replicas)
		deployment.Status.AvailableReplicas = int32(replicas)
		deployment.Status.Conditions = []appsv1.DeploymentCondition{
			{
				Type:    appsv1.DeploymentAvailable,
				Status:  corev1.ConditionTrue,
				Reason:  "MinimumReplicasAvailable",
				Message: "Deployment has minimum availability.",
			},
		}
		Expect(env.Client.Status().Update(ctx, deployment)).To(Succeed())

		service := &corev1.Service{}
		// Verify Service is created now
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, service)
			g.Expect(err).ToNot(HaveOccurred())

			// validate service spec
			Expect(service.Annotations).To(HaveLen(1))
			Expect(service.Annotations["app"]).To(Equal("service"))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Delete instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Verify instance is deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, instance)
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Delete ResourceGraphDefinition
		Expect(env.Client.Delete(ctx, rg)).To(Succeed())

		// Verify ResourceGraphDefinition is deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, &krov1alpha1.ResourceGraphDefinition{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())
	})

})
