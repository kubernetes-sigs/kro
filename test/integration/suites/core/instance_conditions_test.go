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
	ctrlinstance "github.com/kubernetes-sigs/kro/pkg/controller/instance"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Instance Conditions", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should have correct hierarchical conditions on successful instance reconciliation", func(ctx SpecContext) {
		// Create a simple ResourceGraphDefinition
		rgd := generator.NewResourceGraphDefinition("test-instance-conditions-success",
			generator.WithSchema(
				"TestInstanceConditions", "v1alpha1",
				map[string]interface{}{
					"configData": "string",
				},
				nil,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}",
				},
				"data": map[string]interface{}{
					"config": "${schema.spec.configData}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to be ready and CRD to be created
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())

			// Debug: print all conditions
			GinkgoWriter.Printf("RGD %s conditions:\n", rgd.Name)
			for _, cond := range rgd.Status.Conditions {
				GinkgoWriter.Printf("  - Type: %s, Status: %s, Reason: %s, Message: %s\n",
					cond.Type, cond.Status,
					func() string {
						if cond.Reason != nil {
							return *cond.Reason
						} else {
							return "nil"
						}
					}(),
					func() string {
						if cond.Message != nil {
							return *cond.Message
						} else {
							return "nil"
						}
					}())
			}

			// Check that RGD has Ready condition
			var readyCondition *krov1alpha1.Condition
			for _, cond := range rgd.Status.Conditions {
				if cond.Type == "Ready" {
					readyCondition = &cond
					break
				}
			}
			g.Expect(readyCondition).ToNot(BeNil(), "Ready condition should exist")
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue), "Ready condition should be True")
		}, 30*time.Second, time.Second).Should(Succeed())

		// Wait a bit more for CRD to be fully registered in API discovery
		time.Sleep(5 * time.Second)

		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestInstanceConditions",
				"metadata": map[string]interface{}{
					"name":      "test-instance-conditions",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"configData": "test-data",
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Verify instance conditions progress through the expected states
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance-conditions",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Get status conditions
			statusConditions, found, _ := unstructured.NestedSlice(instance.Object, "status", "conditions")
			g.Expect(found).To(BeTrue())
			g.Expect(statusConditions).ToNot(BeEmpty())

			// Find the Ready condition
			var readyCondition map[string]interface{}
			for _, condInterface := range statusConditions {
				if cond, ok := condInterface.(map[string]interface{}); ok {
					condType, _ := cond["type"].(string)
					if condType == ctrlinstance.Ready {
						readyCondition = cond
						break
					}
				}
			}

			// Validate Ready condition
			g.Expect(readyCondition).ToNot(BeNil(), "Ready condition should be present")
			g.Expect(readyCondition["status"]).To(Equal("True"), "Ready should be True")
			g.Expect(readyCondition["reason"]).To(Equal("Ready"), "Ready reason should be Ready")

			// Validate observedGeneration is set correctly
			expectedGeneration := instance.GetGeneration()
			g.Expect(readyCondition["observedGeneration"]).To(Equal(expectedGeneration), "Ready observedGeneration should match")

			// Validate lastTransitionTime is set
			g.Expect(readyCondition["lastTransitionTime"]).ToNot(BeNil(), "Ready lastTransitionTime should be set")

		}, 30*time.Second, time.Second).Should(Succeed())

		// Verify the ConfigMap was created
		configMap := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance-conditions",
				Namespace: namespace,
			}, configMap)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(configMap.Data["config"]).To(Equal("test-data"))
		}, 10*time.Second, time.Second).Should(Succeed())
	})

	It("should show proper condition progression during deletion", func(ctx SpecContext) {
		// Create a simple ResourceGraphDefinition
		rgd := generator.NewResourceGraphDefinition("test-instance-conditions-deletion",
			generator.WithSchema(
				"TestInstanceDeletion", "v1alpha1",
				map[string]interface{}{
					"configData": "string",
				},
				nil,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.metadata.name}",
				},
				"data": map[string]interface{}{
					"config": "${schema.spec.configData}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to be ready and CRD to be created
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())

			// Check that RGD has Ready condition
			var readyCondition *krov1alpha1.Condition
			for _, cond := range rgd.Status.Conditions {
				if cond.Type == "Ready" {
					readyCondition = &cond
					break
				}
			}
			g.Expect(readyCondition).ToNot(BeNil())
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		}, 30*time.Second, time.Second).Should(Succeed())

		// Wait a bit more for CRD to be fully registered in API discovery
		time.Sleep(5 * time.Second)

		// Create instance
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestInstanceDeletion",
				"metadata": map[string]interface{}{
					"name":      "test-instance-deletion",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"configData": "delete-test-data",
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Wait for successful reconciliation
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance-deletion",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			statusConditions, found, _ := unstructured.NestedSlice(instance.Object, "status", "conditions")
			g.Expect(found).To(BeTrue())

			var readyCondition map[string]interface{}
			for _, condInterface := range statusConditions {
				if cond, ok := condInterface.(map[string]interface{}); ok {
					if cond["type"] == ctrlinstance.Ready {
						readyCondition = cond
						break
					}
				}
			}
			g.Expect(readyCondition).ToNot(BeNil())
			g.Expect(readyCondition["status"]).To(Equal("True"))
		}, 30*time.Second, time.Second).Should(Succeed())

		// Delete the instance
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// During deletion, conditions should still be present and show deletion progress
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      "test-instance-deletion",
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Verify deletion timestamp is set
			g.Expect(instance.GetDeletionTimestamp()).ToNot(BeNil())

			statusConditions, found, _ := unstructured.NestedSlice(instance.Object, "status", "conditions")
			if found && len(statusConditions) > 0 {
				// If conditions are present during deletion, they should maintain valid structure
				for _, condInterface := range statusConditions {
					if cond, ok := condInterface.(map[string]interface{}); ok {
						g.Expect(cond["type"]).ToNot(BeNil(), "Condition type should not be nil during deletion")
						g.Expect(cond["status"]).ToNot(BeNil(), "Condition status should not be nil during deletion")
					}
				}
			}
		}, 20*time.Second, time.Second).Should(Succeed())
	})
})
