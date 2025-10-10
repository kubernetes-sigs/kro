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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Status", func() {
	DescribeTableSubtree("apply mode",
		testStatus,
		Entry(string(krov1alpha1.ResourceGraphDefinitionReconcileModeApplySet),
			krov1alpha1.ResourceGraphDefinitionReconcileSpec{
				Mode: krov1alpha1.ResourceGraphDefinitionReconcileModeApplySet,
			}, Label(string(krov1alpha1.ResourceGraphDefinitionReconcileModeApplySet)),
		),
		Entry(string(krov1alpha1.ResourceGraphDefinitionReconcileModeClientSideDelta),
			krov1alpha1.ResourceGraphDefinitionReconcileSpec{
				Mode: krov1alpha1.ResourceGraphDefinitionReconcileModeClientSideDelta,
			}, Label(string(krov1alpha1.ResourceGraphDefinitionReconcileModeClientSideDelta)),
		),
	)
})

func testStatus(reconcileSpec krov1alpha1.ResourceGraphDefinitionReconcileSpec) {
	It("should have correct conditions when ResourceGraphDefinition is created", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-status",
			generator.WithReconcileSpec(reconcileSpec),
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
			generator.WithReconcileSpec(reconcileSpec),
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
}
