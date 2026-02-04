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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	krov1alpha1 "github.com/kubernetes-sigs/kro/api/v1alpha1"
	"github.com/kubernetes-sigs/kro/pkg/controller/resourcegraphdefinition"
	"github.com/kubernetes-sigs/kro/pkg/testutil/generator"
)

var _ = Describe("Validation", func() {
	var (
		namespace string
	)

	BeforeEach(func(ctx SpecContext) {
		namespace = fmt.Sprintf("test-%s", rand.String(5))
		Expect(env.Client.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})).To(Succeed())
	})

	Context("Resource IDs", func() {
		It("should validate correct resource naming conventions", func(ctx SpecContext) {
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
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject invalid resource IDs", func(ctx SpecContext) {
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
				expectRGDInactiveWithError(ctx, rgd, "naming convention violation")
				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			}
		})

		It("should reject duplicate resource IDs", func(ctx SpecContext) {
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
			expectRGDInactiveWithError(ctx, rgd, "found duplicate resource IDs")
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})

	Context("Kubernetes Object Structure", func() {
		It("should validate correct kubernetes object structure", func(ctx SpecContext) {
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

			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject invalid kubernetes object structures", func(ctx SpecContext) {
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

				Eventually(func(g Gomega, ctx SpecContext) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name: rgd.Name,
					}, rgd)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))
				}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			}
		})
	})

	Context("RGD Status", func() {
		It("should reject RGDs with plain fileds (no expression)", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-k8s-valid",
				generator.WithSchema(
					"TestK8sValidation", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					// status field
					map[string]interface{}{
						"name": "string",
					},
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

			expectRGDInactiveWithError(ctx, rgd, "failed to build instance status schema: "+
				"status fields without expressions are not supported")

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})

	Context("Kind Names", func() {
		It("should validate correct kind names", func(ctx SpecContext) {
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

				Eventually(func(g Gomega, ctx SpecContext) {
					err := env.Client.Get(ctx, types.NamespacedName{
						Name: rgd.Name,
					}, rgd)
					g.Expect(err).ToNot(HaveOccurred())
					g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
				}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

				Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
			}
		})

		It("should reject invalid kind names", func(ctx SpecContext) {
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
		It("should not panic when deleting an inactive ResourceGraphDefinition", func(ctx SpecContext) {
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

			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))
				g.Expect(rgd.Status.TopologicalOrder).To(BeEmpty())
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})

	Context("ForEach Collections", func() {
		It("should accept valid forEach with single iterator from schema list", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-collection-valid",
				generator.WithSchema(
					"TestCollection", "v1alpha1",
					map[string]interface{}{
						"name":   "string",
						"labels": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("labeledConfigMaps", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${label}",
					},
					"data": map[string]interface{}{
						"label": "${label}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"label": "${schema.spec.labels}"},
					},
					nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())

			Eventually(func(g Gomega, ctx SpecContext) {
				err := env.Client.Get(ctx, types.NamespacedName{
					Name: rgd.Name,
				}, rgd)
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateActive))
			}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject forEach expression that does not return a list", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-collection-invalid-type",
				generator.WithSchema(
					"TestInvalidType", "v1alpha1",
					map[string]interface{}{
						"name": "string",
					},
					nil,
				),
				generator.WithResourceCollection("badConfigMaps", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${element}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"element": "${schema.spec.name}"}, // string, not a list
					},
					nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			expectRGDInactiveWithError(ctx, rgd, "must return a list")
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject forEach with reserved keyword iterator name", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-collection-reserved",
				generator.WithSchema(
					"TestReserved", "v1alpha1",
					map[string]interface{}{
						"name":  "string",
						"items": "[]string",
					},
					nil,
				),
				generator.WithResourceCollection("badIterator", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-test",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"item": "${schema.spec.items}"}, // "item" is reserved
					},
					nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			expectRGDInactiveWithError(ctx, rgd, "reserved keyword")
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject forEach iterator name that conflicts with resource ID", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-collection-conflict",
				generator.WithSchema(
					"TestConflict", "v1alpha1",
					map[string]interface{}{
						"name":  "string",
						"items": "[]string",
					},
					nil,
				),
				generator.WithResource("myConfig", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-base",
					},
				}, nil, nil),
				generator.WithResourceCollection("otherConfigs", map[string]interface{}{
					"apiVersion": "v1",
					"kind":       "ConfigMap",
					"metadata": map[string]interface{}{
						"name": "${schema.spec.name}-${myConfig}",
					},
				},
					[]krov1alpha1.ForEachDimension{
						{"myConfig": "${schema.spec.items}"}, // conflicts with resource ID
					},
					nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			expectRGDInactiveWithError(ctx, rgd, "conflicts with resource ID")
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

// expectRGDInactiveWithError waits for the RGD to become inactive and validates
// that the Ready condition contains the expected error message substring.
func expectRGDInactiveWithError(
	ctx SpecContext,
	rgd *krov1alpha1.ResourceGraphDefinition,
	expectedErrorSubstring string,
) {
	Eventually(func(g Gomega, ctx SpecContext) {
		err := env.Client.Get(ctx, types.NamespacedName{
			Name: rgd.Name,
		}, rgd)
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(rgd.Status.State).To(Equal(krov1alpha1.ResourceGraphDefinitionStateInactive))

		var condition *krov1alpha1.Condition
		for _, cond := range rgd.Status.Conditions {
			if cond.Type == resourcegraphdefinition.Ready {
				condition = &cond
				break
			}
		}
		g.Expect(condition).ToNot(BeNil())
		g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(*condition.Message).To(ContainSubstring(expectedErrorSubstring))
	}, 10*time.Second, time.Second).WithContext(ctx).Should(Succeed())
}
