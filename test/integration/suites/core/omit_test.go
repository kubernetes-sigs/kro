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
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Omit", func() {
	var namespace string

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		}
		Expect(env.Client.Create(ctx, ns)).To(Succeed())
	})

	AfterEach(func(ctx SpecContext) {
		Expect(env.Client.Delete(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	It("should omit a ConfigMap data field when omit() is triggered", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-omit",
			generator.WithSchema(
				"TestOmit", "v1alpha1",
				map[string]interface{}{
					"name":     "string",
					"optional": "string | default=\"\"",
				},
				nil,
			),
			generator.WithResource("configmap", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "ConfigMap",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"data": map[string]interface{}{
					"always":   "present",
					"optional": `${schema.spec.optional != "" ? schema.spec.optional : omit()}`,
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		// Wait for RGD to become active.
		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// --- Case 1: optional is empty → field should be omitted ---
		name1 := fmt.Sprintf("omit-yes-%s", rand.String(4))
		instance1 := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestOmit",
				"metadata": map[string]interface{}{
					"name":      name1,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":     name1,
					"optional": "",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance1)).To(Succeed())

		// Wait for instance to become ACTIVE.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, instance1)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance1.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify ConfigMap: "always" present, "optional" absent.
		cm1 := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, cm1)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm1.Data).To(HaveKey("always"))
			g.Expect(cm1.Data).ToNot(HaveKey("optional"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// --- Case 1b: update instance1 to set optional → field should appear ---
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, instance1)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(unstructured.SetNestedField(instance1.Object, "now-present", "spec", "optional")).To(Succeed())
		Expect(env.Client.Update(ctx, instance1)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, cm1)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm1.Data).To(HaveKeyWithValue("always", "present"))
			g.Expect(cm1.Data).To(HaveKeyWithValue("optional", "now-present"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// --- Case 1c: update instance1 back to empty → field should be omitted again ---
		// This is the interesting SSA edge case: the field was previously managed
		// by kro (set to "now-present"). When omit() fires, kro drops the field
		// from the apply payload, so SSA should relinquish ownership and the
		// field should disappear from the ConfigMap.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, instance1)
			g.Expect(err).ToNot(HaveOccurred())
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(unstructured.SetNestedField(instance1.Object, "", "spec", "optional")).To(Succeed())
		Expect(env.Client.Update(ctx, instance1)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, cm1)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm1.Data).To(HaveKeyWithValue("always", "present"))
			g.Expect(cm1.Data).ToNot(HaveKey("optional"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// --- Case 2: optional is set → field should be present ---
		name2 := fmt.Sprintf("omit-no-%s", rand.String(4))
		instance2 := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestOmit",
				"metadata": map[string]interface{}{
					"name":      name2,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":     name2,
					"optional": "my-value",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance2)).To(Succeed())

		// Wait for instance to become ACTIVE.
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name2, Namespace: namespace}, instance2)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance2.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Verify ConfigMap: both "always" and "optional" present.
		cm2 := &corev1.ConfigMap{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name2, Namespace: namespace}, cm2)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(cm2.Data).To(HaveKeyWithValue("always", "present"))
			g.Expect(cm2.Data).To(HaveKeyWithValue("optional", "my-value"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance1)).To(Succeed())
		Expect(env.Client.Delete(ctx, instance2)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, instance1)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance1 should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name2, Namespace: namespace}, instance2)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance2 should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})

	It("should omit array elements when omit() is triggered", func(ctx SpecContext) {
		rgd := generator.NewResourceGraphDefinition("test-omit-array",
			generator.WithSchema(
				"TestOmitArray", "v1alpha1",
				map[string]interface{}{
					"name":        "string",
					"optionalArg": "string | default=\"\"",
				},
				nil,
			),
			generator.WithResource("pod", map[string]interface{}{
				"apiVersion": "v1",
				"kind":       "Pod",
				"metadata": map[string]interface{}{
					"name": "${schema.spec.name}",
				},
				"spec": map[string]interface{}{
					"containers": []interface{}{
						map[string]interface{}{
							"name":  "main",
							"image": "busybox",
							"command": []interface{}{
								"echo",
								`${schema.spec.optionalArg != "" ? schema.spec.optionalArg : omit()}`,
							},
						},
					},
				},
			}, nil, nil),
		)

		Expect(env.Client.Create(ctx, rgd)).To(Succeed())

		createdRGD := &krov1alpha1.ResourceGraphDefinition{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, createdRGD)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(createdRGD.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
		}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// --- Case 1: optionalArg is empty → array element should be omitted ---
		name1 := fmt.Sprintf("omit-arr-yes-%s", rand.String(4))
		instance1 := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestOmitArray",
				"metadata": map[string]interface{}{
					"name":      name1,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":        name1,
					"optionalArg": "",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance1)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, instance1)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance1.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		pod1 := &corev1.Pod{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, pod1)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(pod1.Spec.Containers).To(HaveLen(1))
			g.Expect(pod1.Spec.Containers[0].Command).To(Equal([]string{"echo"}))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// --- Case 2: optionalArg is set → array element should be present ---
		name2 := fmt.Sprintf("omit-arr-no-%s", rand.String(4))
		instance2 := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": fmt.Sprintf("%s/%s", krov1alpha1.KRODomainName, "v1alpha1"),
				"kind":       "TestOmitArray",
				"metadata": map[string]interface{}{
					"name":      name2,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"name":        name2,
					"optionalArg": "hello",
				},
			},
		}
		Expect(env.Client.Create(ctx, instance2)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name2, Namespace: namespace}, instance2)
			g.Expect(err).ToNot(HaveOccurred())
			val, found, err := unstructured.NestedString(instance2.Object, "status", "state")
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(val).To(Equal("ACTIVE"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		pod2 := &corev1.Pod{}
		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name2, Namespace: namespace}, pod2)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(pod2.Spec.Containers).To(HaveLen(1))
			g.Expect(pod2.Spec.Containers[0].Command).To(Equal([]string{"echo", "hello"}))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		// Cleanup
		Expect(env.Client.Delete(ctx, instance1)).To(Succeed())
		Expect(env.Client.Delete(ctx, instance2)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name1, Namespace: namespace}, instance1)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance1 should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: name2, Namespace: namespace}, instance2)
			g.Expect(err).To(MatchError(errors.IsNotFound, "instance2 should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())

		Expect(env.Client.Delete(ctx, rgd)).To(Succeed())

		Eventually(func(g Gomega) {
			err := env.Client.Get(ctx, types.NamespacedName{Name: rgd.Name}, &krov1alpha1.ResourceGraphDefinition{})
			g.Expect(err).To(MatchError(errors.IsNotFound, "rgd should be deleted"))
		}, 20*time.Second, time.Second).WithContext(ctx).Should(Succeed())
	})
})
