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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	internalv1alpha1 "github.com/kubernetes-sigs/kro/api/internal.kro.run/v1alpha1"
	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/apis"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
)

var _ = Describe("RGD Conditions", func() {
	It("should report the exact success condition contract once an RGD is active", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("rgd-conds-%s", rand.String(5))
		kind := fmt.Sprintf("RGDConditions%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		createConditionTestRGD(ctx, rgd)
		expectExactRGDConditions(ctx, rgdName, time.Second, exactRGDExpectation{
			state:      krov1alpha1.ResourceGraphDefinitionStateActive,
			lastIssued: ptrToInt64(1),
			conditions: map[krov1alpha1.ConditionType]conditionExpectation{
				krov1alpha1.ConditionType(apis.ConditionReady): {
					status:  metav1.ConditionTrue,
					reason:  apis.ConditionReady,
					message: "",
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.KindReady): {
					status:  metav1.ConditionTrue,
					reason:  "Ready",
					message: fmt.Sprintf("kind %s has been accepted and ready", kind),
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.ControllerReady): {
					status:  metav1.ConditionTrue,
					reason:  "Running",
					message: "controller is running",
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphAccepted): {
					status:  metav1.ConditionTrue,
					reason:  "Valid",
					message: "resource graph and schema are valid",
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphRevisionsResolved): {
					status:  metav1.ConditionTrue,
					reason:  "Resolved",
					message: "revision 1 compiled and active",
				},
			},
		})
	})

	It("should converge to ready after the first revision is compiled", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("rgd-conds-pending-%s", rand.String(5))
		kind := fmt.Sprintf("RGDPendingConditions%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		createConditionTestRGD(ctx, rgd)

		expectRGDConditions(ctx, rgdName, time.Second, rgdExpectation{
			state:      krov1alpha1.ResourceGraphDefinitionStateActive,
			lastIssued: ptrToInt64(1),
			conditions: map[string]metav1.ConditionStatus{
				apis.ConditionReady:                            metav1.ConditionTrue,
				resourcegraphdefinition.KindReady:              metav1.ConditionTrue,
				resourcegraphdefinition.ControllerReady:        metav1.ConditionTrue,
				resourcegraphdefinition.GraphAccepted:          metav1.ConditionTrue,
				resourcegraphdefinition.GraphRevisionsResolved: metav1.ConditionTrue,
			},
		})
	})

	It("should report the exact failure condition contract when the initial RGD spec is invalid", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("rgd-conds-invalid-%s", rand.String(5))
		kind := fmt.Sprintf("RGDInvalidConditions%s", rand.String(5))
		rgd := unknownReferenceRGD(rgdName, kind)

		createConditionTestRGD(ctx, rgd)
		expectExactRGDConditions(ctx, rgdName, time.Second, exactRGDExpectation{
			state: krov1alpha1.ResourceGraphDefinitionStateInactive,
			conditions: map[krov1alpha1.ConditionType]conditionExpectation{
				krov1alpha1.ConditionType(apis.ConditionReady): {
					status:  metav1.ConditionFalse,
					reason:  "InvalidResourceGraph",
					message: unknownReferenceRGDErrorMessage(),
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.KindReady): {
					status:  metav1.ConditionUnknown,
					reason:  "AwaitingReconciliation",
					message: `condition "KindReady" is awaiting reconciliation`,
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.ControllerReady): {
					status:  metav1.ConditionUnknown,
					reason:  "AwaitingReconciliation",
					message: `condition "ControllerReady" is awaiting reconciliation`,
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphAccepted): {
					status:  metav1.ConditionFalse,
					reason:  "InvalidResourceGraph",
					message: unknownReferenceRGDErrorMessage(),
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphRevisionsResolved): {
					status:  metav1.ConditionUnknown,
					reason:  "AwaitingReconciliation",
					message: `condition "GraphRevisionsResolved" is awaiting reconciliation`,
				},
			},
		})
	})

	It("should converge to ready after a valid update issues a new revision", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("rgd-conds-update-%s", rand.String(5))
		kind := fmt.Sprintf("RGDUpdateConditions%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		createConditionTestRGD(ctx, rgd)

		waitForRGDActive(ctx, rgdName)

		updateRGDTemplate(ctx, rgdName, "rev-2")

		expectRGDConditions(ctx, rgdName, time.Second, rgdExpectation{
			state:      krov1alpha1.ResourceGraphDefinitionStateActive,
			lastIssued: ptrToInt64(2),
			conditions: map[string]metav1.ConditionStatus{
				apis.ConditionReady:                            metav1.ConditionTrue,
				resourcegraphdefinition.KindReady:              metav1.ConditionTrue,
				resourcegraphdefinition.ControllerReady:        metav1.ConditionTrue,
				resourcegraphdefinition.GraphAccepted:          metav1.ConditionTrue,
				resourcegraphdefinition.GraphRevisionsResolved: metav1.ConditionTrue,
			},
		})
	})

	It("should report the exact failure condition contract when a later RGD update is invalid", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("rgd-conds-invalid-update-%s", rand.String(5))
		kind := fmt.Sprintf("RGDInvalidUpdateConditions%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		createConditionTestRGD(ctx, rgd)

		waitForRGDActive(ctx, rgdName)

		invalidRGD := unknownReferenceRGD(rgdName, kind)
		Eventually(func(g Gomega) {
			fresh := &krov1alpha1.ResourceGraphDefinition{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
			g.Expect(err).ToNot(HaveOccurred())
			fresh.Spec = *invalidRGD.Spec.DeepCopy()
			err = env.Client.Update(ctx, fresh)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		expectExactRGDConditions(ctx, rgdName, time.Second, exactRGDExpectation{
			state:      krov1alpha1.ResourceGraphDefinitionStateInactive,
			lastIssued: ptrToInt64(1),
			conditions: map[krov1alpha1.ConditionType]conditionExpectation{
				krov1alpha1.ConditionType(apis.ConditionReady): {
					status:  metav1.ConditionFalse,
					reason:  "InvalidResourceGraph",
					message: unknownReferenceRGDErrorMessage(),
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.KindReady): {
					status:                   metav1.ConditionTrue,
					reason:                   "Ready",
					message:                  fmt.Sprintf("kind %s has been accepted and ready", kind),
					observedGenerationOffset: -1,
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.ControllerReady): {
					status:                   metav1.ConditionTrue,
					reason:                   "Running",
					message:                  "controller is running",
					observedGenerationOffset: -1,
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphAccepted): {
					status:  metav1.ConditionFalse,
					reason:  "InvalidResourceGraph",
					message: unknownReferenceRGDErrorMessage(),
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphRevisionsResolved): {
					status:  metav1.ConditionTrue,
					reason:  "Resolved",
					message: "revision 1 compiled and active",
				},
			},
		})

		Consistently(func(g Gomega) {
			g.Expect(listGraphRevisions(ctx, rgdName)).To(HaveLen(1))
		}, 5*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should surface unknown serving conditions while terminating revisions settle", func(ctx SpecContext) {
		rgdName := fmt.Sprintf("rgd-conds-settle-%s", rand.String(5))
		kind := fmt.Sprintf("RGDSettleConditions%s", rand.String(5))
		rgd := configmapRGD(rgdName, kind)

		createConditionTestRGD(ctx, rgd)

		waitForRGDActive(ctx, rgdName)

		var grName string
		Eventually(func(g Gomega) {
			grs := listGraphRevisions(ctx, rgdName)
			g.Expect(grs).To(HaveLen(1))
			grName = grs[0].Name
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		DeferCleanup(func(ctx SpecContext) {
			gr := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: grName}, gr)
			if apierrors.IsNotFound(err) {
				return
			}
			Expect(err).ToNot(HaveOccurred())
			gr.Finalizers = nil
			Expect(env.Client.Update(ctx, gr)).To(Succeed())
			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{Name: grName}, &internalv1alpha1.GraphRevision{})
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}, 20*time.Second, 100*time.Millisecond).WithContext(ctx).Should(Succeed())
		})

		Eventually(func(g Gomega) {
			gr := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: grName}, gr)
			g.Expect(err).ToNot(HaveOccurred())
			gr.Finalizers = append(gr.Finalizers, "test.kro.run/stuck")
			err = env.Client.Update(ctx, gr)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, 100*time.Millisecond).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			gr := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: grName}, gr)
			g.Expect(err).ToNot(HaveOccurred())
			err = env.Client.Delete(ctx, gr)
			if err != nil {
				g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			}
		}, 10*time.Second, 100*time.Millisecond).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			gr := &internalv1alpha1.GraphRevision{}
			err := env.Client.Get(ctx, types.NamespacedName{Name: grName}, gr)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(gr.GetDeletionTimestamp().IsZero()).To(BeFalse())
		}, 10*time.Second, 100*time.Millisecond).WithContext(ctx).Should(Succeed())

		updateRGDTemplate(ctx, rgdName, "settling")

		expectExactRGDConditions(ctx, rgdName, 100*time.Millisecond, exactRGDExpectation{
			state:      krov1alpha1.ResourceGraphDefinitionStateActive,
			lastIssued: ptrToInt64(1),
			conditions: map[krov1alpha1.ConditionType]conditionExpectation{
				krov1alpha1.ConditionType(apis.ConditionReady): {
					status:  metav1.ConditionUnknown,
					reason:  "WaitingForGraphRevisionSettlement",
					message: "waiting for terminating graph revisions to settle",
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphAccepted): {
					status:                   metav1.ConditionTrue,
					reason:                   "Valid",
					message:                  "resource graph and schema are valid",
					observedGenerationOffset: -1,
				},
				krov1alpha1.ConditionType(resourcegraphdefinition.GraphRevisionsResolved): {
					status:  metav1.ConditionUnknown,
					reason:  "WaitingForGraphRevisionSettlement",
					message: "waiting for terminating graph revisions to settle",
				},
			},
		})
	})
})

