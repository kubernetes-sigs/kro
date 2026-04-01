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

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Webhook Denial", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
				Labels: map[string]string{
					"test-webhook": "true",
				},
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("should report webhook denial in status and recover when policy is removed", func(ctx SpecContext) {
		policyName := fmt.Sprintf("deny-configmap-%s", rand.String(5))
		bindingName := fmt.Sprintf("deny-configmap-binding-%s", rand.String(5))

		policy := &admissionregistrationv1.ValidatingAdmissionPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: policyName},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicySpec{
				FailurePolicy: new(admissionregistrationv1.Fail),
				MatchConstraints: &admissionregistrationv1.MatchResources{
					ResourceRules: []admissionregistrationv1.NamedRuleWithOperations{{
						RuleWithOperations: admissionregistrationv1.RuleWithOperations{
							Operations: []admissionregistrationv1.OperationType{
								admissionregistrationv1.Create,
								admissionregistrationv1.Update,
							},
							Rule: admissionregistrationv1.Rule{
								APIGroups:   []string{""},
								APIVersions: []string{"v1"},
								Resources:   []string{"configmaps"},
							},
						},
					}},
				},
				Validations: []admissionregistrationv1.Validation{{
					Expression: "object.metadata.annotations['deny-me'] != 'true'",
					Message:    "ConfigMaps with annotation deny-me=true are not allowed by webhook policy",
				}},
			},
		}
		Expect(env.Client.Create(ctx, policy)).To(Succeed())
		defer func() { _ = env.Client.Delete(ctx, policy) }()

		binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{
			ObjectMeta: metav1.ObjectMeta{Name: bindingName},
			Spec: admissionregistrationv1.ValidatingAdmissionPolicyBindingSpec{
				PolicyName:        policyName,
				ValidationActions: []admissionregistrationv1.ValidationAction{admissionregistrationv1.Deny},
				MatchResources: &admissionregistrationv1.MatchResources{
					NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"test-webhook": "true"},
					},
				},
			},
		}
		Expect(env.Client.Create(ctx, binding)).To(Succeed())
		defer func() { _ = env.Client.Delete(ctx, binding) }()

		rgd := generator.NewResourceGraphDefinition("test-webhook-denial",
			generator.WithSchema("TestWebhookDenial", "v1alpha1",
				map[string]interface{}{"name": "string"}, nil),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name":        "${schema.spec.name}",
					"annotations": map[string]interface{}{"deny-me": "true"},
				},
				"data": map[string]interface{}{"key": "value"},
			}, nil, nil),
		)
		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			createdRGD := &krov1alpha1.ResourceGraphDefinition{}
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)).To(Succeed())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		instanceName := "test-webhook-instance"
		instance := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestWebhookDenial",
				"metadata":   map[string]interface{}{"name": instanceName, "namespace": namespace},
				"spec":       map[string]interface{}{"name": instanceName},
			},
		}
		Expect(env.Client.Create(ctx, instance)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)).To(Succeed())

			state, found, err := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(state).To(Equal("ERROR"))

			conditions, found, err := unstructured.NestedSlice(instance.Object, "status", "conditions")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())

			var resourcesReady map[string]interface{}
			for _, cond := range conditions {
				if c, ok := cond.(map[string]interface{}); ok && c["type"] == "ResourcesReady" {
					resourcesReady = c
					break
				}
			}
			g.Expect(resourcesReady).ToNot(BeNil())
			g.Expect(resourcesReady["status"]).To(Equal("False"))
			g.Expect(resourcesReady["message"]).To(ContainSubstring("not allowed by webhook policy"))
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, binding)).To(Succeed())
		Expect(env.Client.Delete(ctx, policy)).To(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)).To(Succeed())
			labels := instance.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			labels["trigger-reconcile"] = "true"
			instance.SetLabels(labels)
			g.Expect(env.Client.Update(ctx, instance)).To(Succeed())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega, ctx SpecContext) {
			g.Expect(env.Client.Get(ctx, types.NamespacedName{Name: instanceName, Namespace: namespace}, instance)).To(Succeed())
			state, _, _ := unstructured.NestedString(instance.Object, "status", "state")
			g.Expect(state).To(Equal("ACTIVE"))

			configMap := &corev1.ConfigMap{}
			configMapName := types.NamespacedName{Name: instanceName, Namespace: namespace}
			g.Expect(env.Client.Get(ctx, configMapName, configMap)).To(Succeed())
		}, 30*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
