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
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Collection transient update rejection", func() {
	It("retries a rejected member without an instance edit and preserves its identity", func(ctx SpecContext) {
		namespace := "collection-retry-" + rand.String(5)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgd := generator.NewResourceGraphDefinition("test-transient-rejection",
			generator.WithSchema("TransientRejectionCollection", "v1alpha1",
				map[string]any{"slots": "[]string", "value": "string"}, nil),
			generator.WithResourceCollection("configmaps", map[string]any{
				"apiVersion": "v1", "kind": "ConfigMap",
				"metadata": map[string]any{
					"name":   "${schema.metadata.name}-${slot}",
					"labels": map[string]any{"slot": "${slot}"},
				},
				"data": map[string]any{"value": "${schema.spec.value}"},
			}, []krov1alpha1.ForEachDimension{{"slot": "${schema.spec.slots}"}}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
		waitForRGDActive(ctx, rgd.Name)

		instance := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kro.run/v1alpha1", "kind": "TransientRejectionCollection",
			"metadata": map[string]any{"name": "coll", "namespace": namespace},
			"spec":     map[string]any{"slots": []any{"a", "b"}, "value": "v1"},
		}}
		createInstanceWithCleanup(ctx, instance)
		waitForInstanceActive(ctx, namespace, instance.GetName(), instance)
		instanceKey := client.ObjectKeyFromObject(instance)
		instanceUID := instance.GetUID()
		initialGeneration := instance.GetGeneration()
		memberUIDs := map[string]types.UID{}
		for _, name := range []string{"coll-a", "coll-b"} {
			cm := &corev1.ConfigMap{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm)).To(Succeed())
			Expect(cm.Data["value"]).To(Equal("v1"))
			Expect(cm.UID).NotTo(BeEmpty())
			memberUIDs[name] = cm.UID
		}

		By("rejecting only slot=a ConfigMap updates through an unreachable admission webhook")
		webhook := &admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: "block-slot-a-" + namespace},
			Webhooks: []admissionregistrationv1.ValidatingWebhook{{
				Name:                    "block-slot-a.kro-test.invalid",
				AdmissionReviewVersions: []string{"v1"},
				SideEffects:             new(admissionregistrationv1.SideEffectClassNone),
				FailurePolicy:           new(admissionregistrationv1.Fail),
				TimeoutSeconds:          new(int32(1)),
				Rules: []admissionregistrationv1.RuleWithOperations{{
					Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
					Rule: admissionregistrationv1.Rule{
						APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
					},
				}},
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
				ObjectSelector:    &metav1.LabelSelector{MatchLabels: map[string]string{"slot": "a"}},
				ClientConfig: admissionregistrationv1.WebhookClientConfig{
					URL: new("https://127.0.0.1:1/validate"),
				},
			}},
		}
		Expect(env.Client.Create(ctx, webhook)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, webhook))).To(Succeed())
		})

		// Wait for admission configuration propagation using an unmanaged object;
		// probing a managed sibling would also enqueue its instance.
		probe := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "webhook-probe", Namespace: namespace, Labels: map[string]string{"slot": "a"}},
			Data:       map[string]string{"value": "v1"},
		}
		Expect(env.Client.Create(ctx, probe)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, probe)).To(Succeed())
		})
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(probe), probe)).To(Succeed())
			probe.Data["value"] = rand.String(5)
			err := env.Client.Update(ctx, probe)
			g.Expect(apierrors.IsInternalError(err)).To(BeTrue(), "expected webhook InternalError, got %v", err)
			g.Expect(err).To(MatchError(ContainSubstring("failed calling webhook")))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("requesting v2 for both existing members")
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			g.Expect(unstructured.SetNestedField(instance.Object, "v2", "spec", "value")).To(Succeed())
			g.Expect(env.Client.Update(ctx, instance)).To(Succeed())
		}).WithContext(ctx).WithTimeout(10 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
		generation := instance.GetGeneration()
		Expect(generation).To(BeNumerically(">", initialGeneration))

		By("reporting current-generation not-ready while retaining the stale member and updating its sibling")
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			cond := findInstanceConditionByType(instance, "ResourcesReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond["observedGeneration"]).To(Equal(generation))
			for name, value := range map[string]string{"coll-a": "v1", "coll-b": "v2"} {
				cm := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm)).To(Succeed())
				g.Expect(cm.Data["value"]).To(Equal(value))
				g.Expect(cm.UID).To(Equal(memberUIDs[name]))
			}
			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).To(Equal("IN_PROGRESS"), "ResourcesReady: %v", cond)
			g.Expect(cond["status"]).To(Equal("False"))
			g.Expect(cond["reason"]).To(Equal("NotReady"))
			g.Expect(cond["message"]).To(ContainSubstring("item " + namespace + "/coll-a"))
			g.Expect(cond["message"]).To(ContainSubstring("failed calling webhook"))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("removing only the webhook and letting the existing retry recover the unchanged instance")
		Expect(env.Client.Delete(ctx, webhook)).To(Succeed())
		// Allow a full capped backoff interval (five minutes) plus propagation.
		Eventually(func(g Gomega) {
			for name, uid := range memberUIDs {
				cm := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm)).To(Succeed())
				g.Expect(cm.Data["value"]).To(Equal("v2"))
				g.Expect(cm.UID).To(Equal(uid))
			}
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			g.Expect(instance.GetUID()).To(Equal(instanceUID))
			g.Expect(instance.GetGeneration()).To(Equal(generation))
			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).To(Equal("ACTIVE"))
			cond := findInstanceConditionByType(instance, "ResourcesReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond["observedGeneration"]).To(Equal(generation))
			g.Expect(cond["status"]).To(Equal("True"))
			g.Expect(cond["reason"]).To(Equal("AllResourcesReady"))
		}).WithContext(ctx).WithTimeout(6 * time.Minute).WithPolling(500 * time.Millisecond).Should(Succeed())
	})
})
