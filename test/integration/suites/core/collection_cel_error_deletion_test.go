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
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Collection CEL Error Deletion", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
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

	It("should delete collection instance despite CEL error in forEach identity path", func(ctx SpecContext) {
		// Create RGD with CEL error in forEach collection identity path
		rgd := generator.NewResourceGraphDefinition("cel-error-test",
			generator.WithSchema(
				"CelErrorTest", "v1alpha1",
				map[string]interface{}{
					"counts": "[]integer",
				},
				nil,
			),
			generator.WithResourceCollection("configmaps", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					// CEL error: division by zero in identity path
					"name":      "${\"test-\" + string(schema.spec.counts[idx] / 0)}",
					"namespace": "${schema.metadata.namespace}",
				},
				"data": map[string]interface{}{
					"index": "${string(idx)}",
				},
			},
				[]krov1alpha1.ForEachDimension{
					{"idx": "${lists.range(size(schema.spec.counts))}"},
				},
				nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to become active
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))

			var readyCondition krov1alpha1.Condition
			for _, cond := range createdRGD.Status.Conditions {
				if cond.Type == resourcegraphdefinition.Ready {
					readyCondition = cond
				}
			}
			g.Expect(readyCondition.Status).To(Equal(metav1.ConditionTrue))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Create instance with values that will trigger CEL error
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "CelErrorTest",
				"metadata": map[string]interface{}{
					"name":      "test-instance",
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"counts": []interface{}{5, 10, 15},
				},
			},
		}

		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		// Instance should fail to reconcile due to CEL error
		Eventually(func(g Gomega, ctx SpecContext) {
			var updated unstructured.Unstructured
			updated.SetGroupVersionKind(instance.GroupVersionKind())
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, &updated)).To(Succeed())

			// Should have error in status
			status, ok := updated.Object["status"].(map[string]interface{})
			g.Expect(ok).To(BeTrue())

			// Check for error in conditions
			conditions, ok := status["conditions"].([]interface{})
			g.Expect(ok).To(BeTrue())
			g.Expect(len(conditions)).To(BeNumerically(">", 0))

			hasError := false
			for _, c := range conditions {
				cond := c.(map[string]interface{})
				if status, ok := cond["status"].(string); ok && status == "False" {
					hasError = true
					break
				}
			}
			g.Expect(hasError).To(BeTrue())
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Now delete the instance - this is the key test
		// Before the fix, this would hang forever due to CEL evaluation failure
		Expect(env.Client.Delete(ctx, instance)).To(Succeed())

		// Instance should be deleted successfully despite CEL error
		Eventually(func(g Gomega, ctx SpecContext) {
			var updated unstructured.Unstructured
			updated.SetGroupVersionKind(instance.GroupVersionKind())
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      instance.GetName(),
				Namespace: namespace,
			}, &updated)
			g.Expect(errors.IsNotFound(err)).To(BeTrue())
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
