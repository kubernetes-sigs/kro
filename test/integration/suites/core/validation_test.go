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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kro-run/kro/api/v1alpha1"
	"github.com/kro-run/kro/pkg/testutil/generator"
)

var _ = Describe("Validation", func() {
	var (
		ctx       context.Context
		namespace string
	)

	BeforeEach(func() {
		ctx = context.Background()
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	Context("Resource IDs", func() {
		It("should validate correct resource naming conventions", func() {
			rgd := generator.NewResourceGraphDefinition("test-validation",
				generator.WithSchema(
					"TestValidation", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				// Valid lower camelCase names
				generator.WithResource("myResource", validResourceDef(), nil, nil),
				generator.WithResource("anotherResource", validResourceDef(), nil, nil),
				generator.WithResource("testResource", validResourceDef(), nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).Should(Succeed())
		})

		It("should reject invalid resource IDs", func() {
			invalidNames := []string{
				"MyResource",  // Uppercase first letter
				"my_resource", // Contains underscore
				"my-resource", // Contains hyphen
				"123resource", // Starts with number
				"my.resource", // Contains dot
				"resource!",   // Special character
				"spec",        // Reserved word
				"metadata",    // Reserved word
				"status",      // Reserved word
				"instance",    // Reserved word
			}

			for _, invalidName := range invalidNames {
				rgd := generator.NewResourceGraphDefinition(fmt.Sprintf("test-validation-%s", rand.String(5)),
					generator.WithSchema(
						"TestValidation", "v1alpha1",
						map[string]interface{}{
							"name": "string",
						},
						nil,
					),
					generator.WithResource(invalidName, validResourceDef(), nil, nil),
				)

				Expect(env.Client.Create(ctx, rgd)).To(Succeed())

				Eventually(func(g Gomega) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name: rgd.Name,
					}, rgd)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))

					// Verify validation condition
					var condition *krov1alpha1.Condition
					for _, cond := range rgd.Status.Conditions {
						if cond.Type == krov1alpha1.ResourceGraphDefinitionConditionTypeGraphVerified {
							condition = &cond
							break
						}
					}
					g.Expect(condition).ToNot(BeNil())
					g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
					g.Expect(*condition.Reason).To(ContainSubstring("naming convention violation"))
				}, 10*time.Second, time.Second).Should(Succeed())
			}
		})

		It("should reject duplicate resource IDs", func() {
			rgd := generator.NewResourceGraphDefinition("test-validation-dup",
				generator.WithSchema(
					"TestValidation", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("myResource", validResourceDef(), nil, nil),
				generator.WithResource("myResource", validResourceDef(), nil, nil), // Duplicate
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))

				// Verify validation condition
				var condition *krov1alpha1.Condition
				for _, cond := range rgd.Status.Conditions {
					if cond.Type == krov1alpha1.ResourceGraphDefinitionConditionTypeGraphVerified {
						condition = &cond
						break
					}
				}
				g.Expect(condition).ToNot(BeNil())
				g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(*condition.Reason).To(ContainSubstring("found duplicate resource IDs"))
			}, 10*time.Second, time.Second).Should(Succeed())
		})
	})

	Context("Kubernetes Object Structure", func() {
		It("should validate correct kubernetes object structure", func() {
			rgd := generator.NewResourceGraphDefinition("test-k8s-valid",
				generator.WithSchema(
					"TestK8sValidation", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("validResource", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "test-config",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).Should(Succeed())
		})

		It("should reject invalid kubernetes object structures", func() {
			invalidObjects := []map[string]interface{}{
				{
					// Missing apiVersion
					"kind":     "ConfigMap",
					"metadata": map[string]interface{}{},
				},
				{
					// Missing kind
					"apiVersion": "v1",
					"metadata":   map[string]interface{}{},
				},
				{
					// Missing metadata
					"apiVersion": "v1",
					"kind":       "ConfigMap",
				},
				{
					// Invalid apiVersion format
					"apiVersion": "invalid/version/format",
					"kind":       "ConfigMap",
					"metadata":   map[string]interface{}{},
				},
				{
					// Invalid version
					"apiVersion": "v999xyz1",
					"kind":       "ConfigMap",
					"metadata":   map[string]interface{}{},
				},
			}

			for i, invalidObj := range invalidObjects {
				rgd := generator.NewResourceGraphDefinition(fmt.Sprintf("test-k8s-invalid-%d", i),
					generator.WithSchema(
						"TestK8sValidation", "v1alpha1",
						map[string]interface{}{
							"name": "string",
						},
						nil,
					),
					generator.WithResource("resource", invalidObj, nil, nil),
				)

				Expect(env.Client.Create(ctx, rgd)).To(Succeed())

				Eventually(func(g Gomega) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name: rgd.Name,
					}, rgd)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))
				}, 10*time.Second, time.Second).Should(Succeed())
			}
		})
	})

	Context("Kind Names", func() {
		It("should validate correct kind names", func() {
			validKinds := []string{
				"TestResource",
				"AnotherTest",
				"MyKindName",
				"Resource123",
			}

			for _, kind := range validKinds {
				rgd := generator.NewResourceGraphDefinition(fmt.Sprintf("test-kind-%s", rand.String(5)),
					generator.WithSchema(
						kind, "v1alpha1",
						map[string]interface{}{
							"name": "string",
						},
						nil,
					),
				)

				Expect(env.Client.Create(ctx, rgd)).To(Succeed())

				Eventually(func(g Gomega) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name: rgd.Name,
					}, rgd)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				}, 10*time.Second, time.Second).Should(Succeed())
			}
		})

		It("should reject invalid kind names", func() {
			invalidKinds := []string{
				"testResource",  // Lowercase first letter
				"Test_Resource", // Contains underscore
				"Test-Resource", // Contains hyphen
				"123Test",       // Starts with number
				"Test.Resource", // Contains dot
				"Test!",         // Special character
				"TestThisIsAValidButReallyLongNameSoLongThatItIsGreaterThan63Characters", // Greater than 63 characters
			}

			for _, kind := range invalidKinds {
				rgd := generator.NewResourceGraphDefinition(fmt.Sprintf("test-kind-%s", rand.String(5)),
					generator.WithSchema(
						kind, "v1alpha1",
						map[string]interface{}{
							"name": "string",
						},
						nil,
					),
				)

				Expect(env.Client.Create(ctx, rgd)).ToNot(Succeed())
			}
		})
	})

	Context("Proper Cleanup", func() {
		It("should not panic when deleting an inactive ResourceGraphDefinition", func() {
			rgd := generator.NewResourceGraphDefinition("test-cleanup",
				generator.WithSchema(
					"TestCleanup", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("testResource", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ServiceAccount",
					"metadata": map[string]interface{}{
						"name": "${Bad expression}",
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))
				g.Expect(rgd.Status.TopologicalOrder).To(BeEmpty())
			}, 10*time.Second, time.Second).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})
})

func validResourceDef() map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]interface{}{
			"name": "test-config",
		},
	}
}