type exactRGDExpectation struct {
	state      krov1alpha1.ResourceGraphDefinitionState
	lastIssued *int64
	conditions map[krov1alpha1.ConditionType]conditionExpectation
}

func expectExactRGDConditions(
	ctx SpecContext,
	rgdName string,
	interval time.Duration,
	want exactRGDExpectation,
) {
	Eventually(func(g Gomega) {
		fresh := &krov1alpha1.ResourceGraphDefinition{}
		err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
		g.Expect(err).ToNot(HaveOccurred())

		if want.state != "" {
			g.Expect(fresh.Status.State).To(Equal(want.state))
		}
		if want.lastIssued != nil {
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(*want.lastIssued))
		}
		for conditionType, expectation := range want.conditions {
			assertConditionContract(g, fresh, conditionType, expectation)
		}
	}, 20*time.Second, interval).WithContext(ctx).Should(Succeed())
}

type rgdExpectation struct {
	state           krov1alpha1.ResourceGraphDefinitionState
	lastIssued      *int64
	conditions      map[string]metav1.ConditionStatus
	reasonCondition string
	reason          string
}

func createConditionTestRGD(ctx SpecContext, rgd *krov1alpha1.ResourceGraphDefinition) {
	Expect(env.Client.Create(ctx, rgd)).To(Succeed())
	DeferCleanup(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
	})
}

