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
	"strings"
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

	Context("Schema short names and categories", func() {
		It("should reject invalid values at admission", func(ctx SpecContext) {
			tests := []struct {
				name   string
				mutate func(*krov1alpha1.Schema)
			}{
				{
					name: "uppercase short name",
					mutate: func(schema *krov1alpha1.Schema) {
						schema.ShortNames = []string{"WA"}
					},
				},
				{
					name: "duplicate short name",
					mutate: func(schema *krov1alpha1.Schema) {
						schema.ShortNames = []string{"wa", "wa"}
					},
				},
				{
					name: "too long short name",
					mutate: func(schema *krov1alpha1.Schema) {
						schema.ShortNames = []string{strings.Repeat("a", 64)}
					},
				},
				{
					name: "uppercase category",
					mutate: func(schema *krov1alpha1.Schema) {
						schema.Categories = []string{"Kro"}
					},
				},
				{
					name: "duplicate category",
					mutate: func(schema *krov1alpha1.Schema) {
						schema.Categories = []string{"platform", "platform"}
					},
				},
			}

			for _, tt := range tests {
				By(tt.name)
				rgd := generator.NewResourceGraphDefinition(fmt.Sprintf("test-alias-validation-%s", rand.String(5)),
					generator.WithSchema(
						"AliasValidation", "v1alpha1",
						map[string]any{
							"name": "string",
						},
						nil,
					),
				)
				tt.mutate(rgd.Spec.Schema)

				Expect(env.Client.Create(ctx, rgd)).ToNot(Succeed())
			}
		})
	})

	Context("Resource IDs", func() {

	})

	Context("Kubernetes Object Structure", func() {

	})

	Context("RGD Status", func() {
	})

	Context("Kind Names", func() {

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
						map[string]any{
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
					map[string]any{
						"name": "string",
					},
					nil,
				),
				generator.WithResource("testResource", map[string]any{
					"apiVersion": "v1",
					"kind":       "ServiceAccount",
					"metadata": map[string]any{
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
			}, 10*time.Second, 250*time.Millisecond).WithContext(ctx).Should(Succeed())

			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject an external collection with a mis-shaped selector object", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-invalid-external-selector",
				generator.WithSchema(
					"TestInvalidExternalSelector", "v1alpha1",
					map[string]interface{}{},
					nil,
				),
				generator.WithExternalRef("extconfigs", &krov1alpha1.ExternalRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Metadata: krov1alpha1.ExternalRefMetadata{
						Selector: toRawExtension(map[string]interface{}{
							"matchLabels": "team=platform",
						}),
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			expectRGDInactiveWithError(ctx, rgd, "expected object type for path metadata.selector.matchLabels")
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject an external collection with an unknown selector field", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-unknown-external-selector-field",
				generator.WithSchema(
					"TestUnknownExternalSelectorField", "v1alpha1",
					map[string]interface{}{},
					nil,
				),
				generator.WithExternalRef("extconfigs", &krov1alpha1.ExternalRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Metadata: krov1alpha1.ExternalRefMetadata{
						Selector: toRawExtension(map[string]interface{}{
							"matchLabels": map[string]interface{}{"team": "platform"},
							"bogusField":  "value",
						}),
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			expectRGDInactiveWithError(ctx, rgd, "schema not found for field bogusField")
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})

		It("should reject an external collection with an invalid literal selector operator", func(ctx SpecContext) {
			rgd := generator.NewResourceGraphDefinition("test-invalid-external-selector-operator",
				generator.WithSchema(
					"TestInvalidExternalSelectorOperator", "v1alpha1",
					map[string]interface{}{},
					nil,
				),
				generator.WithExternalRef("extconfigs", &krov1alpha1.ExternalRef{
					APIVersion: "v1",
					Kind:       "ConfigMap",
					Metadata: krov1alpha1.ExternalRefMetadata{
						Selector: toRawExtension(map[string]interface{}{
							"matchExpressions": []map[string]interface{}{{
								"key":      "team",
								"operator": "NotARealOperator",
							}},
						}),
					},
				}, nil, nil),
			)

			Expect(env.Client.Create(ctx, rgd)).To(Succeed())
			expectRGDInactiveWithError(ctx, rgd, "invalid label selector")
			Expect(env.Client.Delete(ctx, rgd)).To(Succeed())
		})
	})
})

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
