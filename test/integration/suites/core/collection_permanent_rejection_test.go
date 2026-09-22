// Copyright 2026 The Kube Resource Orchestrator Authors
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

// A permanent update rejection on an existing collection member (an immutable
// field the apiserver refuses to change) must NOT be silently tolerated as
// ACTIVE. The member keeps its live value, but the instance surfaces the
// failure (ERROR + ResourcesReady=False) so the dropped update is visible to
// the operator, matching the pre-graph RGD contract. Dependents of the
// collection do not converge. This is the collection-specific counterpart to
// the transient-rejection spec, which recovers; a permanent rejection stays
// visible until the operator withdraws the impossible change.
var _ = Describe("Collection permanent update rejection", func() {
	It("surfaces the failure as ERROR instead of reporting ACTIVE", func(ctx SpecContext) {
		namespace := "collection-perm-reject-" + rand.String(5)
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: map[string]string{"kubernetes.io/metadata.name": namespace},
		}}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(env.Client.Delete(ctx, ns)).To(Succeed())
		})

		rgd := generator.NewResourceGraphDefinition("test-perm-rejection",
			generator.WithSchema("PermRejectionCollection", "v1alpha1",
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
			"apiVersion": "kro.run/v1alpha1", "kind": "PermRejectionCollection",
			"metadata": map[string]any{"name": "coll", "namespace": namespace},
			"spec":     map[string]any{"slots": []any{"a", "b"}, "value": "v1"},
		}}
		createInstanceWithCleanup(ctx, instance)
		waitForInstanceActive(ctx, namespace, instance.GetName(), instance)
		instanceKey := client.ObjectKeyFromObject(instance)
		memberUIDs := map[string]types.UID{}
		for _, name := range []string{"coll-a", "coll-b"} {
			cm := &corev1.ConfigMap{}
			Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm)).To(Succeed())
			Expect(cm.Data["value"]).To(Equal("v1"))
			memberUIDs[name] = cm.UID
		}

		By("making data.value immutable for slot=a via a ValidatingAdmissionPolicy")
		policyName := "immutable-slot-a-" + rand.String(5)
		bindingName := "immutable-slot-a-binding-" + rand.String(5)
		policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
				FailurePolicy: new(admissionregistrationv1.Fail),
				MatchConstraints: &admissionregistrationv1.MatchResources{
					ObjectSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"slot": "a"}},
					ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{admissionregistrationv1.Update},
							Rule: admissionregistrationv1.Rule{
								APIGroups: []string{""}, APIVersions: []string{"v1"}, Resources: []string{"configmaps"},
							},
						},
					}},
				},
				Validations: []admissionregistrationv1.Validation{{
					// Reject any UPDATE that changes data.value — a synthetic
					// immutable field that can never accept the change.
					Expression: "object.data['value'] == oldObject.data['value']",
					Message:    "data.value is immutable",
				}},
			},
		}
		Expect(env.Client.Create(ctx, policy)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, policy))).To(Succeed())
		})
		binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        policyName,
				ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
				MatchResources: &admissionregistrationv1.MatchResources{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": namespace}},
				},
			},
		}
		Expect(env.Client.Create(ctx, binding)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, binding))).To(Succeed())
		})

		// Wait for policy propagation using an unmanaged probe, so we do not
		// enqueue a managed sibling's instance.
		probe := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "policy-probe", Namespace: namespace, Labels: map[string]string{"slot": "a"}},
			Data:       map[string]string{"value": "p1"},
		}
		Expect(env.Client.Create(ctx, probe)).To(Succeed())
		DeferCleanup(func(ctx SpecContext) {
			Expect(client.IgnoreNotFound(env.Client.Delete(ctx, probe))).To(Succeed())
		})
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(probe), probe)).To(Succeed())
			probe.Data["value"] = rand.String(5)
			err := env.Client.Update(ctx, probe)
			g.Expect(err).To(HaveOccurred())
			g.Expect(err.Error()).To(ContainSubstring("data.value is immutable"))
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())

		By("requesting v2 for both members — slot=a's update is permanently rejected")
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			g.Expect(unstructured.SetNestedField(instance.Object, "v2", "spec", "value")).To(Succeed())
			g.Expect(env.Client.Update(ctx, instance)).To(Succeed())
		}).WithContext(ctx).WithTimeout(10 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
		generation := instance.GetGeneration()

		By("reporting ERROR + ResourcesReady=False (not ACTIVE), keeping the live slot=a value")
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			cond := findInstanceConditionByType(instance, "ResourcesReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond["observedGeneration"]).To(Equal(generation))
			g.Expect(cond["status"]).To(Equal("False"),
				"a silently-dropped update must surface as ResourcesReady=False, not ACTIVE")
			g.Expect(cond["message"]).To(ContainSubstring("item " + namespace + "/coll-a"))
			g.Expect(cond["message"]).To(ContainSubstring("update rejected"))

			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).To(Equal("ERROR"), "ResourcesReady: %v", cond)

			// The rejected member keeps its live (old) value; the healthy
			// sibling took the update.
			for name, value := range map[string]string{"coll-a": "v1", "coll-b": "v2"} {
				cm := &corev1.ConfigMap{}
				g.Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm)).To(Succeed())
				g.Expect(cm.Data["value"]).To(Equal(value))
				g.Expect(cm.UID).To(Equal(memberUIDs[name]))
			}
		}).WithContext(ctx).WithTimeout(30 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

		By("never flipping to ACTIVE while the update stays rejected")
		Consistently(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).NotTo(Equal("ACTIVE"), "a silently-dropped update must never report ACTIVE")
			cm := &corev1.ConfigMap{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: "coll-a"}, cm)).To(Succeed())
			g.Expect(cm.Data["value"]).To(Equal("v1"), "the rejected update must not have landed")
		}).WithContext(ctx).WithTimeout(10 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())

		By("recovering to ACTIVE once the impossible change is withdrawn")
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			g.Expect(unstructured.SetNestedField(instance.Object, "v1", "spec", "value")).To(Succeed())
			g.Expect(env.Client.Update(ctx, instance)).To(Succeed())
		}).WithContext(ctx).WithTimeout(10 * time.Second).WithPolling(250 * time.Millisecond).Should(Succeed())
		Eventually(func(g Gomega) {
			g.Expect(env.Client.Get(ctx, instanceKey, instance)).To(Succeed())
			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).To(Equal("ACTIVE"))
			cond := findInstanceConditionByType(instance, "ResourcesReady")
			g.Expect(cond).NotTo(BeNil())
			g.Expect(cond["status"]).To(Equal("True"))
		}).WithContext(ctx).WithTimeout(60 * time.Second).WithPolling(500 * time.Millisecond).Should(Succeed())
	})
})