func expectRGDConditions(
	ctx SpecContext,
	rgdName string,
	interval time.Duration,
	want rgdExpectation,
) {
	Eventually(func(g Gomega) {
		fresh := &krov1alpha1.ResourceGraphDefinition{}
		err := env.Client.Get(ctx, types.NamespacedName{Name: rgdName}, fresh)
		g.Expect(err).ToNot(HaveOccurred())

		if want.state != "" {
			g.Expect(fresh.Status.State).To(Equal(want.state))
		}
		if want.lastIssued != nil {
			g.Expect(fresh.Status.LastIssuedRevision).To(Equal(*want.lastIssued))
		}
		for conditionType, status := range want.conditions {
			assertRGDConditionStatusOnly(g, fresh, conditionType, status)
		}
		if want.reasonCondition == "" {
			return
		}

		cond := findRGDCondition(
			fresh.Status.Conditions,
			krov1alpha1.ConditionType(want.reasonCondition),
		)
		g.Expect(cond).ToNot(BeNil())
		g.Expect(cond.Reason).ToNot(BeNil())
		g.Expect(*cond.Reason).To(Equal(want.reason))
	}, 20*time.Second, interval).WithContext(ctx).Should(Succeed())
}

func ptrToInt64(v int64) *int64 {
	return &v
}

