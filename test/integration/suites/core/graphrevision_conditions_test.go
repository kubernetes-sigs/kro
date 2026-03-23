// Copyright 2026 The Kubernetes Authors.
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
)

var _ = Describe("GraphRevision Conditions", func() {
	It("should report the exact success condition contract for a valid GraphRevision snapshot", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gr-conds-valid-%s", rand.String(5))
		kind := fmt.Sprintf("GRConditionsValid%s", rand.String(5))
		grName := fmt.Sprintf("gr-conditions-valid-%s", rand.String(5))

		gr := newGraphRevisionFromRGD(grName, 7, configmapRGD(rgdName, kind))

		Expect(env.Client.Create(ctx, gr)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, gr)).To(Succeed())
		})

		Eventually(func(g Gomega) {
			fresh := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: grName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())

			assertConditionContract(g, fresh, internalv1alpha1.GraphRevisionConditionTypeGraphVerified, conditionExpectation{
				status:  metav1.ConditionTrue,
				reason:  "Verified",
				message: "graph revision compiled and verified",
			})
			assertConditionContract(g, fresh, krov1alpha1.ConditionType(apis.ConditionReady), conditionExpectation{
				status:  metav1.ConditionTrue,
				reason:  apis.ConditionReady,
				message: "",
			})
			g.Expect(fresh.Status.TopologicalOrder).To(Equal([]string{"configmap"}))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should report the exact failure condition contract for an invalid GraphRevision snapshot", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("gr-conds-invalid-%s", rand.String(5))
		kind := fmt.Sprintf("GRConditionsInvalid%s", rand.String(5))
		grName := fmt.Sprintf("gr-conditions-invalid-%s", rand.String(5))

		gr := newGraphRevisionFromRGD(grName, 9, unknownReferenceRGD(rgdName, kind))

		Expect(env.Client.Create(ctx, gr)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, gr)).To(Succeed())
		})

		wantMessage := unknownReferenceGraphRevisionErrorMessage(grName)

		Eventually(func(g Gomega) {
			fresh := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: grName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())

			assertConditionContract(g, fresh, internalv1alpha1.GraphRevisionConditionTypeGraphVerified, conditionExpectation{
				status:  metav1.ConditionFalse,
				reason:  "InvalidGraph",
				message: wantMessage,
			})
			assertConditionContract(g, fresh, krov1alpha1.ConditionType(apis.ConditionReady), conditionExpectation{
				status:  metav1.ConditionFalse,
				reason:  "InvalidGraph",
				message: wantMessage,
			})
			g.Expect(fresh.Status.TopologicalOrder).To(BeEmpty())
			g.Expect(fresh.Status.Resources).To(BeEmpty())
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
