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

// This suite pins the customer-facing behavior for the p1 regression: when an
// author status OBJECT field is sourced entirely from an externalRef that is
// temporarily missing, the instance must stay IN_PROGRESS with the object
// field trimmed away — NOT flip to ERROR with a frozen, stale status.
//
// Before the fix, rendering under TolerateDataPending left the object as an
// empty {} (all children omitted). The empty object is invalid against the
// generated CRD status schema, so the server-side status apply is rejected
// with 422 "status.<obj>: Invalid", the instance goes ERROR, and the status
// freezes at its last good value. The fix drops the emptied object so the
// apply stays valid and SSA prunes the previously-owned field.
var _ = Describe("Status Partial Object Pending", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("keeps the instance IN_PROGRESS and trims the object when its externalRef source disappears", func(ctx SpecContext) {
		// RGD: a spec-sourced status field (always resolvable) plus a nested
		// object whose children are ALL sourced from an externalRef ConfigMap.
		rgd := generator.NewResourceGraphDefinition("test-partial-status-object",
			generator.WithSchema(
				"TestPartialStatusObject", "v1alpha1",
				map[string]any{
					"tag": "string",
				},
				map[string]any{
					// Always resolvable from spec — the sibling that proves the
					// status was written (not frozen) during the pending window.
					"tag": "${schema.spec.tag}",
					// Every child sourced from the externalRef — goes fully
					// data-pending when the ConfigMap is deleted.
					"extInfo": map[string]any{
						"source": "${ext.metadata.name}",
						"value":  "${ext.data.value}",
					},
				},
			),
			generator.WithExternalRef("ext", &krov1alpha1.ExternalRef{
				APIVersion: "v1",
				Kind:       "ConfigMap",
				Metadata: krov1alpha1.ExternalRefMetadata{
					Name: "partial-status-ext",
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		By("waiting for the RGD to become Active")
		Eventually(func(g Gomega, ctx SpecContext) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, rgd)
			g.Expect(err).ToNot(HaveOccurred())
			var ready *krov1alpha1.Condition
			for i := range rgd.Status.Conditions {
				if rgd.Status.Conditions[i].Type == resourcegraphdefinition.Ready {
					ready = &rgd.Status.Conditions[i]
					break
				}
			}
			g.Expect(ready).ToNot(BeNil())
			g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		gvk := fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1")
		instanceName := "partial-status-inst"
		instance := &unstructured.Unstructured{
			Object: map[string]any{
				"apiVersion": gvk,
				"kind":       "TestPartialStatusObject",
				"metadata": map[string]any{
					"name":      instanceName,
					"namespace": namespace,
				},
				"spec": map[string]any{"tag": "v1"},
			},
		}

		// Helper: fetch the live instance.
		getInstance := func(g Gomega, ctx SpecContext) *unstructured.Unstructured {
			got := &unstructured.Unstructured{}
			got.SetAPIVersion(gvk)
			got.SetKind("TestPartialStatusObject")
			g.Expect(env.Client.Get(ctx, types.NamespacedName{
				Name: instanceName, Namespace: namespace,
			}, got)).To(Succeed())
			return got
		}

		extCM := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "partial-status-ext", Namespace: namespace},
			Data:       map[string]string{"value": "from-external"},
		}

		By("(0) seeding the externalRef ConfigMap so every status field resolves")
		Expect(env.Client.Create(ctx, extCM)).To(Succeed())

		Expect(env.Client.Create(ctx, instance)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			_ = env.Client.Delete(ctx, instance)
		})

		By("(1) with the ext present, the full status object is projected")
		Eventually(func(g Gomega, ctx SpecContext) {
			got := getInstance(g, ctx)
			tag, _, _ := unstructured.NestedString(got.Object, "status", "tag")
			g.Expect(tag).To(Equal("v1"))
			src, found, _ := unstructured.NestedString(got.Object, "status", "extInfo", "source")
			g.Expect(found).To(BeTrue(), "extInfo.source should be present while ext exists")
			g.Expect(src).To(Equal("partial-status-ext"))
			val, _, _ := unstructured.NestedString(got.Object, "status", "extInfo", "value")
			g.Expect(val).To(Equal("from-external"))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		By("(2) deleting the externalRef makes the ext-sourced object fully data-pending")
		Expect(env.Client.Delete(ctx, extCM)).To(Succeed())

		By("(2a) the instance MUST stay IN_PROGRESS — not flip to ERROR")
		// The core regression assertion: the emptied extInfo object must be
		// dropped so the status apply stays schema-valid. If the bug is
		// present, the apply is rejected 422 and state becomes ERROR.
		Eventually(func(g Gomega, ctx SpecContext) {
			got := getInstance(g, ctx)
			state, found, _ := unstructured.NestedString(got.Object, "status", "state")
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("IN_PROGRESS"),
				"instance must stay IN_PROGRESS while the ext source is missing, not ERROR")
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		By("(2b) state must NOT become ERROR at any point during the pending window")
		Consistently(func(g Gomega, ctx SpecContext) {
			got := getInstance(g, ctx)
			state, _, _ := unstructured.NestedString(got.Object, "status", "state")
			g.Expect(state).ToNot(Equal("ERROR"),
				"the empty-object status apply must not be rejected as Invalid")
		}, 5*time.Second, 500*time.Millisecond).WithContext(ctx).Should(Succeed())

		By("(2c) the status is TRIMMED: the sibling survives, the emptied object is gone")
		Eventually(func(g Gomega, ctx SpecContext) {
			got := getInstance(g, ctx)
			// The spec-sourced sibling still resolves and is present (proves
			// the status was written during the window, not frozen/removed).
			tag, _, _ := unstructured.NestedString(got.Object, "status", "tag")
			g.Expect(tag).To(Equal("v1"))
			// The fully-pending object must be ABSENT, not present-but-empty
			// and not frozen at its old value.
			_, found, _ := unstructured.NestedMap(got.Object, "status", "extInfo")
			g.Expect(found).To(BeFalse(),
				"extInfo must be trimmed away while its ext source is missing")
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

		By("(3) recreating the externalRef reconverges the full status")
		extCM = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "partial-status-ext", Namespace: namespace},
			Data:       map[string]string{"value": "from-external"},
		}
		Expect(env.Client.Create(ctx, extCM)).To(Succeed())
		Eventually(func(g Gomega, ctx SpecContext) {
			got := getInstance(g, ctx)
			src, found, _ := unstructured.NestedString(got.Object, "status", "extInfo", "source")
			g.Expect(found).To(BeTrue(), "extInfo should reappear once ext is restored")
			g.Expect(src).To(Equal("partial-status-ext"))
		}, 30*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())
	})
})
