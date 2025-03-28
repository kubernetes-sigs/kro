// Copyright 2025 The Kube Resource Orchestrator Authors.
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

var _ = Describe("Conditions", func() {
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

	It("should not create deployment, service, and configmap due to condition deploymentEnabled == false", func() {
		rgd := generator.NewResourceGraphDefinition("test-conditions",
			generator.WithSchema(
				"TestConditions", "v1alpha1",
				map[string]interface{}{
					"name":                   "string",
					"deploymentAenabled":     "boolean",
					"deploymentBenabled":     "boolean",
					"serviceAccountAenabled": "boolean",
					"serviceAccountBenabled": "boolean",
					"serviceBenabled":        "boolean",
				},
				nil,
			),
			// Deployment - no dependencies
			generator.WithResource("deploymentA", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-a",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
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
			}, nil, []string{"${schema.spec.deploymentAenabled}"}),
			// Depends on serviceAccountA
			generator.WithResource("deploymentB", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-b",
				},
				"spec": map[string]interface{}{
					"replicas": 1,
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
							"serviceAccountName": "${serviceAccountA.metadata.name + schema.spec.name}",
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
			}, nil, []string{"${schema.spec.deploymentBenabled}"}),
			// serviceAccountA - no dependencies
			generator.WithResource("serviceAccountA", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-a",
				},
			}, nil, []string{"${schema.spec.serviceAccountAenabled}"}),
			// ServiceAccount - depends on service
			generator.WithResource("serviceAccountB", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ServiceAccount",
				"metadata": map[string]interface{}{
					"name": "${serviceA.metadata.name}",
				},
			}, nil, []string{"${schema.spec.serviceAccountBenabled}"}),
			// ServiceA - depends on DeploymentA
			generator.WithResource("serviceA", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${deploymentA.metadata.name}",
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
			// ServiceB - depends on deploymentA and deploymentB
			generator.WithResource("serviceB", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Service",
				"metadata": map[string]interface{}{
					"name": "${deploymentB.metadata.name + deploymentA.metadata.name}",
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
			}, nil, []string{"${schema.spec.serviceBenabled}"}),
		)

		// Create ResourceGraphDefinition
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("TestConditions"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(6))

			g.Expect(createdRGD.Status.TopologicalOrder).To(Equal([]string{
				"deploymentA",
				"serviceAccountA",
				"serviceA",
				"deploymentB",
				"serviceAccountB",
				"serviceB",
			}))

			// Verify the ResourceGraphDefinition status
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(6))
			// Verify conditions
			g.Expect(createdRGD.Status.Conditions).To(HaveLen(3))
			g.Expect(createdRGD.Status.Conditions[0].Type).To(Equal(
				krov1alpha1.ResourceGraphDefinitionConditionTypeReconcilerReady,
			))
			g.Expect(createdRGD.Status.Conditions[0].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRGD.Status.Conditions[1].Type).To(Equal(
				krov1alpha1.ResourceGraphDefinitionConditionTypeGraphVerified,
			))
			g.Expect(createdRGD.Status.Conditions[1].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRGD.Status.Conditions[2].Type).To(
				Equal(krov1alpha1.ResourceGraphDefinitionConditionTypeCustomResourceDefinitionSynced),
			)
			g.Expect(createdRGD.Status.Conditions[2].Status).To(Equal(metav1.ConditionTrue))
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

		}, 10*time.Second, time.Second).Should(Succeed())

		name := "test-conditions"
		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestConditions",
				"metadata": map[string]interface{}{
					"name":      name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":                   name,
					"deploymentAenabled":     false,
					"deploymentBenabled":     true,
					"serviceAccountAenabled": true,
					"serviceBenabled":        true,
					"serviceAccountBenabled": true,
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
			val, b, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(b).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify DeploymentA is not created
		Eventually(func(g Gomega) bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-a",
				Namespace: namespace,
			}, &appsv1.Deployment{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Verify serviceAccountA is created
		serviceAccountA := &corev1.ServiceAccount{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      fmt.Sprintf("%s-a", name),
				Namespace: namespace,
			}, serviceAccountA)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify DeploymentB is created
		deploymentB := &appsv1.Deployment{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-b",
				Namespace: namespace,
			}, deploymentB)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deploymentB.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(deploymentB.Spec.Template.Spec.ServiceAccountName).To(Equal(name + "-a" + name))
		}, 20*time.Second, time.Second).Should(Succeed())

		// Verify ServiceA is not created
		Eventually(func(g Gomega) bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-a",
				Namespace: namespace,
			}, &corev1.Service{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Verify ServiceB is not created
		Eventually(func(g Gomega) bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name + "-b",
				Namespace: namespace,
			}, &corev1.Service{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

		// Verify ServiceAccountB is not created
		Eventually(func(g Gomega) bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      name,
				Namespace: namespace,
			}, &corev1.ServiceAccount{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())

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
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is deleted
		Eventually(func() bool {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			return errors.IsNotFound(err)
		}, 20*time.Second, time.Second).Should(BeTrue())
	})

})
