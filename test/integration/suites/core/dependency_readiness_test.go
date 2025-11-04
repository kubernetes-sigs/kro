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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Dependency Readiness", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should wait for all dependencies to be ready before creating dependent resource", func(ctx SpecContext) {
		// This test creates a resource graph with:
		// - ConfigMap A (no dependencies, has readyWhen condition)
		// - ConfigMap B (no dependencies, has readyWhen condition)
		// - Deployment (depends on both ConfigMap A and B)
		// The deployment should only be created after both configmaps satisfy their readyWhen conditions

		rgd := generator.NewResourceGraphDefinition("test-dependency-readiness",
			generator.WithSchema(
				"TestDependencyReadiness", "v1alpha1",
				map[string]interface{}{
					"name": "string",
					"configA": map[string]interface{}{
						"data":  "string",
						"ready": "boolean | default=false",
					},
					"configB": map[string]interface{}{
						"data":  "string",
						"ready": "boolean | default=false",
					},
					"replicas": "integer | default=1",
				},
				nil,
			),
			// ConfigMap A - no dependencies, has readyWhen
			generator.WithResource("configmapA", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-config-a",
				},
				"data": map[string]interface{}{
					"value": "${schema.spec.configA.data}",
					"ready": "${string(schema.spec.configA.ready)}",
				},
			}, []string{"${configmapA.data.?ready.orValue(\"false\") == \"true\"}"}, nil),
			// ConfigMap B - no dependencies, has readyWhen
			generator.WithResource("configmapB", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}-config-b",
				},
				"data": map[string]interface{}{
					"value": "${schema.spec.configB.data}",
					"ready": "${string(schema.spec.configB.ready)}",
				},
			}, []string{"${configmapB.data.?ready.orValue(\"false\") == \"true\"}"}, nil),
			// Deployment - depends on both configmaps
			generator.WithResource("deployment", map[string]interface{}{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
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
									"name":  "nginx",
									"image": "nginx",
									"env": []interface{}{
										map[string]interface{}{
											"name":  "CONFIG_A",
											"value": "${configmapA.data.?value.orValue(\"\")}",
										},
										map[string]interface{}{
											"name":  "CONFIG_B",
											"value": "${configmapB.data.?value.orValue(\"\")}",
										},
									},
								},
							},
						},
					},
				},
			}, nil, nil),
		)

		// Create ResourceGraphDefinition
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Verify ResourceGraphDefinition is created and becomes ready
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify the ResourceGraphDefinition fields
			g.Expect(createdRGD.Spec.Schema.Kind).To(Equal("TestDependencyReadiness"))
			g.Expect(createdRGD.Spec.Schema.APIVersion).To(Equal("v1alpha1"))
			g.Expect(createdRGD.Spec.Resources).To(HaveLen(3))

			// Verify topological order (configmaps should come before deployment)
			g.Expect(createdRGD.Status.TopologicalOrder).To(HaveLen(3))
			g.Expect(createdRGD.Status.TopologicalOrder[2]).To(Equal("deployment"))

			// Verify ready condition
			g.Expect(createdRGD.Status.Conditions).ShouldNot(BeEmpty())
			var readyCondition krov1alpha1.Condition
			for _, cond := range createdRGD.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					readyCondition = cond
				}
			}
			g.Expect(readyCondition).ToNot(BeNil())
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(readyCondition.ObservedGeneration).To(Equal(createdRGD.Generation))

			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		instanceName := "test-dep-readiness"
		// Create instance with both configmaps NOT ready initially
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestDependencyReadiness",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": instanceName,
					"configA": map[string]interface{}{
						"data":  "valueA",
						"ready": false,
					},
					"configB": map[string]interface{}{
						"data":  "valueB",
						"ready": false,
					},
					"replicas": 1,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Check if instance is created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify ConfigMaps are created
		configMapA := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-config-a",
				Namespace: namespace,
			}, configMapA)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMapA.Data["value"]).To(Equal("valueA"))
			g.Expect(configMapA.Data["ready"]).To(Equal("false"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		configMapB := &corev1.ConfigMap{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName + "-config-b",
				Namespace: namespace,
			}, configMapB)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMapB.Data["value"]).To(Equal("valueB"))
			g.Expect(configMapB.Data["ready"]).To(Equal("false"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify Deployment is NOT created yet (dependencies not ready)
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, &appsv1.Deployment{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "deployment should not be created yet"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify instance state is IN_PROGRESS
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("IN_PROGRESS"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update instance spec to set ConfigMap A to ready
		Eventually(func(g Gomega, ctx SpecContext) {
			// Get fresh copy to avoid conflicts
			freshInstance := &unstructured.Unstructured{}
			freshInstance.SetGroupVersionKind(instance.GroupVersionKind())
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())

			err = unstructured.SetNestedField(freshInstance.Object, true, "spec", "configA", "ready")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Verify deployment is still NOT created (ConfigMap B not ready yet)
		Consistently(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, &appsv1.Deployment{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "deployment should still not be created"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Update instance spec to set ConfigMap B to ready
		Eventually(func(g Gomega, ctx SpecContext) {
			// Get fresh copy to avoid conflicts
			freshInstance := &unstructured.Unstructured{}
			freshInstance.SetGroupVersionKind(instance.GroupVersionKind())
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())

			err = unstructured.SetNestedField(freshInstance.Object, true, "spec", "configB", "ready")
			g.Expect(err).ToNot(HaveOccurred())

			err = env.Client.Update(ctx, freshInstance)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		// Now verify Deployment IS created (all dependencies are ready)
		deployment := &appsv1.Deployment{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, deployment)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deployment specs
			g.Expect(deployment.Spec.Template.Spec.Containers).To(HaveLen(1))
			g.Expect(*deployment.Spec.Replicas).To(Equal(int32(1)))

			// Verify environment variables from configmaps
			envVars := deployment.Spec.Template.Spec.Containers[0].Env
			g.Expect(envVars).To(HaveLen(2))

			var foundConfigA, foundConfigB bool
			for _, env := range envVars {
				if env.Name == "CONFIG_A" && env.Value == "valueA" {
					foundConfigA = true
				}
				if env.Name == "CONFIG_B" && env.Value == "valueB" {
					foundConfigB = true
				}
			}
			g.Expect(foundConfigA).To(BeTrue(), "CONFIG_A should be set from configMapA")
			g.Expect(foundConfigB).To(BeTrue(), "CONFIG_B should be set from configMapB")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify instance state becomes ACTIVE once all resources are synced
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			status, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(status).To(Equal("ACTIVE"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