func assertRGDConditionStatusOnly(
	g Gomega,
	rgd *krov1alpha1.ResourceGraphDefinition,
	conditionType string,
	status metav1.ConditionStatus,
) {
	g.ExpectWithOffset(1, rgd.GetGeneration()).To(BeNumerically(">", 0))

	cond := findRGDCondition(rgd.Status.Conditions, krov1alpha1.ConditionType(conditionType))
	g.ExpectWithOffset(1, cond).ToNot(BeNil(), "expected RGD condition %s", conditionType)
	g.ExpectWithOffset(1, cond.Status).To(Equal(status), "unexpected status for condition %s", conditionType)
	g.ExpectWithOffset(
		1,
		cond.LastTransitionTime,
	).ToNot(BeNil(), "LastTransitionTime must be set for condition %s", conditionType)
	g.ExpectWithOffset(
		1,
		cond.LastTransitionTime.Time,
	).ToNot(BeZero(), "LastTransitionTime must not be zero for condition %s", conditionType)
	g.ExpectWithOffset(
		1,
		cond.Message,
	).ToNot(BeNil(), "Message must be set for condition %s", conditionType)
}

type conditionExpectation struct {
	status                   metav1.ConditionStatus
	reason                   string
	message                  string
	observedGenerationOffset int64
}

func assertConditionContract(
	g Gomega,
	obj apis.Object,
	conditionType krov1alpha1.ConditionType,
	want conditionExpectation,
) {
	g.ExpectWithOffset(1, obj.GetGeneration()).To(BeNumerically(">", 0))

	cond := findConditionByType(obj.GetConditions(), conditionType)
	g.ExpectWithOffset(1, cond).ToNot(BeNil(), "expected condition %s", conditionType)
	g.ExpectWithOffset(1, cond.Status).To(Equal(want.status), "unexpected status for condition %s", conditionType)
	g.ExpectWithOffset(1, cond.ObservedGeneration).To(
		Equal(obj.GetGeneration()+want.observedGenerationOffset),
		"unexpected observedGeneration for condition %s",
		conditionType,
	)
	g.ExpectWithOffset(1, cond.LastTransitionTime).ToNot(
		BeNil(),
		"LastTransitionTime must be set for condition %s",
		conditionType,
	)
	g.ExpectWithOffset(1, cond.LastTransitionTime.Time).ToNot(
		BeZero(),
		"LastTransitionTime must not be zero for condition %s",
		conditionType,
	)
	g.ExpectWithOffset(1, cond.Reason).ToNot(BeNil(), "Reason must be set for condition %s", conditionType)
	g.ExpectWithOffset(1, cond.Message).ToNot(BeNil(), "Message must be set for condition %s", conditionType)
	g.ExpectWithOffset(1, *cond.Reason).To(Equal(want.reason), "unexpected reason for condition %s", conditionType)
	g.ExpectWithOffset(1, *cond.Message).To(Equal(want.message), "unexpected message for condition %s", conditionType)
}

func findConditionByType(conditions []krov1alpha1.Condition, t krov1alpha1.ConditionType) *krov1alpha1.Condition {
	for i := range conditions {
		if conditions[i].Type == t {
			return &conditions[i]
		}
	}
	return nil
}

func unknownReferenceRGD(name, kind string) *krov1alpha1.ResourceGraphDefinition {
	rgd := configmapRGD(name, kind)
	rgd.Spec.Resources[0].IncludeWhen = []string{"${missing.status.state == 'available'}"}
	return rgd
}

func unknownReferenceRGDErrorMessage() string {
	return "failed to build dependency graph: " +
		"failed to extract dependencies from includeWhen: " +
		"references unknown identifiers: [missing]"
}

func unknownReferenceGraphRevisionErrorMessage(grName string) string {
	return fmt.Sprintf("failed to compile graph revision %q: %s", grName, unknownReferenceRGDErrorMessage())
}

func newGraphRevisionFromRGD(
	name string,
	revision int64,
	rgd *krov1alpha1.ResourceGraphDefinition,
) *internalv1alpha1.GraphRevision {
	return &internalv1alpha1.GraphRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: internalv1alpha1.GraphRevisionSpec{
			Revision: revision,
			Snapshot: internalv1alpha1.ResourceGraphDefinitionSnapshot{
				Name:       rgd.Name,
				Generation: rgd.Generation,
				Spec:       *rgd.Spec.DeepCopy(),
			},
		},
	}
}
