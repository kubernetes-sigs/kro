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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/awslabs/kro/api/v1alpha1"
	"github.com/awslabs/kro/internal/testutil/generator"
)

var _ = Describe("Status", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		// Create namespace
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should have correct conditions when ResourceGroup is created", func() {
		rg := generator.NewResourceGroup("test-status",
			generator.WithNamespace(namespace),
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

		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		// Verify ResourceGroup status
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, rg)
			g.Expect(err).ToNot(HaveOccurred())

			// Check conditions
			g.Expect(rg.Status.Conditions).To(HaveLen(3))
			g.Expect(rg.Status.State).To(Equal(krov1alpha1.ResourceGroupStateActive))

			for _, cond := range rg.Status.Conditions {
				g.Expect(cond.Status).To(Equal(metav1.ConditionTrue))
			}

		}, 10*time.Second, time.Second).Should(Succeed())
	})

	It("should reflect failure conditions when definition is invalid", func() {
		rg := generator.NewResourceGroup("test-status-fail",
			generator.WithNamespace(namespace),
			generator.WithSchema(
				"TestStatusFail", "v1alpha1",
				map[string]interface{}{
					"field1": "invalid-type", // Invalid type
				},
				nil,
			),
		)

		Expect(env.Client.Create(ctx, rg)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{
				Name:      rg.Name,
				Namespace: namespace,
			}, rg)
			g.Expect(err).ToNot(HaveOccurred())

			g.Expect(rg.Status.State).To(Equal(krov1alpha1.ResourceGroupStateInactive))

			// Check specific failure condition
			var crdCondition *krov1alpha1.Condition
			for _, cond := range rg.Status.Conditions {
				if cond.Type == krov1alpha1.ResourceGroupConditionTypeGraphVerified {
					crdCondition = &cond
					break
				}
			}

			g.Expect(crdCondition).ToNot(BeNil())
			g.Expect(crdCondition.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(*crdCondition.Reason).To(ContainSubstring("Error"))
		}, 10*time.Second, time.Second).Should(Succeed())
	})
})
