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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Status", func() {
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

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should have correct conditions when ResourceGraphDefinition is created", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-status",
			generator.WithSchema(
				"TestStatus", "v1alpha1",
				map[string]interface{}{
					"field1": "string",
				},
				nil,
			),
			generator.WithResource("res1", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.field1}",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Verify ResourceGraphDefinition status
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, rgd)
			g.Expect(err).ToNot(HaveOccurred())

			// Check conditions
			g.Expect(rgd.Status.Conditions).To(Not(BeNil()))
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

			for _, cond := range rgd.Status.Conditions {
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}

		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should reflect failure conditions when definition is invalid", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-status-fail",
			generator.WithSchema(
				"TestStatusFail", "v1alpha1",
				map[string]interface{}{
					"field1": "invalid-type", // Invalid type
				},
				nil,
			),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		//nolint:dupl // we have many test cases checking for inactivity but with different conditions
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name: rgd.Name,
			}, rgd)
			g.Expect(err).ToNot(HaveOccurred())

			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))

			// Check specific failure condition
			var crdCondition *krov1alpha1.Condition
			for _, cond := range rgd.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					crdCondition = &cond
					break
				}
			}

			g.Expect(crdCondition).ToNot(BeNil())
			g.Expect(crdCondition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(*crdCondition.Message).To(ContainSubstring("failed to build resourcegraphdefinition"))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should interpolate string templates in instance status", func(ctx SpecContext) {
		// This test verifies that status fields with string templates like
		// "${configmap.metadata.name}-${configmap.metadata.namespace}" are properly
		// interpolated to produce the final string value.
		rgd := generator.NewResourceGraphDefinition("test-status-interpolation",
			generator.WithSchema(
				"StatusInterpolation", "v1alpha1",
				map[string]interface{}{
					"name": "string",
				},
				// Status with string template (multiple expressions)
				map[string]interface{}{
					"configmapRef": "${configmap.metadata.name}-in-${configmap.metadata.namespace}",
				},
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"data": map[string]interface{}{
					"key": "value",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for RGD to be active
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance
		instanceName := "test-interpolation"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "StatusInterpolation",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name": "my-configmap",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		// Verify the status field is properly interpolated
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())

			// Check that the string template was properly interpolated
			configmapRef, found, err := unstructured.NestedString(instance.Object, "status", "configmapRef")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue(), "status.configmapRef should exist")
			// Expected format: "my-configmap-in-<namespace>"
			g.Expect(configmapRef).To(Equal(fmt.Sprintf("my-configmap-in-%s", namespace)),
				"status.configmapRef should be properly interpolated from string template")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should only show status fields when all referenced resources are available", func(ctx SpecContext) {
		// This test verifies that:
		// 1. Status fields with string interpolation only appear when ALL referenced resources exist
		// 2. Partial resolution correctly handles different dependency combinations
		//
		// Setup:
		// - 3 ConfigMaps (cm1, cm2, cm3) each controlled by includeWhen
		// - 3 status fields with cascading dependencies:
		//   - field1: "${cm1.data.value}" - depends on cm1 only
		//   - field2: "${cm1.data.value}-${cm2.data.value}" - depends on cm1 AND cm2
		//   - field3: "${cm1.data.value}-${cm2.data.value}-${cm3.data.value}" - depends on all
		rgd := generator.NewResourceGraphDefinition("test-status-partial",
			generator.WithSchema(
				"StatusPartial", "v1alpha1",
				map[string]interface{}{
					"includeCm1": "boolean",
					"includeCm2": "boolean",
					"includeCm3": "boolean",
				},
				map[string]interface{}{
					"field1": "${cm1.data.value}",
					"field2": "${cm1.data.value}-${cm2.data.value}",
					"field3": "${cm1.data.value}-${cm2.data.value}-${cm3.data.value}",
				},
			),
			generator.WithResource("cm1", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "cm1",
				},
				"data": map[string]interface{}{
					"value": "one",
				},
			}, nil, []string{"${schema.spec.includeCm1}"}),
			generator.WithResource("cm2", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "cm2",
				},
				"data": map[string]interface{}{
					"value": "two",
				},
			}, nil, []string{"${schema.spec.includeCm2}"}),
			generator.WithResource("cm3", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "cm3",
				},
				"data": map[string]interface{}{
					"value": "three",
				},
			}, nil, []string{"${schema.spec.includeCm3}"}),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		// Wait for RGD to be active
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance with all ConfigMaps disabled initially
		instanceName := "test-partial"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "StatusPartial",
				"metadata": map[string]interface{}{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"includeCm1": false,
					"includeCm2": false,
					"includeCm3": false,
				},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, instance)).To(Succeed())
		})

		getStatus := func(g Gomega, ctx SpecContext) map[string]interface{} {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instanceName,
				Namespace: namespace,
			}, instance)
			g.Expect(err).ToNot(HaveOccurred())
			status, _, _ := unstructured.NestedMap(instance.Object, "status")
			return status
		}

		// State 1: All disabled - no status fields should exist
		Eventually(func(g Gomega, ctx SpecContext) {
			status := getStatus(g, ctx)
			// Status might be nil or empty, or have only conditions
			_, hasField1 := status["field1"]
			_, hasField2 := status["field2"]
			_, hasField3 := status["field3"]
			g.Expect(hasField1).To(BeFalse(), "field1 should not exist when cm1 is disabled")
			g.Expect(hasField2).To(BeFalse(), "field2 should not exist when cm1/cm2 are disabled")
			g.Expect(hasField3).To(BeFalse(), "field3 should not exist when all cms are disabled")
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// State 2: Enable cm1 only - field1 should appear
		instance.Object["spec"].(map[string]interface{})["includeCm1"] = true
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			status := getStatus(g, ctx)
			field1, hasField1 := status["field1"]
			_, hasField2 := status["field2"]
			_, hasField3 := status["field3"]
			g.Expect(hasField1).To(BeTrue(), "field1 should exist when cm1 is enabled")
			g.Expect(field1).To(Equal("one"))
			g.Expect(hasField2).To(BeFalse(), "field2 should not exist when cm2 is disabled")
			g.Expect(hasField3).To(BeFalse(), "field3 should not exist when cm2/cm3 are disabled")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// State 3: Enable cm1 and cm2 - field1 and field2 should appear
		instance.Object["spec"].(map[string]interface{})["includeCm2"] = true
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			status := getStatus(g, ctx)
			field1, hasField1 := status["field1"]
			field2, hasField2 := status["field2"]
			_, hasField3 := status["field3"]
			g.Expect(hasField1).To(BeTrue(), "field1 should exist")
			g.Expect(field1).To(Equal("one"))
			g.Expect(hasField2).To(BeTrue(), "field2 should exist when cm1 and cm2 are enabled")
			g.Expect(field2).To(Equal("one-two"))
			g.Expect(hasField3).To(BeFalse(), "field3 should not exist when cm3 is disabled")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// State 4: Enable all - all fields should appear
		instance.Object["spec"].(map[string]interface{})["includeCm3"] = true
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			status := getStatus(g, ctx)
			field1, hasField1 := status["field1"]
			field2, hasField2 := status["field2"]
			field3, hasField3 := status["field3"]
			g.Expect(hasField1).To(BeTrue(), "field1 should exist")
			g.Expect(field1).To(Equal("one"))
			g.Expect(hasField2).To(BeTrue(), "field2 should exist")
			g.Expect(field2).To(Equal("one-two"))
			g.Expect(hasField3).To(BeTrue(), "field3 should exist when all cms are enabled")
			g.Expect(field3).To(Equal("one-two-three"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// State 5: Disable cm2 - field2 and field3 should disappear, field1 remains
		instance.Object["spec"].(map[string]interface{})["includeCm2"] = false
		Expect(env.Client.Update(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			status := getStatus(g, ctx)
			field1, hasField1 := status["field1"]
			_, hasField2 := status["field2"]
			_, hasField3 := status["field3"]
			g.Expect(hasField1).To(BeTrue(), "field1 should still exist (cm1 is enabled)")
			g.Expect(field1).To(Equal("one"))
			g.Expect(hasField2).To(BeFalse(), "field2 should disappear when cm2 is disabled")
			g.Expect(hasField3).To(BeFalse(), "field3 should disappear when cm2 is disabled")
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
